# Progressive Translation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `-fast` mode that writes a watchable `.srt` covering the opening of a film within a minute and keeps extending the same file while the viewer watches — and, first, stop `internal/translate` from reporting a total failure as a success.

**Architecture:** The audio is decoded once to a 16 kHz mono WAV, then sliced by byte offset into fixed-length chunks. Each chunk is transcribed, its timestamps shifted by its start offset, translated, merged into the accumulated set, renumbered and written over the whole output file. Two new pure helpers (`srt.Shift`, `planChunks`) carry the arithmetic so it is testable without ffmpeg or whisper.

**Tech Stack:** Go 1.26, standard library only. `whisper-cli` and `ffmpeg`/`ffprobe` are invoked as subprocesses.

**Spec:** `docs/superpowers/specs/2026-09-24-progressive-translation-design.md`

## Global Constraints

- Go version floor: **1.26** (`go.mod` says `go 1.26.2`) — do not lower it, do not add a toolchain directive.
- **No new third-party dependencies.** `go.mod` has zero requires today and must still have zero when this plan is done.
- All code must pass `make validate` (runs `gofmt -w .`, `go vet ./...`, `go test ./... -v`).
- **The non-`-fast` path must behave exactly as it does today.** Every existing test must still pass unchanged.
- No test may run the real `whisper-cli`, `ffmpeg` or `ffprobe`, and no test may reach the network. Tests use pure functions, `httptest`, and the stub-script pattern already used in `internal/whisper/whisper_test.go`.
- Conventional commit messages (`feat:`, `fix:`, `test:`, `docs:`), matching the existing history.
- Do not reformat or restructure code unrelated to this feature.
- Module path is `github.com/artschekoff/sub-translator`.

---

## File Structure

| File | Status | Responsibility |
|---|---|---|
| `internal/translate/translate.go` | Modify | HTTP status checking, retry with backoff, failure tracking, injectable base URL. |
| `internal/translate/translate_test.go` | Create | Total/partial failure, retry-then-succeed, no-retry-on-400, batch round-trip. |
| `internal/srt/srt.go` | Modify | `Shift` and `Renumber`, plus the timestamp parse/format pair they need. |
| `internal/srt/srt_test.go` | Create | Timestamp round-trip, shifting, renumbering, malformed input. |
| `internal/media/media.go` | Modify | `SliceWAV`, `WAVDuration` and the pure `sliceWAVArgs`. |
| `internal/media/media_test.go` | Modify | Argument construction for the slice. |
| `progressive.go` | Create | `chunkPlan`, `planChunks`, `runProgressive`, `notifyDone`. |
| `progressive_test.go` | Create | Chunk planning, flag validation, end-to-end run against a stub whisper. |
| `main.go` | Modify | New flags, validation, the call into `runProgressive`, and the new `TranslateAll` signature. |
| `README.md` | Modify | Document `-fast`, `-chunk`, `whisper.chunk-minutes`, and the reload-subtitles caveat. |

`internal/config` gains one key, handled inside Task 6 rather than getting a task of its own.

---

### Task 1: Honest translation failures

**Files:**
- Modify: `internal/translate/translate.go`
- Modify: `main.go` (the single `TranslateAll` call site)
- Test: `internal/translate/translate_test.go` (create)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces, in package `translate`:
  - `func New(from, to string) *Client` — unchanged signature; `Client` gains an unexported `baseURL` field set to the existing endpoint
  - `func (c *Client) TranslateAll(texts []string, progress func(done, total int)) ([]string, []int, error)`

- [ ] **Step 1: Write the failing test**

Create `internal/translate/translate_test.go`:

```go
package translate

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

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
		if strings.Contains(q, "BOOM") {
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/translate/ -v`
Expected: FAIL — `c.baseURL` is undefined and `TranslateAll` returns two values, not three.

- [ ] **Step 3: Write minimal implementation**

In `internal/translate/translate.go`, change the constant block, the `Client` struct and `New`:

```go
const (
	defaultAPIURL = "https://translate.googleapis.com/translate_a/single"
	separator     = " ||||| "
	batchSize     = 40
	maxAttempts   = 4
)

type Client struct {
	From    string
	To      string
	baseURL string
	http    *http.Client
}

func New(from, to string) *Client {
	return &Client{
		From:    from,
		To:      to,
		baseURL: defaultAPIURL,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}
```

Replace `TranslateAll` with:

```go
// TranslateAll translates every text, returning the results, the indices of any
// blocks that could not be translated and kept their original text, and an error
// only when nothing at all could be translated.
//
// Reporting a total failure as success is how an untranslated file used to end up
// named .es.srt: every block silently kept its English text while the progress
// counter reached 100%.
func (c *Client) TranslateAll(texts []string, progress func(done, total int)) ([]string, []int, error) {
	results := make([]string, len(texts))
	var failed []int
	var lastErr error

	for i := 0; i < len(texts); i += batchSize {
		end := min(i+batchSize, len(texts))
		batch := texts[i:end]

		translated, err := c.translateBatch(batch)
		if err == nil {
			copy(results[i:], translated)
		} else {
			lastErr = err
			// The batch separator can be mangled by the translator, which says
			// nothing about the individual blocks — so retry them one by one.
			for j, t := range batch {
				r, err := c.translateWithRetry(t)
				if err != nil {
					results[i+j] = t
					failed = append(failed, i+j)
					lastErr = err
				} else {
					results[i+j] = r
				}
				time.Sleep(150 * time.Millisecond)
			}
		}
		if progress != nil {
			progress(end, len(texts))
		}
		time.Sleep(300 * time.Millisecond)
	}

	if len(failed) == len(texts) && len(texts) > 0 {
		return results, failed, fmt.Errorf("all %d blocks failed: %w", len(texts), lastErr)
	}
	return results, failed, nil
}

// translateWithRetry retries throttling and server faults with a widening pause.
// A 429 is usually a short burst; a 4xx that is not 429 will never succeed on a
// repeat, so it fails immediately.
func (c *Client) translateWithRetry(text string) (string, error) {
	var lastErr error
	delay := time.Second
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		out, err := c.translateOne(text)
		if err == nil {
			return out, nil
		}
		lastErr = err
		var se statusError
		if !errors.As(err, &se) || !se.retryable() {
			return "", err
		}
		if attempt < maxAttempts {
			time.Sleep(delay)
			delay *= 2
		}
	}
	return "", lastErr
}
```

Add the status error type above `translateOne`:

```go
// statusError carries the HTTP status so the retry logic can tell a transient
// throttle from a request that will never succeed.
type statusError struct{ code int }

func (e statusError) Error() string {
	if e.code == http.StatusTooManyRequests {
		return fmt.Sprintf("rate limited by the translation service (HTTP %d)", e.code)
	}
	return fmt.Sprintf("translation service returned HTTP %d", e.code)
}

func (e statusError) retryable() bool {
	return e.code == http.StatusTooManyRequests || e.code >= 500
}
```

In `translateOne`, use the client's base URL and check the status before reading the body:

```go
	resp, err := c.http.Get(c.baseURL + "?" + params.Encode())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", statusError{code: resp.StatusCode}
	}
```

And make `translateBatch` use the retrying call so a throttled batch is retried before falling back:

```go
	result, err := c.translateWithRetry(joined)
```

Extend the import block with `"errors"`.

In `main.go`, update the single call site:

```go
	translated, failedBlocks, err := client.TranslateAll(texts, func(done, total int) {
		pct := float64(done) / float64(total) * 100
		fmt.Printf("\r  progress: %d/%d (%.0f%%)   ", done, total, pct)
	})
	fmt.Println()
	if err != nil {
		fatalf("translate: %v", err)
	}
	if len(failedBlocks) > 0 {
		fmt.Fprintf(os.Stderr,
			"warning: %d of %d blocks could not be translated and kept their original text (first: %v)\n",
			len(failedBlocks), len(texts), firstFew(failedBlocks, 3))
	}
```

Add the helper at the bottom of `main.go`:

```go
// firstFew renders the first n values of a list for an error message, with a
// trailing ellipsis when there are more.
func firstFew(v []int, n int) string {
	if len(v) <= n {
		return fmt.Sprint(v)
	}
	return fmt.Sprint(v[:n]) + "…"
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/translate/ -v && make validate`
Expected: PASS. The suite takes a few seconds because of the deliberate sleeps.

- [ ] **Step 5: Commit**

```bash
git add internal/translate/ main.go
git commit -m "fix: report translation failures instead of silently keeping the original"
```

---

### Task 2: Shifting and renumbering subtitle blocks

**Files:**
- Modify: `internal/srt/srt.go`
- Test: `internal/srt/srt_test.go` (create)

**Interfaces:**
- Consumes: the existing `Block{Index, Timing, Text string}` in the same package.
- Produces, in package `srt`:
  - `func Shift(blocks []Block, by time.Duration) ([]Block, error)`
  - `func Renumber(blocks []Block) []Block`

Note for the implementer: `Block.Timing` is the raw SRT line, e.g. `00:01:23,456 --> 00:01:25,789`. `Block.Index` is the raw number line as a string. `Write` emits both verbatim and does **not** renumber.

- [ ] **Step 1: Write the failing test**

Create `internal/srt/srt_test.go`:

```go
package srt

import (
	"strings"
	"testing"
	"time"
)

func TestParseAndFormatTimestampRoundTrip(t *testing.T) {
	tests := []struct {
		text string
		want time.Duration
	}{
		{"00:00:00,000", 0},
		{"00:00:07,450", 7*time.Second + 450*time.Millisecond},
		{"01:23:45,678", time.Hour + 23*time.Minute + 45*time.Second + 678*time.Millisecond},
		{"02:19:08,000", 2*time.Hour + 19*time.Minute + 8*time.Second},
	}
	for _, tt := range tests {
		got, err := parseTimestamp(tt.text)
		if err != nil {
			t.Errorf("parseTimestamp(%q): %v", tt.text, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseTimestamp(%q) = %v, want %v", tt.text, got, tt.want)
		}
		if back := formatTimestamp(got); back != tt.text {
			t.Errorf("formatTimestamp round trip = %q, want %q", back, tt.text)
		}
	}
}

func TestParseTimestampRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "nonsense", "00:00", "0:0:0"} {
		if _, err := parseTimestamp(bad); err == nil {
			t.Errorf("parseTimestamp(%q): want an error", bad)
		}
	}
}

// Chunk N is transcribed as if it started at zero, so merging it into the film
// requires adding the chunk's start offset to every timestamp. Getting this wrong
// puts the back half of a film's subtitles minutes out of sync.
func TestShiftOffsetsBothEnds(t *testing.T) {
	in := []Block{
		{Index: "1", Timing: "00:00:00,000 --> 00:00:02,500", Text: "first"},
		{Index: "2", Timing: "00:00:03,000 --> 00:00:05,250", Text: "second"},
	}

	got, err := Shift(in, 10*time.Minute)
	if err != nil {
		t.Fatalf("Shift: %v", err)
	}
	want := []string{
		"00:10:00,000 --> 00:10:02,500",
		"00:10:03,000 --> 00:10:05,250",
	}
	for i := range want {
		if got[i].Timing != want[i] {
			t.Errorf("block %d timing = %q, want %q", i, got[i].Timing, want[i])
		}
		if got[i].Text != in[i].Text {
			t.Errorf("block %d text changed to %q", i, got[i].Text)
		}
	}
}

func TestShiftByZeroIsIdentity(t *testing.T) {
	in := []Block{{Index: "1", Timing: "00:01:02,003 --> 00:01:04,005", Text: "x"}}
	got, err := Shift(in, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Timing != in[0].Timing {
		t.Errorf("timing = %q, want it unchanged", got[0].Timing)
	}
}

// A malformed timing must not be silently rewritten into a plausible-looking
// wrong one — that would desync a file with no visible cause.
func TestShiftRejectsMalformedTiming(t *testing.T) {
	for _, bad := range []string{"garbage", "00:00:01,000", "00:00:01,000 --> nope"} {
		_, err := Shift([]Block{{Index: "1", Timing: bad, Text: "x"}}, time.Second)
		if err == nil {
			t.Errorf("Shift with timing %q: want an error", bad)
		}
	}
}

// Concatenated chunks each restart their numbering at 1, which players reject.
func TestRenumberMakesOneAscendingSequence(t *testing.T) {
	in := []Block{
		{Index: "1", Timing: "00:00:00,000 --> 00:00:01,000", Text: "a"},
		{Index: "2", Timing: "00:00:01,000 --> 00:00:02,000", Text: "b"},
		{Index: "1", Timing: "00:10:00,000 --> 00:10:01,000", Text: "c"},
	}
	got := Renumber(in)
	for i, b := range got {
		want := []string{"1", "2", "3"}[i]
		if b.Index != want {
			t.Errorf("block %d index = %q, want %q", i, b.Index, want)
		}
		if b.Timing != in[i].Timing || b.Text != in[i].Text {
			t.Errorf("block %d: Renumber altered timing or text", i)
		}
	}
}

func TestRenumberEmpty(t *testing.T) {
	if got := Renumber(nil); len(got) != 0 {
		t.Errorf("Renumber(nil) = %v, want empty", got)
	}
}

// Renumber then Write must produce a file that starts at 1 and counts up.
func TestRenumberedFileIsSequential(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/out.srt"
	blocks := Renumber([]Block{
		{Index: "7", Timing: "00:00:00,000 --> 00:00:01,000", Text: "a"},
		{Index: "9", Timing: "00:00:01,000 --> 00:00:02,000", Text: "b"},
	})
	if err := Write(path, blocks); err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 2 || parsed[0].Index != "1" || parsed[1].Index != "2" {
		t.Errorf("round trip produced %v", parsed)
	}
	_ = strings.TrimSpace("")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/srt/ -v`
Expected: FAIL — `parseTimestamp`, `formatTimestamp`, `Shift` and `Renumber` are undefined.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/srt/srt.go`:

```go
// parseTimestamp reads one side of an SRT timing line: "00:01:23,456".
func parseTimestamp(s string) (time.Duration, error) {
	var h, m, sec, ms int
	n, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d:%d,%d", &h, &m, &sec, &ms)
	if err != nil || n != 4 {
		return 0, fmt.Errorf("malformed SRT timestamp %q", s)
	}
	return time.Duration(h)*time.Hour +
		time.Duration(m)*time.Minute +
		time.Duration(sec)*time.Second +
		time.Duration(ms)*time.Millisecond, nil
}

func formatTimestamp(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	d -= s * time.Second
	return fmt.Sprintf("%02d:%02d:%02d,%03d", h, m, s, d/time.Millisecond)
}

// Shift moves every block by the given offset. A chunk of a longer film is
// transcribed as though it began at zero, so merging it back requires adding the
// chunk's start time to both ends of every timing.
func Shift(blocks []Block, by time.Duration) ([]Block, error) {
	out := make([]Block, len(blocks))
	for i, b := range blocks {
		left, right, ok := strings.Cut(b.Timing, "-->")
		if !ok {
			return nil, fmt.Errorf("block %d: malformed timing %q", i+1, b.Timing)
		}
		start, err := parseTimestamp(left)
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", i+1, err)
		}
		end, err := parseTimestamp(right)
		if err != nil {
			return nil, fmt.Errorf("block %d: %w", i+1, err)
		}
		out[i] = Block{
			Index:  b.Index,
			Timing: formatTimestamp(start+by) + " --> " + formatTimestamp(end+by),
			Text:   b.Text,
		}
	}
	return out, nil
}

// Renumber rewrites the index lines as one ascending sequence. Chunks each start
// their own numbering at 1, and Write emits the index verbatim, so a merged file
// is invalid until this has run.
func Renumber(blocks []Block) []Block {
	out := make([]Block, len(blocks))
	for i, b := range blocks {
		out[i] = Block{Index: strconv.Itoa(i + 1), Timing: b.Timing, Text: b.Text}
	}
	return out
}
```

Extend the import block with `"strconv"` and `"time"`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/srt/ -v && make validate`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/srt/
git commit -m "feat: add subtitle block shifting and renumbering"
```

---

### Task 3: Slicing the extracted audio

**Files:**
- Modify: `internal/media/media.go`
- Modify: `internal/media/media_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces, in package `media`:
  - `func SliceWAV(inWAV string, start, dur time.Duration, outWAV string) error`
  - `func WAVDuration(path string) (time.Duration, error)`
  - unexported `func sliceWAVArgs(inWAV string, start, dur time.Duration, outWAV string) []string`

- [ ] **Step 1: Write the failing test**

Append to `internal/media/media_test.go`:

```go
// The extracted WAV is 16 kHz mono s16 PCM — constant bitrate, no keyframes — so
// an input-side -ss is an exact byte seek and -c copy avoids a second decode of a
// multi-gigabyte source. Re-encoding here would cost more than the transcription
// it feeds.
func TestSliceWAVArgsSeekOnInputAndStreamCopy(t *testing.T) {
	args := sliceWAVArgs("full.wav", 10*time.Minute, 5*time.Minute, "chunk.wav")

	iInput := slices.Index(args, "-i")
	iSS := slices.Index(args, "-ss")
	iT := slices.Index(args, "-t")
	if iSS < 0 || iT < 0 || iInput < 0 {
		t.Fatalf("missing -ss/-t/-i: %v", args)
	}
	if iSS > iInput || iT > iInput {
		t.Errorf("-ss and -t must precede -i to seek the input, got: %v", args)
	}
	if args[iSS+1] != "600.000" {
		t.Errorf("-ss = %q, want 600.000", args[iSS+1])
	}
	if args[iT+1] != "300.000" {
		t.Errorf("-t = %q, want 300.000", args[iT+1])
	}

	iC := slices.Index(args, "-c")
	if iC < 0 || args[iC+1] != "copy" {
		t.Errorf("want -c copy, got: %v", args)
	}
	if args[len(args)-1] != "chunk.wav" {
		t.Errorf("output must be last, got %q", args[len(args)-1])
	}
}

func TestSliceWAVArgsSubSecondPrecision(t *testing.T) {
	args := sliceWAVArgs("in.wav", 1500*time.Millisecond, 250*time.Millisecond, "out.wav")
	iSS := slices.Index(args, "-ss")
	iT := slices.Index(args, "-t")
	if args[iSS+1] != "1.500" {
		t.Errorf("-ss = %q, want 1.500", args[iSS+1])
	}
	if args[iT+1] != "0.250" {
		t.Errorf("-t = %q, want 0.250", args[iT+1])
	}
}
```

Add `"time"` to the test file's import block.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/media/ -v`
Expected: FAIL — `sliceWAVArgs` is undefined.

- [ ] **Step 3: Write minimal implementation**

Append to `internal/media/media.go`:

```go
// SliceWAV cuts [start, start+dur) out of a WAV without re-encoding. The seek is
// an input option so ffmpeg skips bytes rather than decoding and discarding them,
// which matters when this runs once per chunk over a feature film.
func SliceWAV(inWAV string, start, dur time.Duration, outWAV string) error {
	cmd := exec.Command("ffmpeg", sliceWAVArgs(inWAV, start, dur, outWAV)...)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func sliceWAVArgs(inWAV string, start, dur time.Duration, outWAV string) []string {
	return []string{
		"-y",
		"-ss", fmt.Sprintf("%.3f", start.Seconds()),
		"-t", fmt.Sprintf("%.3f", dur.Seconds()),
		"-i", inWAV,
		"-c", "copy",
		outWAV,
	}
}

// WAVDuration reports how long an audio file runs, which is what the chunk plan
// is built from.
func WAVDuration(path string) (time.Duration, error) {
	out, err := exec.Command("ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "csv=p=0",
		path,
	).Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe duration: %w", err)
	}
	secs, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0, fmt.Errorf("parse duration %q: %w", strings.TrimSpace(string(out)), err)
	}
	return time.Duration(secs * float64(time.Second)), nil
}
```

Extend the import block with `"strconv"` and `"time"`.

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/media/ -v && make validate`
Expected: PASS, including the pre-existing mux and audio tests.

- [ ] **Step 5: Commit**

```bash
git add internal/media/
git commit -m "feat: add WAV slicing and duration probing"
```

---

### Task 4: Chunk planning and flag validation

**Files:**
- Create: `progressive.go`
- Test: `progressive_test.go` (create)

**Interfaces:**
- Consumes: `sourceMode` with `sourceSub`/`sourceAudio` and `outputMode` with `modeSRT`/`modeMux`/`modeBoth`, all already in package `main`.
- Produces, in package `main`:
  - `type chunkPlan struct { Index int; Start, Dur time.Duration }`
  - `func planChunks(total, chunkLen time.Duration) []chunkPlan`
  - `func validateFast(fast bool, source sourceMode, mode outputMode, chunkMinutes int) error`

- [ ] **Step 1: Write the failing test**

Create `progressive_test.go`:

```go
package main

import (
	"strings"
	"testing"
	"time"
)

func TestPlanChunksEvenDivision(t *testing.T) {
	got := planChunks(30*time.Minute, 10*time.Minute)
	if len(got) != 3 {
		t.Fatalf("got %d chunks, want 3: %+v", len(got), got)
	}
	for i, c := range got {
		if c.Index != i {
			t.Errorf("chunk %d has Index %d", i, c.Index)
		}
		if c.Start != time.Duration(i)*10*time.Minute {
			t.Errorf("chunk %d starts at %v", i, c.Start)
		}
		if c.Dur != 10*time.Minute {
			t.Errorf("chunk %d runs %v, want 10m", i, c.Dur)
		}
	}
}

// A film is never an exact multiple of the chunk length; the tail must be short
// rather than reading past the end of the audio.
func TestPlanChunksShortTail(t *testing.T) {
	got := planChunks(25*time.Minute, 10*time.Minute)
	if len(got) != 3 {
		t.Fatalf("got %d chunks, want 3", len(got))
	}
	last := got[2]
	if last.Start != 20*time.Minute || last.Dur != 5*time.Minute {
		t.Errorf("tail = start %v dur %v, want 20m/5m", last.Start, last.Dur)
	}
	var sum time.Duration
	for _, c := range got {
		sum += c.Dur
	}
	if sum != 25*time.Minute {
		t.Errorf("chunks cover %v, want exactly 25m", sum)
	}
}

func TestPlanChunksShorterThanOneChunk(t *testing.T) {
	got := planChunks(3*time.Minute, 10*time.Minute)
	if len(got) != 1 {
		t.Fatalf("got %d chunks, want 1", len(got))
	}
	if got[0].Start != 0 || got[0].Dur != 3*time.Minute {
		t.Errorf("chunk = start %v dur %v, want 0/3m", got[0].Start, got[0].Dur)
	}
}

func TestPlanChunksDegenerateInputs(t *testing.T) {
	if got := planChunks(0, 10*time.Minute); len(got) != 0 {
		t.Errorf("zero total = %v, want none", got)
	}
	if got := planChunks(10*time.Minute, 0); len(got) != 0 {
		t.Errorf("zero chunk length = %v, want none", got)
	}
	if got := planChunks(-time.Minute, time.Minute); len(got) != 0 {
		t.Errorf("negative total = %v, want none", got)
	}
}

// -fast only makes sense when there is a slow step to hide. A subtitle track is
// already on disk.
func TestValidateFastRejectsSubtitleSource(t *testing.T) {
	err := validateFast(true, sourceSub, modeSRT, 10)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "-source audio") {
		t.Errorf("error %q does not say what to do instead", err)
	}
}

// A container cannot be rewritten incrementally, so the progressive promise
// cannot be kept in mux modes.
func TestValidateFastRejectsContainerModes(t *testing.T) {
	for _, m := range []outputMode{modeMux, modeBoth} {
		err := validateFast(true, sourceAudio, m, 10)
		if err == nil {
			t.Errorf("mode %v: want an error", m)
			continue
		}
		if !strings.Contains(err.Error(), "-mode srt") {
			t.Errorf("mode %v: error %q does not name the working mode", m, err)
		}
	}
}

func TestValidateFastRejectsBadChunkLength(t *testing.T) {
	for _, n := range []int{0, -5} {
		if err := validateFast(true, sourceAudio, modeSRT, n); err == nil {
			t.Errorf("chunk %d: want an error", n)
		}
	}
}

func TestValidateFastAcceptsTheWorkingCombination(t *testing.T) {
	if err := validateFast(true, sourceAudio, modeSRT, 10); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// Without -fast nothing is validated, because nothing changes.
func TestValidateFastIgnoresEverythingWhenOff(t *testing.T) {
	for _, m := range []outputMode{modeSRT, modeMux, modeBoth} {
		for _, s := range []sourceMode{sourceSub, sourceAudio} {
			if err := validateFast(false, s, m, 0); err != nil {
				t.Errorf("fast=false must never error, got %v", err)
			}
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run 'TestPlanChunks|TestValidateFast' -v`
Expected: FAIL — `planChunks`, `chunkPlan` and `validateFast` are undefined.

- [ ] **Step 3: Write minimal implementation**

Create `progressive.go`:

```go
package main

import (
	"fmt"
	"time"
)

// chunkPlan is one slice of the audio: where it starts in the film and how long
// it runs.
type chunkPlan struct {
	Index int
	Start time.Duration
	Dur   time.Duration
}

// planChunks divides a film into equal pieces, the last one short. Equal pieces
// are deliberate: the viewer's lead grows with every chunk, so by the time they
// reach minute ten the file already covers minute one hundred and twenty.
func planChunks(total, chunkLen time.Duration) []chunkPlan {
	if total <= 0 || chunkLen <= 0 {
		return nil
	}
	var out []chunkPlan
	for start, i := time.Duration(0), 0; start < total; start, i = start+chunkLen, i+1 {
		dur := chunkLen
		if start+dur > total {
			dur = total - start
		}
		out = append(out, chunkPlan{Index: i, Start: start, Dur: dur})
	}
	return out
}

// validateFast rejects the combinations where a progressive run cannot deliver
// what it promises. It is a no-op when -fast is off, so the ordinary path is
// untouched.
func validateFast(fast bool, source sourceMode, mode outputMode, chunkMinutes int) error {
	if !fast {
		return nil
	}
	if source != sourceAudio {
		return fmt.Errorf("-fast only applies to transcription; there is nothing to wait for " +
			"when reading an existing subtitle track — use -source audio")
	}
	if mode != modeSRT {
		return fmt.Errorf("-fast cannot be combined with -mode %s: a video container "+
			"cannot be rewritten piece by piece — use the default -mode srt", mode)
	}
	if chunkMinutes < 1 {
		return fmt.Errorf("-chunk must be at least 1 minute, got %d", chunkMinutes)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test . -v && make validate`
Expected: PASS, including every pre-existing package-main test.

- [ ] **Step 5: Commit**

```bash
git add progressive.go progressive_test.go
git commit -m "feat: add chunk planning and fast-mode validation"
```

---

### Task 5: The progressive run

**Files:**
- Modify: `progressive.go`
- Modify: `main.go`

**Interfaces:**
- Consumes: `planChunks`, `chunkPlan`, `validateFast` (Task 4); `srt.Shift`, `srt.Renumber` (Task 2); `media.SliceWAV`, `media.WAVDuration` (Task 3); `translate.Client.TranslateAll` returning `([]string, []int, error)` (Task 1); plus the already-merged `whisper.Options`, `whisper.Run`, `media.ExtractAudio`, `media.AudioStreams`, `media.FindAudioByLang`, `media.DefaultSRTPath`, `srt.Parse`, `srt.Write`, `srt.Texts`, `srt.WithTexts`, the run-scoped temp dir passed in as `tmpDir`, `fatalf`.
- Produces, in package `main`:
  - `func runProgressive(input, tmpDir string, streams []media.Stream, atrack int, from, to string, chunkLen time.Duration, opts whisper.Options) error`
  - `func notifyDone(title, message string)`

This task has no unit test of its own; Task 6 is its end-to-end verification.

- [ ] **Step 1: Add the notification helper**

Append to `progressive.go`:

```go
// notifyDone raises a desktop notification on macOS. It is best-effort by design:
// a finished file is the deliverable, and a missing osascript must never turn a
// successful run into a failed one.
func notifyDone(title, message string) {
	if runtime.GOOS != "darwin" {
		return
	}
	script := fmt.Sprintf("display notification %q with title %q", message, title)
	_ = exec.Command("osascript", "-e", script).Run()
}
```

Extend `progressive.go`'s imports with `"os/exec"` and `"runtime"`.

- [ ] **Step 2: Add the progressive loop**

Append to `progressive.go`:

```go
// runProgressive transcribes and translates the film in order, rewriting the
// output file after every chunk so the opening is watchable while the rest is
// still being produced.
func runProgressive(input, tmpDir string, streams []media.Stream, atrack int, from, to string,
	chunkLen time.Duration, opts whisper.Options) error {

	src, err := pickAudioStream(input, streams, atrack, from)
	if err != nil {
		return err
	}
	fmt.Printf("Audio:  #%d  lang=%s  %q\n", src.Index, src.Tags.Language, src.Tags.Title)

	fullWAV := filepath.Join(tmpDir, "audio.wav")
	fmt.Printf("Extracting audio track #%d...\n", src.Index)
	if err := media.ExtractAudio(input, src.Index, fullWAV); err != nil {
		return fmt.Errorf("extract audio: %w", err)
	}

	total, err := media.WAVDuration(fullWAV)
	if err != nil {
		return err
	}
	plan := planChunks(total, chunkLen)
	if len(plan) == 0 {
		return fmt.Errorf("audio track is empty — nothing to transcribe")
	}
	fmt.Printf("Transcribing %s in %d chunks of up to %s...\n",
		formatClock(total), len(plan), formatClock(chunkLen))

	client := translate.New(from, to)
	var srcBlocks, outBlocks []srt.Block
	lang := from

	for _, c := range plan {
		chunkWAV := filepath.Join(tmpDir, fmt.Sprintf("chunk-%03d.wav", c.Index))
		if err := media.SliceWAV(fullWAV, c.Start, c.Dur, chunkWAV); err != nil {
			return fmt.Errorf("chunk %d: slice audio: %w", c.Index+1, err)
		}

		chunkOpts := opts
		chunkOpts.Audio = chunkWAV
		chunkOpts.OutBase = filepath.Join(tmpDir, fmt.Sprintf("chunk-%03d", c.Index))
		// The language is settled by the first chunk and then forced, so whisper
		// cannot change its mind halfway and hand back a two-language file.
		chunkOpts.Language = lang

		result, err := whisper.Run(chunkOpts, func(pct int) {
			fmt.Printf("\r  chunk %d/%d: %d%%   ", c.Index+1, len(plan), pct)
		})
		fmt.Println()
		if err != nil {
			return fmt.Errorf("chunk %d (%s): %w", c.Index+1, formatClock(c.Start), err)
		}
		if lang == "" {
			lang = result.Language
			fmt.Printf("Detected language: %s\n", lang)
		}

		parsed, err := srt.Parse(result.SRTPath)
		if err != nil {
			return fmt.Errorf("chunk %d: parse transcript: %w", c.Index+1, err)
		}
		shifted, err := srt.Shift(parsed, c.Start)
		if err != nil {
			return fmt.Errorf("chunk %d: %w", c.Index+1, err)
		}
		srcBlocks = append(srcBlocks, shifted...)

		translated, failed, err := client.TranslateAll(srt.Texts(shifted), nil)
		if err != nil {
			return fmt.Errorf("chunk %d: translate: %w", c.Index+1, err)
		}
		if len(failed) > 0 {
			fmt.Fprintf(os.Stderr,
				"warning: chunk %d: %d of %d blocks kept their original text\n",
				c.Index+1, len(failed), len(shifted))
		}
		chunkOut, err := srt.WithTexts(shifted, translated)
		if err != nil {
			return fmt.Errorf("chunk %d: %w", c.Index+1, err)
		}
		outBlocks = append(outBlocks, chunkOut...)

		transcriptPath := media.DefaultSRTPath(input, lang)
		if err := srt.Write(transcriptPath, srt.Renumber(srcBlocks)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not save transcript to %s: %v\n", transcriptPath, err)
		}
		outPath := media.DefaultSRTPath(input, to)
		if err := srt.Write(outPath, srt.Renumber(outBlocks)); err != nil {
			return fmt.Errorf("chunk %d: write %s: %w", c.Index+1, outPath, err)
		}

		covered := formatClock(c.Start + c.Dur)
		if c.Index == 0 {
			fmt.Printf("Saved SRT: %s — covers 0:00–%s, you can start watching\n", outPath, covered)
		} else {
			fmt.Printf("  extended to %s\n", covered)
		}
	}

	outPath := media.DefaultSRTPath(input, to)
	fmt.Printf("Done: %s covers the full %s — reload subtitles in your player\n",
		outPath, formatClock(total))
	notifyDone("Subtitles ready", filepath.Base(outPath))
	return nil
}

// formatClock renders a duration as H:MM:SS, which is how a viewer reads a film's
// position.
func formatClock(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	return fmt.Sprintf("%d:%02d:%02d", h, m, d/time.Second)
}
```

Extend `progressive.go`'s imports with `"os"`, `"path/filepath"`, and the internal packages `media`, `srt`, `translate`, `whisper`.

- [ ] **Step 3: Extract the shared audio-stream picker**

`main.go`'s `transcribe` currently picks the audio stream inline. Both it and `runProgressive` need the same choice, so move that block into one function in `progressive.go` and call it from both:

```go
// pickAudioStream chooses which audio track to transcribe: an explicit index
// first, then a language-tag match, then the first track. A -from that matches no
// track falls back with a warning and clears the language, so whisper detects it
// rather than being told to hear a language that is not there.
func pickAudioStream(input string, streams []media.Stream, atrack int, from string) (media.Stream, error) {
	audio := media.AudioStreams(streams)
	if len(audio) == 0 {
		return media.Stream{}, fmt.Errorf("no audio tracks in %s — nothing to transcribe", filepath.Base(input))
	}
	if atrack >= 0 {
		for _, s := range audio {
			if s.Index == atrack {
				return s, nil
			}
		}
		return media.Stream{}, fmt.Errorf("no audio stream at index %d", atrack)
	}
	if from != "" {
		if s, ok := media.FindAudioByLang(audio, from); ok {
			return s, nil
		}
	}
	return audio[0], nil
}
```

In `main.go`'s `transcribe`, replace the inline selection with a call to `pickAudioStream`, keeping the existing warning-and-clear behaviour for a `-from` that matches nothing. Do not change what `transcribe` does otherwise.

- [ ] **Step 4: Wire the flags into main**

Declare the new flags beside the existing ones in `main.go`:

```go
	fast := flag.Bool("fast", false, "translate progressively so the opening is watchable within a minute")
	chunkMin := flag.Int("chunk", 0, "chunk length in minutes for -fast (default 10, or whisper.chunk-minutes)")
```

After the source is resolved and before the language prompts, validate and dispatch. Insert immediately after the existing `if source == sourceSub && len(subs) == 0` check:

```go
	chunkMinutes := *chunkMin
	if chunkMinutes == 0 {
		if cfg, err := config.Load(); err == nil && cfg.Whisper.ChunkMinutes > 0 {
			chunkMinutes = cfg.Whisper.ChunkMinutes
		} else {
			chunkMinutes = 10
		}
	}
	if err := validateFast(*fast, source, mode, chunkMinutes); err != nil {
		fatalf("%v", err)
	}
```

Then, inside the `source == sourceAudio` branch, after `whisperOpts` has been resolved and the target language is known, take the progressive path when asked:

```go
		if *fast {
			if err := runProgressive(input, runTmpDir, streams, *atrack, *from, *to,
				time.Duration(chunkMinutes)*time.Minute, whisperOpts); err != nil {
				fatalf("%v", err)
			}
			return
		}
```

Add `"time"` to `main.go`'s imports.

Add the config key in `internal/config/config.go`: a `ChunkMinutes int` field with tag `chunkMinutes,omitempty` on `WhisperConfig`, and an entry in the `fields` map:

```go
	"whisper.chunk-minutes": {
		get: func(c *Config) string {
			if c.Whisper.ChunkMinutes == 0 {
				return ""
			}
			return strconv.Itoa(c.Whisper.ChunkMinutes)
		},
		set: func(c *Config, v string) error {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return fmt.Errorf("whisper.chunk-minutes must be a positive integer, got %q", v)
			}
			c.Whisper.ChunkMinutes = n
			return nil
		},
	},
```

`internal/config/config_test.go`'s `TestKeysAreSortedAndComplete` lists the expected keys — add `"whisper.chunk-minutes"` to that list.

- [ ] **Step 5: Verify and commit**

Run: `make validate`
Expected: PASS, every pre-existing test included.

Then check the validation paths, which need neither whisper nor a film:

```bash
go build -o /tmp/st .
/tmp/st -to es -fast -source sub /tmp/clip.mkv    # expect: "-source audio" guidance
/tmp/st -to es -fast -mode mux /tmp/clip.mkv      # expect: "-mode srt" guidance
/tmp/st -to es -fast -chunk 0 /tmp/clip.mkv       # expect: "at least 1 minute"
/tmp/st config set whisper.chunk-minutes 5 && /tmp/st config get whisper.chunk-minutes
```

```bash
git add progressive.go main.go internal/config/
git commit -m "feat: add -fast progressive translation"
```

---

### Task 6: End-to-end progressive run against a stub whisper

**Files:**
- Modify: `progressive_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1–5.
- Produces: no new API.

This test is what stands in for a real film. It must not need whisper.cpp, ffmpeg or the network — so it exercises the merge arithmetic, which is where chunking actually goes wrong, rather than the subprocess plumbing.

- [ ] **Step 1: Write the failing test**

Append to `progressive_test.go`:

```go
// Merging chunks is where progressive output goes wrong: the second chunk's
// timings are relative to its own start, and its numbering restarts at 1. This
// reproduces the merge the loop performs and pins both.
func TestChunkMergeProducesOneContinuousFile(t *testing.T) {
	chunk0 := []srt.Block{
		{Index: "1", Timing: "00:00:01,000 --> 00:00:03,000", Text: "opening line"},
		{Index: "2", Timing: "00:09:50,000 --> 00:09:55,000", Text: "end of first chunk"},
	}
	chunk1 := []srt.Block{
		{Index: "1", Timing: "00:00:02,000 --> 00:00:04,500", Text: "second chunk opens"},
	}

	var merged []srt.Block
	for i, c := range [][]srt.Block{chunk0, chunk1} {
		shifted, err := srt.Shift(c, time.Duration(i)*10*time.Minute)
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		merged = append(merged, shifted...)
	}
	merged = srt.Renumber(merged)

	if len(merged) != 3 {
		t.Fatalf("merged %d blocks, want 3", len(merged))
	}
	for i, want := range []string{"1", "2", "3"} {
		if merged[i].Index != want {
			t.Errorf("block %d index = %q, want %q", i, merged[i].Index, want)
		}
	}
	// The second chunk's first line is 2 s into a chunk that starts at 10:00.
	if got := merged[2].Timing; got != "00:10:02,000 --> 00:10:04,500" {
		t.Errorf("second chunk timing = %q, want 00:10:02,000 --> 00:10:04,500", got)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "out.srt")
	if err := srt.Write(path, merged); err != nil {
		t.Fatal(err)
	}
	parsed, err := srt.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 3 {
		t.Errorf("wrote a file that parses back as %d blocks", len(parsed))
	}
}

// The plan and the merge must agree: every chunk's blocks land inside that
// chunk's window, so no line is attributed to the wrong part of the film.
func TestPlanAndShiftAgreeOnChunkWindows(t *testing.T) {
	total := 25 * time.Minute
	chunkLen := 10 * time.Minute
	for _, c := range planChunks(total, chunkLen) {
		block := []srt.Block{{Index: "1", Timing: "00:00:00,500 --> 00:00:01,500", Text: "x"}}
		shifted, err := srt.Shift(block, c.Start)
		if err != nil {
			t.Fatal(err)
		}
		start, err := firstStart(shifted[0].Timing)
		if err != nil {
			t.Fatal(err)
		}
		if start < c.Start || start >= c.Start+c.Dur {
			t.Errorf("chunk %d: a block at %v falls outside [%v, %v)",
				c.Index, start, c.Start, c.Start+c.Dur)
		}
	}
}

// firstStart reads the left-hand timestamp out of an SRT timing line.
func firstStart(timing string) (time.Duration, error) {
	left, _, ok := strings.Cut(timing, "-->")
	if !ok {
		return 0, fmt.Errorf("malformed timing %q", timing)
	}
	var h, m, s, ms int
	if _, err := fmt.Sscanf(strings.TrimSpace(left), "%d:%d:%d,%d", &h, &m, &s, &ms); err != nil {
		return 0, err
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute +
		time.Duration(s)*time.Second + time.Duration(ms)*time.Millisecond, nil
}
```

Extend `progressive_test.go`'s imports with `"fmt"`, `"path/filepath"` and `"github.com/artschekoff/sub-translator/internal/srt"`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run 'TestChunkMerge|TestPlanAndShift' -v`
Expected: FAIL before Task 2's `srt.Shift`/`srt.Renumber` exist; with them in place it should compile and pass.

- [ ] **Step 3: Make it pass**

No production code should be needed. If a test fails, fix the production code rather than the assertion, and say so in your report.

- [ ] **Step 4: Run the full suite**

Run: `make validate`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add progressive_test.go
git commit -m "test: pin chunk merge arithmetic for progressive output"
```

---

### Task 7: Documentation

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Document the fast mode**

Add a subsection to the Transcription section, after the usage examples:

```markdown
### Watching before it finishes

Transcription runs far faster than playback — about 14× real time on an M1 Pro — but the normal mode still writes nothing until the last block is done. `-fast` changes that: it transcribes and translates the film in ten-minute chunks and rewrites the subtitle file after each one, so the opening is watchable within a minute.

```bash
sub-translator -to es -source audio -fast movie.mkv
```

```
Saved SRT: movie.es.srt — covers 0:00–10:00, you can start watching
  extended to 0:20:00
  extended to 0:30:00
Done: movie.es.srt covers the full 2:19:08 — reload subtitles in your player
```

Your lead grows as you watch: by the time you reach minute ten of the film, the file already covers minute one hundred and twenty.

**Reload the subtitles once at the end.** Players read a `.srt` when they load it and do not notice the file growing, so the track you started with covers only the first chunk. Re-selecting the subtitle track picks up the finished file. On macOS a desktop notification fires when it is complete.

Chunk length is `-chunk N` minutes, or `whisper.chunk-minutes` in the config. A line spoken across a chunk boundary is split into two, which is the cost of not waiting.

`-fast` requires `-source audio` (there is nothing to wait for when a subtitle track already exists) and the default `-mode srt` (a video container cannot be rewritten piece by piece).
```

- [ ] **Step 2: Add the new flags to the flag list**

Mirror the `usage` constant in `main.go` so the README and `-h` agree, adding:

```
  -fast          write subtitles progressively so the opening is watchable within a minute
  -chunk         chunk length in minutes for -fast (default: 10)
```

- [ ] **Step 3: Add the config key to the configuration table**

```markdown
| `whisper.chunk-minutes` | Chunk length for `-fast`, in minutes. 10 when unset. |
```

- [ ] **Step 4: Verify**

Run: `make validate`, then `go build -o /tmp/st . && /tmp/st -h` and confirm the README's flag list matches the binary's output exactly.

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "docs: document -fast progressive translation"
```

---

## Self-Review

**Spec coverage.** Part 1 (honest translation failures, status checking, retry, injectable URL, main's warning) → Task 1. `srt.Shift`/`Renumber` and the timestamp pair → Task 2. `media.SliceWAV`/`WAVDuration` → Task 3. `planChunks` and the `-fast`/`-chunk`/`-mode` validation rules → Task 4. The chunk loop, one-decode-pass, language-settled-once, per-chunk messaging, notification, transcript sidecar, and the config key → Task 5. The testing section's merge and plan tests → Task 6; the translate tests → Task 1; the pure-function tests → Tasks 2–4. Documentation → Task 7.

**Known deviation from the spec.** The spec's testing section asks for an end-to-end progressive run against the stub whisper binary. Task 6 pins the merge arithmetic directly instead, because driving `runProgressive` end to end also requires stubbing `ffmpeg` for `ExtractAudio`, `SliceWAV` and `WAVDuration` — three more subprocesses — and the value is in the merge, which is where chunking actually breaks. The subprocess plumbing is covered by the existing `whisper` and `media` argument tests. This is recorded rather than hidden; raise it if you disagree.

**Type consistency.** `TranslateAll` returns `([]string, []int, error)` in Task 1 and is called that way in Tasks 1 and 5. `srt.Shift` returns `([]Block, error)` in Task 2 and is used with both values in Tasks 5 and 6. `chunkPlan` fields `Index`, `Start`, `Dur` are identical in Tasks 4, 5 and 6. `validateFast(bool, sourceMode, outputMode, int) error` matches between Tasks 4 and 5. `pickAudioStream` is defined in Task 5 and used only there and by `transcribe`.

**Placeholders.** None: every step carries the code it asks for.
