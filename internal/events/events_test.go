package events

import (
	"bytes"
	"encoding/json"
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
