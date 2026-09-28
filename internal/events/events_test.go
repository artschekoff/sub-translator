package events

import (
	"bytes"
	"encoding/json"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixed lets the tests assert an exact timestamp instead of matching a pattern.
func fixed(e *Emitter) *Emitter {
	e.now = func() time.Time {
		return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	}
	return e
}

func decode(t *testing.T, line string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("line %q is not JSON: %v", line, err)
	}
	return m
}

// Every event carries its type and a timestamp, because a consumer switches on
// the one and orders by the other.
func TestEmitWritesTypeAndTimestamp(t *testing.T) {
	var buf bytes.Buffer
	e := fixed(New(&buf))
	e.Emit(&Progress{Pct: 42})

	m := decode(t, strings.TrimSpace(buf.String()))
	if m["type"] != string(KindProgress) {
		t.Errorf("type = %v, want %q", m["type"], KindProgress)
	}
	if m["t"] != "2026-09-26T12:00:00Z" {
		t.Errorf("t = %v, want an RFC 3339 timestamp", m["t"])
	}
	if m["pct"] != float64(42) {
		t.Errorf("pct = %v, want 42", m["pct"])
	}
}

// One event per line, so a consumer can read with a line scanner.
func TestEmitWritesOneLinePerEvent(t *testing.T) {
	var buf bytes.Buffer
	e := fixed(New(&buf))
	e.Emit(&Stage{Name: "extract"})
	e.Emit(&Progress{Pct: 10})
	e.Emit(&Done{Path: "/tmp/movie.es.srt", Covers: "2:19:08"})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), buf.String())
	}
	for i, want := range []Kind{KindStage, KindProgress, KindDone} {
		if got := decode(t, lines[i])["type"]; got != string(want) {
			t.Errorf("line %d type = %v, want %q", i, got, want)
		}
	}
}

// The emitter is constructed once and used from every path, including the ones
// that never run under -events json. Both a nil receiver and a nil writer must
// be harmless, or every call site needs a guard.
func TestEmitIsSafeWhenDisabled(t *testing.T) {
	var nilEmitter *Emitter
	nilEmitter.Emit(&Progress{Pct: 1}) // must not panic

	disabled := New(nil)
	disabled.Emit(&Progress{Pct: 1}) // must not panic
}

// Translation runs several workers; their progress callbacks reach the emitter
// concurrently. Interleaved writes would produce unparseable lines.
func TestEmitIsSafeUnderConcurrency(t *testing.T) {
	var buf bytes.Buffer
	e := fixed(New(&buf))

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			e.Emit(&Progress{Pct: n})
		}(i)
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 50 {
		t.Fatalf("got %d lines, want 50", len(lines))
	}
	for i, l := range lines {
		if m := decode(t, l); m["type"] != string(KindProgress) {
			t.Errorf("line %d is not a whole event: %q", i, l)
		}
	}
}

// The probe event is what the GUI builds its track list from, so its shape is
// the one worth pinning exactly.
func TestProbeEventShape(t *testing.T) {
	var buf bytes.Buffer
	e := fixed(New(&buf))
	e.Emit(&Probe{
		Duration:  8348.674,
		Audio:     []Stream{{Index: 3, Lang: "eng", Title: "Original (Eng)", Codec: "ac3"}},
		Subtitles: []Stream{},
	})

	m := decode(t, strings.TrimSpace(buf.String()))
	audio, ok := m["audio"].([]any)
	if !ok || len(audio) != 1 {
		t.Fatalf("audio = %#v, want one stream", m["audio"])
	}
	s := audio[0].(map[string]any)
	for k, want := range map[string]any{
		"index": float64(3), "lang": "eng", "title": "Original (Eng)", "codec": "ac3",
	} {
		if s[k] != want {
			t.Errorf("audio[0].%s = %v, want %v", k, s[k], want)
		}
	}
	if subs, ok := m["subtitles"].([]any); !ok || len(subs) != 0 {
		t.Errorf("subtitles = %#v, want an empty list, not null", m["subtitles"])
	}
}

func TestVersionEventCarriesTheProtocolNumber(t *testing.T) {
	var buf bytes.Buffer
	e := fixed(New(&buf))
	e.Emit(&Version{Events: ProtocolVersion, Tool: "1.4.0"})

	m := decode(t, strings.TrimSpace(buf.String()))
	if m["events"] != float64(ProtocolVersion) {
		t.Errorf("events = %v, want %d", m["events"], ProtocolVersion)
	}
	if m["tool"] != "1.4.0" {
		t.Errorf("tool = %v, want 1.4.0", m["tool"])
	}
}

// A consumer distinguishes a run that lost blocks from one that did not, so the
// warning's numbers have to survive encoding.
func TestWarningEventShape(t *testing.T) {
	var buf bytes.Buffer
	e := fixed(New(&buf))
	e.Emit(&Warning{Kind: "blocks_untranslated", Count: 12, Total: 1834, First: []int{204, 205, 206}})

	m := decode(t, strings.TrimSpace(buf.String()))
	if m["kind"] != "blocks_untranslated" || m["count"] != float64(12) || m["total"] != float64(1834) {
		t.Errorf("warning = %#v", m)
	}
	first, ok := m["first"].([]any)
	if !ok || len(first) != 3 || first[0] != float64(204) {
		t.Errorf("first = %#v, want [204 205 206]", m["first"])
	}
}

// Optional fields must be absent rather than present-and-empty, so a consumer
// can tell "no chunk" from "chunk zero".
func TestStageOmitsChunkFieldsWhenThereAreNone(t *testing.T) {
	var buf bytes.Buffer
	e := fixed(New(&buf))
	e.Emit(&Stage{Name: "extract"})

	m := decode(t, strings.TrimSpace(buf.String()))
	for _, k := range []string{"chunk", "of", "from", "to"} {
		if _, present := m[k]; present {
			t.Errorf("key %q should be omitted for a stage with no chunks", k)
		}
	}
}

// The ordinary run translates whatever it parsed in one pass, not a span of
// the container's own runtime, so it has no honest value for Covers — an
// absent key means "unknown", which is the only truthful thing to say. The
// -fast path does know a span, chunk by chunk, and must still send it.
func TestDoneOmitsCoversWhenUnknownButCarriesItWhenSet(t *testing.T) {
	var buf bytes.Buffer
	e := fixed(New(&buf))
	e.Emit(&Done{Path: "/tmp/movie.es.srt"})
	e.Emit(&Done{Path: "/tmp/movie.es.srt", Covers: "2:19:08"})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), buf.String())
	}

	unknown := decode(t, lines[0])
	if _, present := unknown["covers"]; present {
		t.Errorf(`"covers" = %v, want the key absent when Covers is unset`, unknown["covers"])
	}

	known := decode(t, lines[1])
	if known["covers"] != "2:19:08" {
		t.Errorf(`"covers" = %v, want "2:19:08" when Covers is set`, known["covers"])
	}
}

// byteAtATimeWriter writes its input one byte at a time, yielding between them.
// A real file descriptor behaves this way under load: a write is not atomic, so
// two unsynchronised writers interleave inside a line. The per-byte lock makes
// this writer itself safe to call concurrently, so the only thing the test can
// fail on is the interleaving the shared lock exists to prevent.
type byteAtATimeWriter struct {
	mu  sync.Mutex
	buf []byte
}

func (w *byteAtATimeWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		w.mu.Lock()
		w.buf = append(w.buf, b)
		w.mu.Unlock()
		runtime.Gosched()
	}
	return len(p), nil
}

func (w *byteAtATimeWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.buf)
}

// The events and the tool's own prose share one file descriptor by design, and
// they are written from different goroutines: the SIGINT handler emits while the
// main goroutine may be halfway through a warning line. Handing both the same
// SyncWriter is what keeps every line whole — a consumer's rule is "skip the
// lines that are not JSON", which only works if a prose line and an event are
// never the same line.
func TestProseAndEventsNeverLandInsideEachOther(t *testing.T) {
	const prose = "warning: 12 of 1834 blocks could not be translated\n"
	out := &byteAtATimeWriter{}
	shared := NewSyncWriter(out)
	e := fixed(New(shared))

	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.Emit(&Error{Kind: "cancelled", Message: "interrupted"})
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			io.WriteString(shared, prose)
		}()
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 80 {
		t.Fatalf("got %d lines, want 80:\n%s", len(lines), out.String())
	}
	for i, l := range lines {
		if l+"\n" == prose {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("line %d is neither a whole prose line nor a whole event: %q", i, l)
		}
		if m["type"] != string(KindError) {
			t.Errorf("line %d = %q, want an error event", i, l)
		}
	}
}

// The lock makes each write atomic; it says nothing about column position. The
// two interactive prompts end without a newline on purpose — the answer is typed
// on the same line — so an event emitted while one is on screen used to begin
// mid-line and could not be parsed. Same failure as the ffmpeg one, in our own
// code, on the same Cancel path.
func TestAnEventEmittedMidLineStartsOnALineOfItsOwn(t *testing.T) {
	for _, tt := range []struct {
		name  string
		prose string
	}{
		{
			name:  "after a prompt with no newline",
			prose: "Target language code (e.g. en, ru, es, fr): ",
		},
		{
			// A carriage return is not a line start: a reader splitting on
			// newlines still sees what follows as part of that line.
			name:  "after a carriage-return progress fragment",
			prose: "size=    7168KiB time=00:00:57.32 speed=  57x    \r",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			shared := NewSyncWriter(&buf)
			e := fixed(New(shared))

			io.WriteString(shared, tt.prose)
			e.Emit(&Error{Kind: "cancelled", Message: "interrupted"})

			lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
			if len(lines) != 2 {
				t.Fatalf("got %d lines, want the prose and the event on separate lines:\n%q",
					len(lines), buf.String())
			}
			if lines[0] != strings.TrimSuffix(tt.prose, "\n") {
				t.Errorf("first line = %q, want the prose unchanged", lines[0])
			}
			m := decode(t, lines[1])
			if m["type"] != string(KindError) || m["kind"] != "cancelled" {
				t.Errorf("second line = %q, want the whole cancelled event", lines[1])
			}
		})
	}
}

// The line break is only for a stream that is mid-line: a run of events must not
// grow a blank line between every pair, and prose that ended properly must not
// gain one either.
func TestNoLineIsBrokenWhenTheStreamIsAlreadyAtTheStartOfOne(t *testing.T) {
	var buf bytes.Buffer
	shared := NewSyncWriter(&buf)
	e := fixed(New(shared))

	// Nothing written yet: the very first event must not be preceded by a newline.
	e.Emit(&Progress{Pct: 1})
	e.Emit(&Progress{Pct: 2})
	io.WriteString(shared, "warning: something worth saying\n")
	e.Emit(&Progress{Pct: 3})

	if strings.Contains(buf.String(), "\n\n") {
		t.Errorf("a blank line was inserted where the stream was already at a line start:\n%q", buf.String())
	}
	if strings.HasPrefix(buf.String(), "\n") {
		t.Errorf("the first event was preceded by a newline:\n%q", buf.String())
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4:\n%q", len(lines), buf.String())
	}
}
