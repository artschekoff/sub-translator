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
