package translate

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// capturedRequest is one request the fake endpoint saw, decoded far enough to
// assert on what was asked for.
type capturedRequest struct {
	Auth    string
	Model   string        `json:"model"`
	Message []chatMessage `json:"messages"`
}

// payload returns the user message's decoded body: the batch and its context.
func (c capturedRequest) payload(t *testing.T) batchPayload {
	t.Helper()
	for _, m := range c.Message {
		if m.Role != "user" {
			continue
		}
		var p batchPayload
		if err := json.Unmarshal([]byte(m.Content), &p); err != nil {
			t.Fatalf("user message is not the JSON payload: %v\n%s", err, m.Content)
		}
		return p
	}
	t.Fatal("no user message in the request")
	return batchPayload{}
}

func (c capturedRequest) system(t *testing.T) string {
	t.Helper()
	for _, m := range c.Message {
		if m.Role == "system" {
			return m.Content
		}
	}
	t.Fatal("no system message in the request")
	return ""
}

// openAIFor returns a client pointed at a handler, plus the requests it saw.
func openAIFor(t *testing.T, h func(w http.ResponseWriter, body capturedRequest)) (*openAIClient, func() []capturedRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []capturedRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got capturedRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("request body is not JSON: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		got.Auth = r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, got)
		mu.Unlock()
		h(w, got)
	}))
	t.Cleanup(srv.Close)

	c := newOpenAI(Options{
		From:    "en",
		To:      "es",
		APIKey:  "sk-test-key",
		Workers: 1,
		BaseURL: srv.URL,
	})
	return c, func() []capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(seen)
	}
}

// reply answers with a chat completion whose content is the given string.
func reply(w http.ResponseWriter, content string) {
	body, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]string{
			"role": "assistant", "content": content,
		}}},
	})
	w.Header().Set("Content-Type", "application/json")
	w.Write(body)
}

// echoTranslation answers a batch by prefixing every line it was given, which is
// the well-formed case: the same keys, in a JSON object, and nothing else.
func echoTranslation(w http.ResponseWriter, p batchPayload) {
	out := make(map[string]string, len(p.Translate))
	for k, v := range p.Translate {
		out[k] = "ES:" + v
	}
	b, _ := json.Marshal(out)
	reply(w, string(b))
}

func TestOpenAIRequestShape(t *testing.T) {
	c, requests := openAIFor(t, func(w http.ResponseWriter, got capturedRequest) {
		var p batchPayload
		json.Unmarshal([]byte(got.Message[1].Content), &p)
		echoTranslation(w, p)
	})

	// Two batches, so the second one has three preceding lines to carry as context.
	texts := lines(45)
	got, failed, err := c.TranslateAll(texts, nil)
	if err != nil || len(failed) != 0 {
		t.Fatalf("err=%v failed=%v", err, failed)
	}

	reqs := requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests for 45 blocks, want 2 batches of at most %d", len(reqs), batchSize)
	}

	// The model and the key that were configured, not something hardcoded.
	for i, r := range reqs {
		if r.Model != DefaultModel {
			t.Errorf("request %d asked for model %q, want %q", i, r.Model, DefaultModel)
		}
		if want := "Bearer sk-test-key"; r.Auth != want {
			t.Errorf("request %d Authorization = %q, want %q", i, r.Auth, want)
		}
	}

	// The batch goes out as an object keyed by the block's index as a string.
	first := reqs[0].payload(t)
	if len(first.Translate) != batchSize {
		t.Fatalf("first batch carried %d blocks, want %d", len(first.Translate), batchSize)
	}
	for i := 0; i < batchSize; i++ {
		k := strconv.Itoa(i)
		if v, ok := first.Translate[k]; !ok {
			t.Errorf("first batch has no key %q", k)
		} else if v != texts[i] {
			t.Errorf("first batch[%q] = %q, want %q", k, v, texts[i])
		}
	}
	if len(first.ContextDoNotTranslate) != 0 {
		t.Errorf("the first batch has nothing before it, got context %v", first.ContextDoNotTranslate)
	}

	// The second batch carries the three lines already seen, in a separate field
	// whose very name says they are not to be translated.
	second := reqs[1].payload(t)
	if want := []string{"line-37", "line-38", "line-39"}; !slices.Equal(second.ContextDoNotTranslate, want) {
		t.Errorf("context = %v, want the previous %d lines %v",
			second.ContextDoNotTranslate, contextLines, want)
	}
	raw := reqs[1].Message[1].Content
	if !strings.Contains(raw, "do_not_translate") {
		t.Errorf("the context field is not marked as not-to-translate:\n%s", raw)
	}
	if strings.Contains(raw, `"translate":{"0":"line-37"`) {
		t.Errorf("the context lines leaked into the batch itself:\n%s", raw)
	}

	// And the instruction says so too, in the languages that were asked for.
	sys := reqs[1].system(t)
	for _, want := range []string{"Never translate them", "context_previous_lines_do_not_translate", "en", "es", "JSON object"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt does not mention %q:\n%s", want, sys)
		}
	}

	for i := range texts {
		if want := "ES:line-" + strconv.Itoa(i); got[i] != want {
			t.Errorf("results[%d] = %q, want %q", i, got[i], want)
		}
	}
}

// The keys are what maps a translation back onto a subtitle block, so a
// well-formed reply must land on exactly the blocks it was asked about — even
// when the model answers them in another order.
func TestOpenAIWellFormedResponseMapsToTheRightIndices(t *testing.T) {
	c, _ := openAIFor(t, func(w http.ResponseWriter, got capturedRequest) {
		p := batchPayload{}
		json.Unmarshal([]byte(got.Message[1].Content), &p)
		// Answer in a deliberately shuffled JSON object.
		var b strings.Builder
		b.WriteString("{")
		keys := make([]string, 0, len(p.Translate))
		for k := range p.Translate {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		slices.Reverse(keys)
		for i, k := range keys {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, "%q:%q", k, "ES:"+p.Translate[k])
		}
		b.WriteString("}")
		reply(w, b.String())
	})

	texts := []string{"alpha", "beta", "gamma"}
	got, failed, err := c.TranslateAll(texts, nil)
	if err != nil || len(failed) != 0 {
		t.Fatalf("err=%v failed=%v", err, failed)
	}
	for i, want := range []string{"ES:alpha", "ES:beta", "ES:gamma"} {
		if got[i] != want {
			t.Errorf("results[%d] = %q, want %q", i, got[i], want)
		}
	}
}

// A reply whose key set is not the requested one cannot be trusted at all: there
// is no way to tell which line a shifted key belongs to. The batch fails and the
// blocks are retried one at a time.
func TestOpenAIMalformedResponseFallsThroughToPerBlock(t *testing.T) {
	for _, tt := range []struct {
		name  string
		badly func(p batchPayload) string
	}{
		{"missing a key", func(p batchPayload) string {
			out := map[string]string{}
			for k, v := range p.Translate {
				if k == "1" {
					continue
				}
				out[k] = "ES:" + v
			}
			b, _ := json.Marshal(out)
			return string(b)
		}},
		{"an extra key", func(p batchPayload) string {
			out := map[string]string{"99": "ES:invented"}
			for k, v := range p.Translate {
				out[k] = "ES:" + v
			}
			b, _ := json.Marshal(out)
			return string(b)
		}},
		{"an unparseable body", func(p batchPayload) string {
			return "Sure! Here are your translations, but not as JSON."
		}},
		// The likeliest LLM misbehaviour of the lot, and the most dangerous: the
		// set is the right size, so only comparing the keys catches it. Accepting
		// it would put every line one block out of place in a file that still
		// looks finished.
		{"renumbered keys", func(p batchPayload) string {
			out := map[string]string{}
			for k, v := range p.Translate {
				n, _ := strconv.Atoi(k)
				out[strconv.Itoa(n+1)] = "ES:" + v
			}
			b, _ := json.Marshal(out)
			return string(b)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c, requests := openAIFor(t, func(w http.ResponseWriter, got capturedRequest) {
				var p batchPayload
				json.Unmarshal([]byte(got.Message[1].Content), &p)
				if len(p.Translate) == 1 {
					// The per-block path: answer it properly.
					echoTranslation(w, p)
					return
				}
				reply(w, tt.badly(p))
			})

			texts := []string{"one", "two", "three"}
			got, failed, err := c.TranslateAll(texts, nil)
			if err != nil {
				t.Fatalf("the per-block path should have rescued this: %v", err)
			}
			if len(failed) != 0 {
				t.Errorf("failed = %v, want none", failed)
			}
			for i, want := range []string{"ES:one", "ES:two", "ES:three"} {
				if got[i] != want {
					t.Errorf("results[%d] = %q, want %q", i, got[i], want)
				}
			}
			// The batch is asked again before the fan-out, because one more batch
			// request is far cheaper than len(batch) requests that each re-send the
			// system prompt and the context lines.
			if n := len(requests()); n != malformedAttempts+len(texts) {
				t.Errorf("%d requests, want %d: the batch twice, then each block",
					n, malformedAttempts+len(texts))
			}
		})
	}
}

// A reply is sampled, so the same prompt can come back well-formed on a second
// look. Retrying the batch costs one request; fanning out costs one per block,
// each re-sending the system prompt and the context lines.
func TestOpenAIRetriesTheBatchBeforeFanningOut(t *testing.T) {
	var calls atomic.Int32
	c, requests := openAIFor(t, func(w http.ResponseWriter, got capturedRequest) {
		var p batchPayload
		json.Unmarshal([]byte(got.Message[1].Content), &p)
		if calls.Add(1) == 1 {
			reply(w, "Absolutely! Here you go, chief.")
			return
		}
		echoTranslation(w, p)
	})

	texts := []string{"one", "two", "three"}
	got, failed, err := c.TranslateAll(texts, nil)
	if err != nil || len(failed) != 0 {
		t.Fatalf("err=%v failed=%v", err, failed)
	}
	for i, want := range []string{"ES:one", "ES:two", "ES:three"} {
		if got[i] != want {
			t.Errorf("results[%d] = %q, want %q", i, got[i], want)
		}
	}
	if n := len(requests()); n != 2 {
		t.Errorf("%d requests, want 2: the malformed batch and the retry that "+
			"worked — no block was translated on its own", n)
	}
}

// A 400 will fail identically however many times it is sent. Repeating it, or
// fanning it out per block, only delays the error the user needs to see.
func TestOpenAIDoesNotRetryClientErrors(t *testing.T) {
	c, requests := openAIFor(t, func(w http.ResponseWriter, got capturedRequest) {
		w.WriteHeader(http.StatusBadRequest)
	})

	texts := []string{"one", "two", "three"}
	got, failed, err := c.TranslateAll(texts, nil)
	if err == nil {
		t.Fatal("want an error when every block failed")
	}
	if len(failed) != len(texts) {
		t.Errorf("failed = %v, want all %d blocks", failed, len(texts))
	}
	for i := range texts {
		if got[i] != texts[i] {
			t.Errorf("block %d: want the original kept, got %q", i, got[i])
		}
	}
	if n := len(requests()); n != 1 {
		t.Errorf("%d requests for a 400 — it was retried or fanned out per block", n)
	}
}

// A 429 that never clears must not fan out either: that is what once turned a
// sustained rate limit into a multi-hour wait before any error appeared.
func TestOpenAISustainedRateLimitDoesNotFanOut(t *testing.T) {
	c, requests := openAIFor(t, func(w http.ResponseWriter, got capturedRequest) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	_, failed, err := c.TranslateAll(lines(5), nil)
	if err == nil || len(failed) != 5 {
		t.Fatalf("want a total failure: err=%v failed=%v", err, failed)
	}
	// The wording is what tells someone they are being throttled and should lower
	// -workers, rather than that they have found a bug in this program.
	if e := strings.ToLower(err.Error()); !strings.Contains(e, "429") && !strings.Contains(e, "rate") {
		t.Errorf("error %q names neither the status nor rate limiting", err)
	}
	if n := len(requests()); n > maxAttempts {
		t.Errorf("%d requests, want at most the %d batch attempts", n, maxAttempts)
	}
}

// NewTranslator is a constructor, not a factory: there is one endpoint. What it
// still has to get right is the defaults, because a partially configured machine
// must run rather than translate one batch at a time or against no model at all.
func TestNewTranslatorAppliesTheDefaults(t *testing.T) {
	d, err := NewTranslator(Options{From: "en", To: "es", APIKey: "sk-x"})
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	dc, ok := d.(*openAIClient)
	if !ok {
		t.Fatalf("NewTranslator built %T, want *openAIClient", d)
	}
	if dc.model != DefaultModel {
		t.Errorf("model = %q, want the default %q", dc.model, DefaultModel)
	}
	if dc.baseURL != defaultOpenAIURL {
		t.Errorf("baseURL = %q, want %q", dc.baseURL, defaultOpenAIURL)
	}
	if dc.workers != DefaultWorkers {
		t.Errorf("workers = %d, want the default %d", dc.workers, DefaultWorkers)
	}
	if DefaultWorkers != 4 {
		t.Errorf("DefaultWorkers = %d, want 4 — enough to hide the round-trip "+
			"latency that otherwise dominates a run", DefaultWorkers)
	}

	// And what the caller does ask for is not overridden.
	o, err := NewTranslator(Options{To: "es", APIKey: "sk-x", Model: "gpt-6-sol", Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	oc := o.(*openAIClient)
	if oc.model != "gpt-6-sol" || oc.workers != 2 {
		t.Errorf("model = %q workers = %d, want the ones asked for", oc.model, oc.workers)
	}

	// An explicit base URL keeps the seam the end-to-end tests drive a whole run
	// through.
	g, err := NewTranslator(Options{From: "en", To: "es", APIKey: "sk-x", BaseURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := g.(*openAIClient).baseURL; got != "http://127.0.0.1:1" {
		t.Errorf("baseURL = %q, want the override", got)
	}
}

// Without a key there is nothing to try, so the error has to name both ways of
// supplying one rather than surfacing as a 401 forty minutes in.
func TestNewTranslatorMissingAPIKeyIsActionable(t *testing.T) {
	_, err := NewTranslator(Options{From: "en", To: "es"})
	if err == nil {
		t.Fatal("want an error with no key")
	}
	for _, want := range []string{"OPENAI_API_KEY", "translate.api-key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}

	// Whitespace is not a key.
	if _, err := NewTranslator(Options{APIKey: "   "}); err == nil {
		t.Error("want an error for a blank key")
	}
	// And Validate says the same thing on its own, because main calls it before
	// the video is probed or a second of audio is extracted.
	if err := (Options{To: "es"}).Validate(); err == nil {
		t.Error("Options.Validate() accepted a missing key")
	}
}

func TestDecodeObjectToleratesWrapping(t *testing.T) {
	for _, tt := range []struct {
		name, content string
		want          map[string]string
	}{
		{"plain", `{"0":"uno"}`, map[string]string{"0": "uno"}},
		{"fenced", "```json\n{\"0\":\"uno\"}\n```", map[string]string{"0": "uno"}},
		{"prefaced", "Here you go:\n{\"0\":\"uno\"}\n", map[string]string{"0": "uno"}},
		// A brace in the trailing prose used to be swallowed by the search for the
		// last one, turning a perfectly usable reply into a 40-request fan-out.
		{"trailing prose with a brace", `{"0":"uno"} (tell me if {anything} looks off)`, map[string]string{"0": "uno"}},
	} {
		got, err := decodeObject(tt.content)
		if err != nil {
			t.Errorf("%s: %v", tt.name, err)
			continue
		}
		if len(got) != len(tt.want) || got["0"] != tt.want["0"] {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
	for _, bad := range []string{"", "no object here", `{"0":`, `{"0":[1,2]}`} {
		if _, err := decodeObject(bad); err == nil {
			t.Errorf("decodeObject(%q): want an error", bad)
		}
	}
}
