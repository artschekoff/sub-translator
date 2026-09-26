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
	"time"
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
		Provider: ProviderOpenAI,
		From:     "en",
		To:       "es",
		APIKey:   "sk-test-key",
		Workers:  1,
		BaseURL:  srv.URL,
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
// blocks are retried one at a time, which is what the Google path does when the
// separator comes back mangled.
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
			// One rejected batch plus one request per block.
			if n := len(requests()); n != 1+len(texts) {
				t.Errorf("%d requests, want %d: the batch and then each block", n, 1+len(texts))
			}
		})
	}
}

// A 429 that carries Retry-After must be waited out, and a batch that fails with
// a status error must not fan out into per-block retries.
func TestOpenAIHonoursRetryAfter(t *testing.T) {
	oldUnit := retryAfterUnit
	retryAfterUnit = 20 * time.Millisecond
	t.Cleanup(func() { retryAfterUnit = oldUnit })

	var calls atomic.Int32
	c, requests := openAIFor(t, func(w http.ResponseWriter, got capturedRequest) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		var p batchPayload
		json.Unmarshal([]byte(got.Message[1].Content), &p)
		echoTranslation(w, p)
	})

	start := time.Now()
	got, failed, err := c.TranslateAll([]string{"one", "two"}, nil)
	elapsed := time.Since(start)

	if err != nil || len(failed) != 0 {
		t.Fatalf("want success after the retry: err=%v failed=%v", err, failed)
	}
	if got[0] != "ES:one" || got[1] != "ES:two" {
		t.Errorf("got %v, want the translations", got)
	}
	if want := 3 * retryAfterUnit; elapsed < want {
		t.Errorf("retried after %s, want at least %s — Retry-After was ignored", elapsed, want)
	}
	if n := len(requests()); n != 2 {
		t.Errorf("%d requests, want 2: the throttled one and the retry", n)
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

	if _, failed, err := c.TranslateAll(lines(5), nil); err == nil || len(failed) != 5 {
		t.Fatalf("want a total failure: err=%v failed=%v", err, failed)
	}
	if n := len(requests()); n > maxAttempts {
		t.Errorf("%d requests, want at most the %d batch attempts", n, maxAttempts)
	}
}

// The pool is shared, so order under concurrency is worth proving through the
// provider that answers out of order most readily. Run under -race.
func TestOpenAIPreservesOrderUnderConcurrency(t *testing.T) {
	texts := lines(180)
	var inFlight, peak atomic.Int32

	c, _ := openAIFor(t, func(w http.ResponseWriter, got capturedRequest) {
		if n := inFlight.Add(1); n > peak.Load() {
			peak.Store(n)
		}
		defer inFlight.Add(-1)

		var p batchPayload
		json.Unmarshal([]byte(got.Message[1].Content), &p)
		// Jitter keyed on the batch's first line, so earlier batches answer last.
		n, _ := strconv.Atoi(strings.TrimPrefix(p.Translate["0"], "line-"))
		time.Sleep(time.Duration(5-n/batchSize) * 8 * time.Millisecond)
		echoTranslation(w, p)
	})
	c.workers = 4

	got, failed, err := c.TranslateAll(texts, nil)
	if err != nil || len(failed) != 0 {
		t.Fatalf("err=%v failed=%v", err, failed)
	}
	for i := range texts {
		if want := "ES:line-" + strconv.Itoa(i); got[i] != want {
			t.Fatalf("results[%d] = %q, want %q — the order was not preserved", i, got[i], want)
		}
	}
	if peak.Load() < 2 {
		t.Errorf("peak concurrency was %d: nothing overlapped, so this proves nothing", peak.Load())
	}
}

func TestDefaultWorkersForIsProviderSpecific(t *testing.T) {
	if got := DefaultWorkersFor(ProviderGoogle); got != 1 {
		t.Errorf("DefaultWorkersFor(google) = %d, want 1", got)
	}
	if got := DefaultWorkersFor(""); got != 1 {
		t.Errorf("DefaultWorkersFor(\"\") = %d, want 1 — an unset provider is google", got)
	}
	if got := DefaultWorkersFor(ProviderOpenAI); got != 4 {
		t.Errorf("DefaultWorkersFor(openai) = %d, want 4", got)
	}
}

func TestNewTranslatorChoosesTheProvider(t *testing.T) {
	g, err := NewTranslator(Options{From: "en", To: "es"})
	if err != nil {
		t.Fatalf("default provider: %v", err)
	}
	gc, ok := g.(*Client)
	if !ok {
		t.Fatalf("default provider built %T, want the Google *Client", g)
	}
	// One worker against the free public endpoint, whatever the openai default is.
	if gc.Workers != defaultGoogleWorkers {
		t.Errorf("Workers = %d, want the Google default %d", gc.Workers, defaultGoogleWorkers)
	}
	if defaultGoogleWorkers != 1 {
		t.Errorf("defaultGoogleWorkers = %d: four batches in flight against a free "+
			"endpoint is what got this host blocked for nineteen minutes",
			defaultGoogleWorkers)
	}

	// And the openai default is the parallel one, applied when nothing asks.
	o4, err := NewTranslator(Options{Provider: ProviderOpenAI, To: "es", APIKey: "sk-x"})
	if err != nil {
		t.Fatal(err)
	}
	if got := o4.(*openAIClient).workers; got != defaultOpenAIWorkers {
		t.Errorf("openai workers = %d, want the default %d", got, defaultOpenAIWorkers)
	}

	o, err := NewTranslator(Options{Provider: ProviderOpenAI, To: "es", APIKey: "sk-x", Workers: 2})
	if err != nil {
		t.Fatalf("openai provider: %v", err)
	}
	oc, ok := o.(*openAIClient)
	if !ok {
		t.Fatalf("openai provider built %T, want *openAIClient", o)
	}
	if oc.model != DefaultModel {
		t.Errorf("model = %q, want the default %q", oc.model, DefaultModel)
	}
	if oc.baseURL != defaultOpenAIURL {
		t.Errorf("baseURL = %q, want %q", oc.baseURL, defaultOpenAIURL)
	}
	if oc.workers != 2 {
		t.Errorf("workers = %d, want the 2 asked for", oc.workers)
	}

	// A Google client with an explicit base URL keeps the seam the end-to-end
	// tests drive a whole run through.
	g2, err := NewTranslator(Options{Provider: ProviderGoogle, From: "en", To: "es", BaseURL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := g2.(*Client).baseURL; got != "http://127.0.0.1:1" {
		t.Errorf("baseURL = %q, want the override", got)
	}
}

// Without a key there is nothing to try, so the error has to name both ways of
// supplying one rather than surfacing as a 401 forty minutes in.
func TestNewTranslatorMissingAPIKeyIsActionable(t *testing.T) {
	_, err := NewTranslator(Options{Provider: ProviderOpenAI, From: "en", To: "es"})
	if err == nil {
		t.Fatal("want an error for the openai provider with no key")
	}
	for _, want := range []string{"OPENAI_API_KEY", "translate.api-key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}

	// Whitespace is not a key.
	if _, err := NewTranslator(Options{Provider: ProviderOpenAI, APIKey: "   "}); err == nil {
		t.Error("want an error for a blank key")
	}
}

func TestNewTranslatorRejectsUnknownProvider(t *testing.T) {
	_, err := NewTranslator(Options{Provider: "deepl", From: "en", To: "es"})
	if err == nil {
		t.Fatal("want an error for an unknown provider")
	}
	for _, want := range []string{"deepl", ProviderGoogle, ProviderOpenAI} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
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
