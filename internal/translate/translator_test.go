package translate

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// lines builds n numbered source texts, which is enough to cross several
// batchSize boundaries.
func lines(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("line-%d", i)
	}
	return out
}

// batchOf answers a Google-shaped batch request by prefixing every line, and
// returns the index the batch started at so a handler can key its behaviour on
// which batch it is looking at.
func batchOf(q string) (start int, reply string) {
	parts := strings.Split(q, strings.TrimSpace(separator))
	start, _ = strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(parts[0]), "line-"))
	for i, p := range parts {
		parts[i] = "ES:" + strings.TrimSpace(p)
	}
	return start, jsonFor2(strings.Join(parts, separator))
}

// The pool finishes batches in whatever order the endpoint answers them, and a
// subtitle file is nothing but its order. The handler deliberately answers the
// first batch last; anything that appended results instead of writing them by
// index scrambles the film. Run this under -race.
func TestTranslateAllPreservesOrderUnderConcurrency(t *testing.T) {
	texts := lines(180) // five batches: 40, 40, 40, 40, 20

	var inFlight, peak atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if n := inFlight.Add(1); n > peak.Load() {
			peak.Store(n)
		}
		defer inFlight.Add(-1)

		start, reply := batchOf(r.URL.Query().Get("q"))
		// Earlier batches are slower, so completion order is the reverse of
		// request order.
		time.Sleep(time.Duration(5-start/batchSize) * 8 * time.Millisecond)
		fmt.Fprint(w, reply)
	})
	c.Workers = 4

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

// Workers: 1 must reproduce the sequential run this package always did: one
// batch in flight at a time, in ascending order.
func TestTranslateAllWorkersOneIsSequential(t *testing.T) {
	texts := lines(100)

	var mu sync.Mutex
	var order []int
	var inFlight, peak atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if n := inFlight.Add(1); n > peak.Load() {
			peak.Store(n)
		}
		defer inFlight.Add(-1)

		start, reply := batchOf(r.URL.Query().Get("q"))
		mu.Lock()
		order = append(order, start)
		mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		fmt.Fprint(w, reply)
	})
	c.Workers = 1

	if _, failed, err := c.TranslateAll(texts, nil); err != nil || len(failed) != 0 {
		t.Fatalf("err=%v failed=%v", err, failed)
	}
	if want := []int{0, 40, 80}; !slices.Equal(order, want) {
		t.Errorf("batches were requested in order %v, want %v", order, want)
	}
	if peak.Load() != 1 {
		t.Errorf("peak concurrency was %d with Workers: 1, want 1", peak.Load())
	}
}

// Progress is read by a human off a single line of terminal, so it must only
// ever move forward, must never claim more than the total, and must count blocks
// translated rather than blocks attempted — a wholly failed run printing
// "1834/1834 (100%)" is the bug this package was changed to stop telling.
func TestTranslateAllProgressIsMonotonicAndCountsTranslated(t *testing.T) {
	texts := lines(180)

	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		start, reply := batchOf(q)
		// The third batch is throttled into failure; the rest answer out of order.
		if start == 80 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		time.Sleep(time.Duration(5-start/batchSize) * 4 * time.Millisecond)
		fmt.Fprint(w, reply)
	})
	c.Workers = 4

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
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, jsonFor(r.URL.Query().Get("q")))
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

// The per-block path must not outlive the endpoint's health. A batch can fail
// for a reason that says nothing about the server — a mangled separator here —
// and the blocks are then retried one at a time; but once those single blocks
// start coming back refused, the rest cannot possibly succeed. Continuing is
// what turns a rate limit into hours of silence: 40 blocks × maxAttempts
// requests, each attempt able to honour a Retry-After, four workers deep, with
// the user watching "progress: 0/1834" and no error at all. Measured at 161
// requests and 120 seconds for one batch before this was bounded.
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
		// wantAtMost is the request budget: the batch, plus the blocks it took to
		// establish that the endpoint is refusing. Never a multiple of len(batch).
		wantAtMost int32
	}{
		// Throttling is about us, so the first one ends the batch.
		{"throttled", http.StatusTooManyRequests, 1 + maxAttempts},
		// A server fault may be about one block's content, so one is tolerated —
		// but not forty.
		{"faulting", http.StatusInternalServerError, 1 + 2*maxAttempts},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			texts := lines(batchSize)

			var calls atomic.Int32
			c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if strings.Contains(r.URL.Query().Get("q"), strings.TrimSpace(separator)) {
					// The batch: answered 200, but with the separator eaten, so the
					// result cannot be split back apart.
					fmt.Fprint(w, jsonFor2("one two three"))
					return
				}
				// Every single block: refused, for as long as anyone cares to ask.
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(tt.status)
			})
			c.Workers = 1

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
			if calls.Load() > tt.wantAtMost {
				t.Errorf("%d requests for one refused batch, want at most %d — the "+
					"fan-out kept going after the endpoint started refusing",
					calls.Load(), tt.wantAtMost)
			}
			t.Logf("requests: %d (unbounded shape would be %d)",
				calls.Load(), 1+maxAttempts*len(texts))
		})
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
// the results slice. Both providers count their own replies today, so this is a
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
