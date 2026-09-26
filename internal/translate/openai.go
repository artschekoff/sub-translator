package translate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// defaultOpenAIURL is the chat completions endpoint. The whole URL is
// overridable rather than just a host, so a test server needs no route of a
// particular shape — that is the seam Options.BaseURL exists for.
const defaultOpenAIURL = "https://api.openai.com/v1/chat/completions"

// openAIClient translates through an OpenAI chat model. The reason to pay for a
// model at all is context: the previous lines go with every batch, so who is
// addressing whom, names and formality survive across a subtitle file in a way
// that a per-line phrase translator cannot manage.
type openAIClient struct {
	from, to string
	model    string
	apiKey   string
	baseURL  string
	workers  int
	http     *http.Client
}

func newOpenAI(o Options) *openAIClient {
	c := &openAIClient{
		from:    o.From,
		to:      o.To,
		model:   strings.TrimSpace(o.Model),
		apiKey:  strings.TrimSpace(o.APIKey),
		baseURL: o.BaseURL,
		workers: o.Workers,
		// Generous on purpose: forty lines through a chat model is a slow round
		// trip, and a timeout here fails a whole batch.
		http: &http.Client{Timeout: 120 * time.Second},
	}
	if c.model == "" {
		c.model = DefaultModel
	}
	if c.baseURL == "" {
		c.baseURL = defaultOpenAIURL
	}
	if c.workers < 1 {
		c.workers = DefaultWorkers
	}
	return c
}

func (c *openAIClient) TranslateAll(texts []string, progress func(done, total int)) ([]string, []int, error) {
	return translateAll(c, texts, c.workers, progress)
}

// chatRequest is the request body. No response_format is sent: JSON mode could
// not be verified against the current models, and a request rejected for an
// unsupported field would fail every batch. The prompt asks for JSON and
// decodeObject parses defensively instead.
type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// batchPayload is what the model is asked to work on. The field names carry the
// instruction: whatever else a model ignores, it is being handed an object whose
// context field says in its own name that it is not to be translated.
type batchPayload struct {
	ContextDoNotTranslate []string          `json:"context_previous_lines_do_not_translate,omitempty"`
	Translate             map[string]string `json:"translate"`
}

// malformedAttempts is how many times a batch is sent before the pool is allowed
// to fall back to one request per block.
//
// One retry, not none: a model's reply is sampled, so the same prompt can come
// back well-formed on a second look. And the arithmetic favours it — fanning out
// is 40 requests, each re-sending the system prompt and the context lines,
// against one request to ask the batch again. The cheap recovery here is the
// retry; the fan-out is the expensive last resort.
const malformedAttempts = 2

// batch translates one batch in a single request. The blocks go out as a JSON
// object keyed by index and must come back as an object with exactly those keys.
// Anything else — a missing key, an extra key, a renumbered set, a body that will
// not parse — is retried once and then fails the batch, which falls through to
// the pool's per-block path.
func (c *openAIClient) batch(texts, before []string) ([]string, error) {
	keys := make([]string, len(texts))
	src := make(map[string]string, len(texts))
	for i, t := range texts {
		k := strconv.Itoa(i)
		keys[i] = k
		src[k] = t
	}

	body, err := json.Marshal(chatRequest{
		Model: c.model,
		Messages: []chatMessage{
			{Role: "system", Content: c.systemPrompt()},
			{Role: "user", Content: mustJSON(batchPayload{
				ContextDoNotTranslate: before,
				Translate:             src,
			})},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	var lastErr error
	for attempt := 1; attempt <= malformedAttempts; attempt++ {
		content, err := retry(func() (string, error) { return c.complete(body) })
		if err != nil {
			// A status failure is the endpoint's, not the model's, and retry has
			// already exhausted it. Asking again would only widen the hole the pool
			// is careful not to dig.
			return nil, err
		}
		out, err := mapReply(content, keys)
		if err == nil {
			return out, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// mapReply turns the model's message into one translation per requested key. The
// key set must match exactly: a renumbered or shifted set is the same size as the
// one asked for and would silently put every line one block out of place, which
// is worse than an untranslated file because it still looks finished.
func mapReply(content string, keys []string) ([]string, error) {
	got, err := decodeObject(content)
	if err != nil {
		return nil, err
	}
	if len(got) != len(keys) {
		return nil, fmt.Errorf("the model returned %d translations for %d blocks",
			len(got), len(keys))
	}
	out := make([]string, len(keys))
	for i, k := range keys {
		v, ok := got[k]
		if !ok {
			return nil, fmt.Errorf("the model returned no translation for block %q", k)
		}
		out[i] = strings.TrimSpace(v)
	}
	return out, nil
}

// one translates a single block, reusing the batch shape with one key. before is
// the context the whole batch was given, which is still the right context for any
// block inside it.
func (c *openAIClient) one(text string, before []string) (string, error) {
	out, err := c.batch([]string{text}, before)
	if err != nil {
		return "", err
	}
	return out[0], nil
}

func (c *openAIClient) systemPrompt() string {
	from := strings.TrimSpace(c.from)
	if from == "" {
		from = "the source language"
	}
	return fmt.Sprintf(`You are a professional subtitle translator. `+
		`Translate every value of the "translate" object from %s into %s.

Rules:
- "context_previous_lines_do_not_translate" holds the source lines that come immediately before this batch. Use them to keep names, forms of address, pronouns and formality consistent. Never translate them and never include them in your reply.
- Reply with a JSON object and nothing else: no prose, no code fence. Its keys must be exactly the keys of "translate", each mapped to the translated line as a string.
- Translate every line, even a single word or an interjection. Never merge, split, reorder, renumber, drop or explain a line.
- Keep the register of the original, and keep any markup and line breaks the line already has.`,
		from, c.to)
}

// complete posts one chat completion and returns the assistant's message.
func (c *openAIClient) complete(body []byte) (string, error) {
	req, err := http.NewRequest(http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", statusError{code: resp.StatusCode, retryAfter: parseRetryAfter(resp.Header)}
	}

	var out struct {
		Choices []struct {
			Message chatMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("parse response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("the model returned no choices")
	}
	return out.Choices[0].Message.Content, nil
}

// decodeObject finds the JSON object in a model's reply. Nothing here relies on
// a JSON-mode flag, so the object can arrive fenced or with a sentence in front
// of it; the outermost braces are what matter. Anything else is an error, and the
// batch falls back to per-block translation.
func decodeObject(content string) (map[string]string, error) {
	s := strings.TrimSpace(content)
	if s == "" {
		return nil, errors.New("the model returned an empty message")
	}
	start := strings.Index(s, "{")
	if start < 0 {
		return nil, fmt.Errorf("no JSON object in the model's reply: %s", excerpt(s))
	}
	// Decode rather than Unmarshal, and no search for a closing brace: the decoder
	// stops at the end of the first complete value, so trailing prose — including
	// prose with a {brace} in it — does not turn a usable reply into a 40-request
	// fan-out.
	var got map[string]string
	if err := json.NewDecoder(strings.NewReader(s[start:])).Decode(&got); err != nil {
		return nil, fmt.Errorf("parse the model's reply (%s): %w", excerpt(s), err)
	}
	return got, nil
}

// excerpt trims a model's reply down to something that belongs in an error
// message rather than a whole batch of dialogue.
func excerpt(s string) string {
	const limit = 80
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= limit {
		return strconv.Quote(string(r))
	}
	return strconv.Quote(string(r[:limit])) + "…"
}

// mustJSON encodes a value that cannot fail to encode: batchPayload is strings
// all the way down.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// Unreachable for batchPayload; a panic here would be a programming
		// error, not a runtime condition.
		panic(err)
	}
	return string(b)
}
