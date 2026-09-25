package translate

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

// jsonFor builds the shape Google's endpoint returns: [[[translated, original, ...]], ...]
func jsonFor(text string) string {
	return fmt.Sprintf(`[[[%q,%q,null,null,3]],null,"en"]`, "ES:"+text, text)
}

func clientFor(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New("en", "es")
	c.baseURL = srv.URL
	return c
}

// A translation that fails for every block used to print "100%" and return a nil
// error, so an untranslated file looked exactly like a translated one.
func TestTranslateAllTotalFailureIsAnError(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})

	texts := []string{"one", "two", "three"}
	got, failed, err := c.TranslateAll(texts, nil)
	if err == nil {
		t.Fatal("want an error when nothing could be translated")
	}
	if !strings.Contains(err.Error(), "429") && !strings.Contains(strings.ToLower(err.Error()), "rate") {
		t.Errorf("error %q names neither the status nor rate limiting", err)
	}
	if len(failed) != len(texts) {
		t.Errorf("failed = %d blocks, want all %d", len(failed), len(texts))
	}
	for i := range texts {
		if got[i] != texts[i] {
			t.Errorf("block %d: want the original kept, got %q", i, got[i])
		}
	}
}

// One bad block must not throw away the other 1833.
func TestTranslateAllPartialFailureReportsIndices(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if strings.Contains(q, "|||") {
			// Batch request: return malformed response to trigger separator mismatch
			// (this causes per-block retries rather than marking all as failed)
			fmt.Fprint(w, jsonFor("malformed"))
			return
		}
		// Per-block request: BOOM fails, others succeed
		if q == "BOOM" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, jsonFor(q))
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
	if !strings.HasPrefix(got[0], "ES:") || !strings.HasPrefix(got[2], "ES:") {
		t.Errorf("surviving blocks were not translated: %q, %q", got[0], got[2])
	}
}

// 429 is usually transient, so a short burst of throttling must not cost the run.
func TestTranslateAllRetriesRateLimiting(t *testing.T) {
	var calls atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		fmt.Fprint(w, jsonFor(r.URL.Query().Get("q")))
	})

	got, failed, err := c.TranslateAll([]string{"hello"}, nil)
	if err != nil {
		t.Fatalf("want success after retries: %v", err)
	}
	if len(failed) != 0 {
		t.Errorf("failed = %v, want none", failed)
	}
	if !strings.HasPrefix(got[0], "ES:") {
		t.Errorf("got %q, want a translation", got[0])
	}
	if n := calls.Load(); n < 3 {
		t.Errorf("only %d attempts — the retry never happened", n)
	}
}

// Repeating a request the server called malformed just wastes time.
func TestTranslateAllDoesNotRetryClientErrors(t *testing.T) {
	var calls atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	})

	if _, _, err := c.TranslateAll([]string{"hello"}, nil); err == nil {
		t.Fatal("want an error")
	}
	// One batch attempt plus one per-block attempt; a retry loop would multiply this.
	if n := calls.Load(); n > 2 {
		t.Errorf("%d requests for a 400 — it was retried", n)
	}
}

// A batch that fails with a status error after exhausting retries must not fan out
// to per-block retries: that would turn a rate limit into a multi-hour hang.
// When the endpoint is throttling us, individual blocks cannot succeed where the
// batch just failed four times.
func TestTranslateAllStatusErrorBatchDoesNotRetryPerBlock(t *testing.T) {
	var calls atomic.Int32
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	})

	texts := []string{"one", "two", "three", "four", "five"}
	got, failed, err := c.TranslateAll(texts, nil)
	if err == nil {
		t.Fatal("want an error when batch fails with 429")
	}
	if len(failed) != len(texts) {
		t.Errorf("failed = %d blocks, want all %d", len(failed), len(texts))
	}
	for i := range texts {
		if got[i] != texts[i] {
			t.Errorf("block %d: want original kept, got %q", i, got[i])
		}
	}
	// maxAttempts retries on the joined batch only; no per-block attempts.
	// Total should be around 4 (batch retries), not 4 + 5*4 (batch + per-block retries).
	n := calls.Load()
	if n > int32(maxAttempts+1) {
		t.Errorf("%d requests for a status failure — per-block retries happened (%d > %d)",
			n, n, maxAttempts+1)
	}
}

func TestTranslateAllBatchRoundTrip(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, jsonFor(r.URL.Query().Get("q")))
	})

	texts := []string{"alpha", "beta", "gamma"}
	got, failed, err := c.TranslateAll(texts, nil)
	if err != nil || len(failed) != 0 {
		t.Fatalf("err=%v failed=%v", err, failed)
	}
	if len(got) != len(texts) {
		t.Fatalf("got %d results, want %d", len(got), len(texts))
	}
}

func TestTranslateAllReportsProgress(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, jsonFor(r.URL.Query().Get("q")))
	})
	var lastDone, lastTotal int
	if _, _, err := c.TranslateAll([]string{"a", "b"}, func(done, total int) {
		lastDone, lastTotal = done, total
	}); err != nil {
		t.Fatal(err)
	}
	if lastDone != 2 || lastTotal != 2 {
		t.Errorf("progress ended at %d/%d, want 2/2", lastDone, lastTotal)
	}
}
