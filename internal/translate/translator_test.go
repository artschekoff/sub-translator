package translate

import (
	"encoding/json"
	"errors"
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

func init() {
	// Tests run with millisecond-scale timing to avoid 48-second overhead.
	retryBaseDelay = time.Millisecond
	perBlockSleep = time.Millisecond
	batchSleep = time.Millisecond
}

// lines builds n numbered source texts, which is enough to cross several
// batchSize boundaries.
func lines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("line-%d", i)
	}
	return out
}

// batchOf decodes the batch one request carries and returns it together with the
// index it starts at, parsed from its first line, so a handler can key its
// behaviour on which batch it is looking at.
//
// It takes no *testing.T on purpose: it runs on the test server's goroutine,
// where t.Fatal would abort the wrong goroutine and hang the run.
func batchOf(r capturedRequest) (batchPayload, int) {
	var p batchPayload
	for _, m := range r.Message {
		if m.Role == "user" {
			_ = json.Unmarshal([]byte(m.Content), &p)
		}
	}
	start, _ := strconv.Atoi(strings.TrimPrefix(p.Translate["0"], "line-"))
	return p, start
}

// The pool finishes batches in whatever order the endpoint answers them, and a
// subtitle file is nothing but its order. The handler deliberately answers the
// first batch last; anything that appended results instead of writing them by
// index scrambles the film. Run this under -race.
func TestTranslateAllPreservesOrderUnderConcurrency(t *testing.T) {
	texts := lines(180) // five batches: 40, 40, 40, 40, 20

	var inFlight, peak atomic.Int32
	c, _ := openAIFor(t, func(w http.ResponseWriter, got capturedRequest) {
		if n := inFlight.Add(1); n > peak.Load() {
			peak.Store(n)
		}
		defer inFlight.Add(-1)

		p, start := batchOf(got)
		// Earlier batches are slower, so completion order is the reverse of
		// request order.
		time.Sleep(time.Duration(5-start/batchSize) * 8 * time.Millisecond)
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
		t.Errorf("peak concurrency was %d: the pool never ran two batches at once, "+
			"so this test proves nothing", peak.Load())
	}
}

// workers: 1 must reproduce the sequential run this package always did: one
// batch in flight at a time, in ascending order.
func TestTranslateAllWorkersOneIsSequential(t *testing.T) {
	texts := lines(100)

	var mu sync.Mutex
	var order []int
	var inFlight, peak atomic.Int32
	c, _ := openAIFor(t, func(w http.ResponseWriter, got capturedRequest) {
		if n := inFlight.Add(1); n > peak.Load() {
			peak.Store(n)
		}
		defer inFlight.Add(-1)

		p, start := batchOf(got)
		mu.Lock()
		order = append(order, start)
		mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		echoTranslation(w, p)
	})
	c.workers = 1

	if _, failed, err := c.TranslateAll(texts, nil); err != nil || len(failed) != 0 {
		t.Fatalf("err=%v failed=%v", err, failed)
	}
	if want := []int{0, 40, 80}; !slices.Equal(order, want) {
		t.Errorf("batches were requested in order %v, want %v", order, want)
	}
	if peak.Load() != 1 {
		t.Errorf("peak concurrency was %d with workers: 1, want 1", peak.Load())
	}
}

// Progress is read by a human off a single line of terminal, so it must only
// ever move forward, must never claim more than the total, and must count blocks
// translated rather than blocks attempted — a wholly failed run printing
// "1834/1834 (100%)" is the bug this package was changed to stop telling.
func TestTranslateAllProgressIsMonotonicAndCountsTranslated(t *testing.T) {
	texts := lines(180)

	c, _ := openAIFor(t, func(w http.ResponseWriter, got capturedRequest) {
		p, start := batchOf(got)
		// The third batch is throttled into failure; the rest answer out of order.
		if start == 80 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		time.Sleep(time.Duration(5-start/batchSize) * 4 * time.Millisecond)
		echoTranslation(w, p)
	})
	c.workers = 4

	var mu sync.Mutex
	var seen [][2]int
	_, failed, err := c.TranslateAll(texts, func(done, total int) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, [2]int{done, total})
	})
	if err != nil {
		t.Fatalf("partial failure must not be fatal: %v", err)
	}
	if len(failed) != 40 {
		t.Fatalf("failed = %d blocks, want the throttled batch's 40", len(failed))
	}
	if len(seen) == 0 {
		t.Fatal("progress was never reported")
	}
	last := 0
	for i, p := range seen {
		done, total := p[0], p[1]
		if total != len(texts) {
			t.Fatalf("progress[%d] total = %d, want %d", i, total, len(texts))
		}
		if done > total {
			t.Fatalf("progress[%d] = %d/%d — more blocks done than exist", i, done, total)
		}
		if done < last {
			t.Fatalf("progress went backwards: %d after %d", done, last)
		}
		last = done
	}
	if last != 140 {
		t.Errorf("progress ended at %d/%d, want 140 — 40 blocks were never translated",
			last, len(texts))
	}
}

// A 429 that says how long to wait must be obeyed instead of the backoff: the
// widening delay is a guess, and the header is not. retryAfterUnit shrinks the
// server's "3" to milliseconds so the suite does not sleep for three seconds.
func TestRetryHonoursRetryAfter(t *testing.T) {
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
		p, _ := batchOf(got)
		echoTranslation(w, p)
	})

	start := time.Now()
	got, failed, err := c.TranslateAll([]string{"hello"}, nil)
	elapsed := time.Since(start)

	if err != nil || len(failed) != 0 {
		t.Fatalf("want success after the retry: err=%v failed=%v", err, failed)
	}
	if !strings.HasPrefix(got[0], "ES:") {
		t.Errorf("got %q, want a translation", got[0])
	}
	// retryBaseDelay is one millisecond in this suite, so only the header can
	// account for a wait of three units.
	if want := 3 * retryAfterUnit; elapsed < want {
		t.Errorf("retried after %s, want at least %s — Retry-After was ignored", elapsed, want)
	}
	if n := len(requests()); n != 2 {
		t.Errorf("%d requests, want 2: the throttled one and the retry that worked", n)
	}
}

func TestParseRetryAfterIgnoresWhatItCannotUse(t *testing.T) {
	oldUnit := retryAfterUnit
	retryAfterUnit = time.Second
	t.Cleanup(func() { retryAfterUnit = oldUnit })

	for _, tt := range []struct {
		value string
		want  time.Duration
	}{
		{"", 0},
		{"2", 2 * time.Second},
		{" 2 ", 2 * time.Second},
		{"0", 0},
		{"-5", 0},
		{"Wed, 21 Oct 2015 07:28:00 GMT", 0}, // HTTP-date form: fall back to the backoff
		{"soon", 0},
		{"86400", maxRetryAfter}, // capped: a film must not hang for a day
	} {
		h := http.Header{}
		if tt.value != "" {
			h.Set("Retry-After", tt.value)
		}
		if got := parseRetryAfter(h); got != tt.want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", tt.value, got, tt.want)
		}
	}
}

// One bad block must not throw away the other 1833. The batch fails for a reason
// that says nothing about the endpoint, the blocks are retried one at a time, and
// the single block that keeps failing is reported by index while its neighbours
// come back translated.
func TestTranslateAllPartialFailureReportsIndices(t *testing.T) {
	c, _ := openAIFor(t, func(w http.ResponseWriter, got capturedRequest) {
		p, _ := batchOf(got)
		if len(p.Translate) > 1 {
			// The batch: a reply about blocks nobody asked about, which is not the
			// endpoint's health and so falls through to the per-block path.
			reply(w, `{"999":"ES:invented"}`)
			return
		}
		if p.Translate["0"] == "BOOM" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		echoTranslation(w, p)
	})

	texts := []string{"one", "BOOM", "three"}
	got, failed, err := c.TranslateAll(texts, nil)
	if err != nil {
		t.Fatalf("partial failure must not be fatal: %v", err)
	}
	if len(failed) != 1 || failed[0] != 1 {
		t.Fatalf("failed = %v, want [1]", failed)
	}
	if got[1] != "BOOM" {
		t.Errorf("failed block must keep its original, got %q", got[1])
	}
	if got[0] != "ES:one" || got[2] != "ES:three" {
		t.Errorf("surviving blocks were not translated: %q, %q", got[0], got[2])
	}
}

// A batch that keeps coming back 5xx is not proof the endpoint is down: the
// server may be choking on the batch itself. Giving the whole batch up at once
// is what left the opening lines of a -fast run in the source language, so the
// blocks get their own attempt — and a server that refuses those too still
// costs a bounded handful of requests, not one per block.
func TestTranslateAllFallsBackPerBlockAfterABatch5xx(t *testing.T) {
	t.Run("singles succeed", func(t *testing.T) {
		c, _ := openAIFor(t, func(w http.ResponseWriter, got capturedRequest) {
			p, _ := batchOf(got)
			if len(p.Translate) > 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			echoTranslation(w, p)
		})
		c.workers = 1

		texts := lines(5)
		got, failed, err := c.TranslateAll(texts, nil)
		if err != nil {
			t.Fatalf("TranslateAll() = %v, want success via the per-block path", err)
		}
		if len(failed) != 0 {
			t.Errorf("failed = %v, want none", failed)
		}
		for i, s := range got {
			if s != "ES:"+texts[i] {
				t.Errorf("block %d = %q, want it translated", i, s)
			}
		}
	})

	t.Run("server down", func(t *testing.T) {
		c, requests := openAIFor(t, func(w http.ResponseWriter, got capturedRequest) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		c.workers = 1

		texts := lines(batchSize)
		_, failed, err := c.TranslateAll(texts, nil)
		if err == nil {
			t.Fatal("want an error when nothing could be translated")
		}
		if len(failed) != len(texts) {
			t.Errorf("failed = %d blocks, want all %d", len(failed), len(texts))
		}
		if n, budget := len(requests()), 3*maxAttempts; n > budget {
			t.Errorf("%d requests for a down server, want at most %d", n, budget)
		}
	})
}

// The per-block path must not outlive the endpoint's health. A batch can fail
// for a reason that says nothing about the server — a reply whose keys are not
// the ones it was asked about — and the blocks are then retried one at a time;
// but once those single blocks start coming back refused, the rest cannot
// possibly succeed. Continuing is what turns a rate limit into hours of silence:
// 40 blocks × maxAttempts requests, each attempt able to honour a Retry-After,
// four workers deep, with the user watching "progress: 0/1834" and no error at
// all. Measured at 161 requests and 120 seconds for one batch before this was
// bounded.
func TestTranslateAllStopsFanningOutWhenTheEndpointRefuses(t *testing.T) {
	// The header is honoured for real; only the unit is shrunk, or asserting the
	// request count would cost the suite three seconds of genuine sleeping.
	oldUnit := retryAfterUnit
	retryAfterUnit = 5 * time.Millisecond
	t.Cleanup(func() { retryAfterUnit = oldUnit })

	tests := []struct {
		name string
		// status is what every per-block request comes back as.
		status int
		// wantAtMost is the request budget: the batch attempts, plus the blocks it
		// took to establish that the endpoint is refusing. Never a multiple of
		// len(batch).
		wantAtMost int
	}{
		// Throttling is about us, so the first one ends the batch.
		{"throttled", http.StatusTooManyRequests, malformedAttempts + maxAttempts},
		// A server fault may be about one block's content, so one is tolerated —
		// but not forty.
		{"faulting", http.StatusInternalServerError, malformedAttempts + 2*maxAttempts},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			texts := lines(batchSize)

			c, requests := openAIFor(t, func(w http.ResponseWriter, got capturedRequest) {
				p, _ := batchOf(got)
				if len(p.Translate) > 1 {
					// The batch: answered 200, but about a block it was never given, so
					// the reply cannot be mapped back onto anything.
					reply(w, `{"999":"ES:invented"}`)
					return
				}
				// Every single block: refused, for as long as anyone cares to ask.
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(tt.status)
			})
			c.workers = 1

			got, failed, err := c.TranslateAll(texts, nil)
			if err == nil {
				t.Fatal("want an error when nothing could be translated")
			}
			if len(failed) != len(texts) {
				t.Errorf("failed = %d blocks, want all %d", len(failed), len(texts))
			}
			if !slices.IsSorted(failed) {
				t.Errorf("failed is not sorted: %v…", firstThree(failed))
			}
			for i := range texts {
				if got[i] != texts[i] {
					t.Errorf("block %d: want the original kept, got %q", i, got[i])
				}
			}
			if n := len(requests()); n > tt.wantAtMost {
				t.Errorf("%d requests for one refused batch, want at most %d — the "+
					"fan-out kept going after the endpoint started refusing",
					n, tt.wantAtMost)
			}
			t.Logf("requests: %d (unbounded shape would be %d)",
				len(requests()), malformedAttempts+maxAttempts*len(texts))
		})
	}
}

// firstFew(failed, 3) is shown to the user as "first: [...]", so the indices
// have to be sorted however the batches finish. Here the low-index batch is the
// slowest, so an unsorted list would open with block 40 and point the user at
// the wrong part of the film.
func TestTranslateAllSortsFailedIndicesWhateverTheCompletionOrder(t *testing.T) {
	texts := lines(3 * batchSize)

	c, _ := openAIFor(t, func(w http.ResponseWriter, got capturedRequest) {
		p, start := batchOf(got)
		switch start {
		case 0:
			// Fails last, though it was requested first.
			time.Sleep(10 * time.Millisecond)
			w.WriteHeader(http.StatusTooManyRequests)
		case batchSize:
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			echoTranslation(w, p)
		}
	})
	c.workers = 4

	_, failed, err := c.TranslateAll(texts, nil)
	if err != nil {
		t.Fatalf("one surviving batch means this is not fatal: %v", err)
	}
	if !slices.IsSorted(failed) {
		t.Errorf("failed = %v… is not sorted", firstThree(failed))
	}
	if len(failed) != 2*batchSize {
		t.Fatalf("failed = %d blocks, want the two throttled batches' %d", len(failed), 2*batchSize)
	}
	if failed[0] != 0 {
		t.Errorf("failed[0] = %d, want 0 — the message would point at the wrong "+
			"part of the film", failed[0])
	}
}

// firstThree mirrors what the caller shows the user.
func firstThree(v []int) []int {
	if len(v) <= 3 {
		return v
	}
	return v[:3]
}

// countingProvider is a provider that answers from a script, so the pool can be
// tested against behaviour no real endpoint should produce.
type countingProvider struct {
	// extra strings are appended to every batch's reply, which no honest
	// provider does and which the pool must not write into the next batch's range.
	extra int
	mu    sync.Mutex
	ones  int
}

func (p *countingProvider) batch(texts, _ []string) ([]string, error) {
	out := make([]string, 0, len(texts)+p.extra)
	for _, t := range texts {
		out = append(out, "ES:"+t)
	}
	for i := 0; i < p.extra; i++ {
		out = append(out, "OVERFLOW")
	}
	return out, nil
}

func (p *countingProvider) one(text string, _ []string) (string, error) {
	p.mu.Lock()
	p.ones++
	p.mu.Unlock()
	return "ONE:" + text, nil
}

// A provider that returns the wrong number of strings must not be trusted with
// the results slice. The provider counts its own replies today, so this is a
// backstop — but the pool's safety argument is that a worker writes only inside
// its own range, and the cost of being wrong is a data race plus a scrambled
// film that still reports 100%.
func TestTranslateAllRejectsAWrongLengthBatch(t *testing.T) {
	texts := lines(batchSize + 5) // two batches, so there is a range to overflow into
	p := &countingProvider{extra: 1}

	got, failed, err := translateAll(p, texts, 4, nil)
	if err != nil {
		t.Fatalf("the per-block path should have rescued this: %v", err)
	}
	if len(failed) != 0 {
		t.Errorf("failed = %v, want none", failed)
	}
	if p.ones != len(texts) {
		t.Errorf("%d per-block calls, want one per block (%d)", p.ones, len(texts))
	}
	for i, r := range got {
		if want := "ONE:line-" + strconv.Itoa(i); r != want {
			t.Fatalf("results[%d] = %q, want %q — the overlong reply was written anyway",
				i, r, want)
		}
	}
}

// The two refusals a caller can act on have to be recognisable through whatever
// wrapping the call chain adds. A caller matching on "translation service
// returned HTTP 401" is matching on prose; a caller told 401 is a rate limit —
// which is what the CLI did — sends a user who mistyped one character of their
// key to a Retry button that can never succeed.
func TestStatusErrorsAreRecognisableThroughTheSentinels(t *testing.T) {
	for _, tt := range []struct {
		code         int
		unauthorized bool
		rateLimited  bool
	}{
		{code: http.StatusUnauthorized, unauthorized: true},
		{code: http.StatusForbidden, unauthorized: true},
		{code: http.StatusTooManyRequests, rateLimited: true},
		{code: http.StatusBadRequest},
		{code: http.StatusInternalServerError},
	} {
		// Wrapped the way the real chain wraps it: translateAll's "all N blocks
		// failed", then the progressive run's per-chunk context on top.
		err := fmt.Errorf("chunk 3: translate: %w",
			fmt.Errorf("all 40 blocks failed: %w", statusError{code: tt.code}))

		if got := errors.Is(err, ErrUnauthorized); got != tt.unauthorized {
			t.Errorf("HTTP %d: errors.Is(err, ErrUnauthorized) = %v, want %v", tt.code, got, tt.unauthorized)
		}
		if got := errors.Is(err, ErrRateLimited); got != tt.rateLimited {
			t.Errorf("HTTP %d: errors.Is(err, ErrRateLimited) = %v, want %v", tt.code, got, tt.rateLimited)
		}
	}
}

// End to end through the real client: a refusing endpoint, every block failing,
// and the error the caller is handed still says which refusal it was.
func TestARefusingEndpointNamesItsRefusal(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status int
		want   error
	}{
		{name: "a rejected key", status: http.StatusUnauthorized, want: ErrUnauthorized},
		{name: "a rate limit", status: http.StatusTooManyRequests, want: ErrRateLimited},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			c := newOpenAI(Options{APIKey: "sk-test", BaseURL: srv.URL, Workers: 1, To: "es"})
			_, failed, err := c.TranslateAll([]string{"one", "two"}, nil)
			if err == nil {
				t.Fatalf("TranslateAll() = nil, want the endpoint's HTTP %d", tt.status)
			}
			if len(failed) != 2 {
				t.Errorf("%d blocks failed, want both", len(failed))
			}
			if !errors.Is(err, tt.want) {
				t.Errorf("errors.Is(%v, %v) = false, want true", err, tt.want)
			}
			// And not the other one: narrowing rate_limited to 429 is half the
			// point of the pair.
			other := ErrRateLimited
			if tt.want == ErrRateLimited {
				other = ErrUnauthorized
			}
			if errors.Is(err, other) {
				t.Errorf("errors.Is(%v, %v) = true, want false", err, other)
			}
		})
	}
}
