package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestDecodeSkipsProseAndReadsEvents(t *testing.T) {
	in := strings.Join([]string{
		`{"type":"version","t":"2026-09-29T01:00:00Z","events":1,"tool":"1.4.0"}`,
		`Extracting audio track #1...`,                       // prose: skipped
		`warning: 12 of 1834 blocks could not be translated`, // prose: skipped
		`{"type":"progress","t":"2026-09-29T01:00:01Z","pct":42}`,
		``, // blank: skipped
		`{"type":"done","t":"2026-09-29T01:00:02Z","path":"/tmp/film.es.srt"}`,
	}, "\n")

	var got []Event
	err := Decode(strings.NewReader(in), func(e Event) error {
		got = append(got, e)
		return nil
	})
	if err != nil {
		t.Fatalf("Decode returned an error: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(got), got)
	}

	wantTypes := []string{"version", "progress", "done"}
	for i, want := range wantTypes {
		if got[i].Type != want {
			t.Errorf("event %d: Type = %q, want %q", i, got[i].Type, want)
		}
	}

	done, err := got[2].AsDone()
	if err != nil {
		t.Fatalf("AsDone: %v", err)
	}
	if done.Path != "/tmp/film.es.srt" {
		t.Errorf("Done.Path = %q, want /tmp/film.es.srt", done.Path)
	}
}

// TestDecodeFailsOnAMalformedJSONLine is the rule the CLI's own guarantees
// earn us: it promises every line is whole and that an event never starts
// mid-line, so a '{'-leading line that does not parse is a bug to surface,
// not skip.
func TestDecodeFailsOnAMalformedJSONLine(t *testing.T) {
	in := `{"type":"done"`

	callbackCalled := false
	err := Decode(strings.NewReader(in), func(e Event) error {
		callbackCalled = true
		return nil
	})

	if err == nil {
		t.Fatal("Decode returned nil error for a malformed JSON line, want an error")
	}
	if !strings.Contains(err.Error(), `{"type":"done"`) {
		t.Errorf("error %q does not name the offending line", err.Error())
	}
	if callbackCalled {
		t.Error("on callback was invoked for a line that failed to parse")
	}
}

// TestDecodeHandlesALineLongerThanTheDefaultScannerBuffer proves the decoder
// raises bufio.Scanner's 64 KB default; without that, a warning event
// carrying tens of thousands of "first" indices would make Scan report
// bufio.ErrTooLong instead of the event.
func TestDecodeHandlesALineLongerThanTheDefaultScannerBuffer(t *testing.T) {
	first := make([]int, 50000)
	for i := range first {
		first[i] = i
	}
	line, err := json.Marshal(struct {
		Type  string `json:"type"`
		T     string `json:"t"`
		Kind  string `json:"kind"`
		Count int    `json:"count"`
		Total int    `json:"total"`
		First []int  `json:"first"`
	}{"warning", "2026-09-29T01:00:00Z", "untranslated", len(first), 100000, first})
	if err != nil {
		t.Fatalf("building fixture line: %v", err)
	}
	if len(line) <= 64*1024 {
		t.Fatalf("fixture line is only %d bytes, too small to exercise the buffer bump", len(line))
	}

	var got []Event
	err = Decode(bytes.NewReader(line), func(e Event) error {
		got = append(got, e)
		return nil
	})
	if err != nil {
		t.Fatalf("Decode returned an error on a long-but-valid line: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}

	w, err := got[0].AsWarning()
	if err != nil {
		t.Fatalf("AsWarning: %v", err)
	}
	if len(w.First) != 50000 {
		t.Errorf("Warning.First has %d entries, want 50000", len(w.First))
	}
}

// TestDecodeReadsTheFastRuleAndTheEmbedFlag asserts EmbedsSubtitles is
// non-nil and false when the probe line says so — not the zero value of an
// unset pointer, which would also be nil-looking if the decoder collapsed
// false and absent together.
func TestDecodeReadsTheFastRuleAndTheEmbedFlag(t *testing.T) {
	in := `{"type":"probe","t":"2026-09-29T01:00:00Z","audio":[{"index":0,"lang":"es","title":"","codec":"aac"}],"subtitles":[],"fast":{"sources":["embedded"],"modes":["sidecar","mux"],"min_chunk_minutes":5},"embeds_subtitles":false}`

	var got *Probe
	err := Decode(strings.NewReader(in), func(e Event) error {
		p, err := e.AsProbe()
		if err != nil {
			return err
		}
		got = p
		return nil
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got == nil {
		t.Fatal("no probe event decoded")
	}

	if got.Fast == nil {
		t.Fatal("Probe.Fast is nil, want a FastRule")
	}
	if len(got.Fast.Sources) != 1 || got.Fast.Sources[0] != "embedded" {
		t.Errorf("Fast.Sources = %v, want [embedded]", got.Fast.Sources)
	}
	if len(got.Fast.Modes) != 2 || got.Fast.Modes[0] != "sidecar" || got.Fast.Modes[1] != "mux" {
		t.Errorf("Fast.Modes = %v, want [sidecar mux]", got.Fast.Modes)
	}
	if got.Fast.MinChunkMinutes != 5 {
		t.Errorf("Fast.MinChunkMinutes = %d, want 5", got.Fast.MinChunkMinutes)
	}

	if got.EmbedsSubtitles == nil {
		t.Fatal("Probe.EmbedsSubtitles is nil, want a non-nil false")
	}
	if *got.EmbedsSubtitles != false {
		t.Errorf("*Probe.EmbedsSubtitles = %v, want false", *got.EmbedsSubtitles)
	}
}

// TestDecodeDistinguishesAnAbsentEmbedFlagFromFalse is why the field is a
// *bool: false means "this container cannot take a track", nil means
// "nobody said", and a plain bool conflates them.
func TestDecodeDistinguishesAnAbsentEmbedFlagFromFalse(t *testing.T) {
	in := `{"type":"probe","t":"2026-09-29T01:00:00Z","audio":[],"subtitles":[]}`

	var got *Probe
	err := Decode(strings.NewReader(in), func(e Event) error {
		p, err := e.AsProbe()
		if err != nil {
			return err
		}
		got = p
		return nil
	})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got == nil {
		t.Fatal("no probe event decoded")
	}

	if got.EmbedsSubtitles != nil {
		t.Errorf("Probe.EmbedsSubtitles = %v, want nil for an absent field", *got.EmbedsSubtitles)
	}
}

// TestDecodeStopsOnACallbackError checks the other documented half of
// Decode's contract: a callback error stops the decode and is returned,
// and no further lines are processed after it.
func TestDecodeStopsOnACallbackError(t *testing.T) {
	in := strings.Join([]string{
		`{"type":"progress","t":"x","pct":1}`,
		`{"type":"progress","t":"x","pct":2}`,
		`{"type":"progress","t":"x","pct":3}`,
	}, "\n")

	boom := fmt.Errorf("boom")
	seen := 0
	err := Decode(strings.NewReader(in), func(e Event) error {
		seen++
		if seen == 2 {
			return boom
		}
		return nil
	})

	if err != boom {
		t.Fatalf("Decode returned %v, want the callback's error", err)
	}
	if seen != 2 {
		t.Fatalf("callback ran %d times, want exactly 2 (decode should stop on error)", seen)
	}
}

// TestDecodeOverEveryProtocolVersion is spec:270 built: the version event's
// `events` number must be refused when it is newer than this window
// understands, rather than read as if it were the number this window was built
// for.
//
// The CLI's own comment on internal/events.ProtocolVersion states the contract
// this satisfies: "a consumer that understands fewer versions than it sees
// should say so rather than guess." Older and equal are not refused — an absent
// field decodes as zero and an unknown field is ignored, so a lower number is
// at worst a subset of what this decoder reads.
func TestDecodeOverEveryProtocolVersion(t *testing.T) {
	cases := []struct {
		name    string
		events  int
		refused bool
	}{
		{"older than this window", ProtocolVersion - 1, false},
		{"the version this window was built for", ProtocolVersion, false},
		{"one newer than this window", ProtocolVersion + 1, true},
		{"far newer than this window", ProtocolVersion + 96, true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := strings.Join([]string{
				fmt.Sprintf(`{"type":"version","t":"x","events":%d,"tool":"9.9.9"}`, c.events),
				`{"type":"progress","t":"x","pct":42}`,
				`{"type":"done","t":"x","path":"/tmp/film.es.srt"}`,
			}, "\n")

			var got []Event
			err := Decode(strings.NewReader(in), func(e Event) error {
				got = append(got, e)
				return nil
			})

			if !c.refused {
				if err != nil {
					t.Fatalf("Decode of events:%d returned %v, want nil — an older or equal "+
						"protocol is readable", c.events, err)
				}
				if len(got) != 3 {
					t.Fatalf("got %d events, want 3", len(got))
				}
				v, err := got[0].AsVersion()
				if err != nil {
					t.Fatalf("AsVersion: %v", err)
				}
				if v.Events != c.events || v.Tool != "9.9.9" {
					t.Errorf("AsVersion = %+v, want Events=%d Tool=9.9.9", v, c.events)
				}
				return
			}

			var unsupported *UnsupportedProtocol
			if !errors.As(err, &unsupported) {
				t.Fatalf("Decode of events:%d returned %v, want an *UnsupportedProtocol — a stream "+
					"this window cannot read must be refused, not guessed at", c.events, err)
			}
			if unsupported.Events != c.events {
				t.Errorf("UnsupportedProtocol.Events = %d, want %d", unsupported.Events, c.events)
			}
			// The refusal has to name both numbers, or it tells the user nothing
			// they can act on.
			msg := unsupported.Error()
			for _, want := range []string{
				strconv.Itoa(c.events),
				strconv.Itoa(ProtocolVersion),
				"9.9.9",
			} {
				if !strings.Contains(msg, want) {
					t.Errorf("the refusal %q does not mention %q", msg, want)
				}
			}
			// And it must refuse before it hands anything over: the events after
			// the version line are exactly the ones whose meaning is in doubt.
			if len(got) != 0 {
				t.Errorf("Decode delivered %d events from a stream it refused, want 0", len(got))
			}
		})
	}
}

// TestDecodeDoesNotRequireAVersionEvent pins the other half of the decision
// above. -probe, -version and the config subcommands emit version and no
// terminal event; a cancel can kill the CLI before it emits anything at all,
// and ProbeFile's signalled-with-an-empty-stream case depends on that being an
// ordinary cancel. So an absent version event is not a protocol violation.
func TestDecodeDoesNotRequireAVersionEvent(t *testing.T) {
	in := `{"type":"progress","t":"x","pct":7}` + "\n"

	seen := 0
	if err := Decode(strings.NewReader(in), func(Event) error { seen++; return nil }); err != nil {
		t.Fatalf("Decode of a stream with no version event returned %v, want nil", err)
	}
	if seen != 1 {
		t.Errorf("Decode delivered %d events, want 1", seen)
	}

	if err := Decode(strings.NewReader(""), func(Event) error { seen++; return nil }); err != nil {
		t.Fatalf("Decode of an empty stream returned %v, want nil", err)
	}
}

// TestDecodeRefusesAMalformedVersionNumber: a version line whose `events` is
// not a number parses as an envelope and would otherwise slip past the check
// with Events left at zero — which reads as "older", the one value that is
// accepted.
func TestDecodeRefusesAMalformedVersionNumber(t *testing.T) {
	in := `{"type":"version","t":"x","events":"two","tool":"9.9.9"}` + "\n"

	seen := 0
	err := Decode(strings.NewReader(in), func(Event) error { seen++; return nil })
	if err == nil {
		t.Fatal("Decode of a version line whose events is a string returned nil, want an error")
	}
	if seen != 0 {
		t.Errorf("Decode delivered %d events from a version line it could not read, want 0", seen)
	}
}

// TestTheWindowsProtocolVersionMatchesTheCLIs is the other direction of I2's
// defect. The refusal above protects this window from a *newer* CLI shipped
// separately; nothing protects it from the CLI in this same tree bumping its
// own ProtocolVersion and leaving this module silently reading version 2 as
// version 1. The two constants are separate on purpose — the programs ship
// apart — but within one tree they must agree, and this is the line that says
// so out loud.
//
// The CLI's file is read as text for the reason TestTheGUIStructsMatchTheCLIsJSONTags
// reads it as text: this module cannot import it.
func TestTheWindowsProtocolVersionMatchesTheCLIs(t *testing.T) {
	src, err := os.ReadFile("../internal/events/events.go")
	if err != nil {
		t.Fatalf("reading the CLI's events.go: %v", err)
	}

	m := regexp.MustCompile(`const ProtocolVersion = (\d+)`).FindSubmatch(src)
	if m == nil {
		t.Fatal("could not find `const ProtocolVersion = <n>` in internal/events/events.go — " +
			"has the CLI's file changed shape? this guard needs updating")
	}
	cli, err := strconv.Atoi(string(m[1]))
	if err != nil {
		t.Fatalf("parsing the CLI's ProtocolVersion %q: %v", m[1], err)
	}

	if cli != ProtocolVersion {
		t.Errorf("internal/events.ProtocolVersion is %d and gui.ProtocolVersion is %d: the CLI in "+
			"this tree emits a protocol this window refuses (or reads a newer one as an older one). "+
			"Teach gui/events.go the new version, then raise gui.ProtocolVersion to %d",
			cli, ProtocolVersion, cli)
	}
}

// TestEventAccessorsRejectTheWrongEventType exercises every accessor's
// mismatch path in one pass: each one must refuse to decode an event of a
// different kind rather than silently returning zero-valued data.
func TestEventAccessorsRejectTheWrongEventType(t *testing.T) {
	e := Event{Type: "progress", Raw: json.RawMessage(`{"type":"progress","t":"x","pct":1}`)}

	checks := []struct {
		name string
		call func() error
	}{
		{"AsVersion", func() error { _, err := e.AsVersion(); return err }},
		{"AsProbe", func() error { _, err := e.AsProbe(); return err }},
		{"AsStage", func() error { _, err := e.AsStage(); return err }},
		{"AsLanguage", func() error { _, err := e.AsLanguage(); return err }},
		{"AsSaved", func() error { _, err := e.AsSaved(); return err }},
		{"AsWarning", func() error { _, err := e.AsWarning(); return err }},
		{"AsDone", func() error { _, err := e.AsDone(); return err }},
		{"AsError", func() error { _, err := e.AsError(); return err }},
	}
	for _, c := range checks {
		if err := c.call(); err == nil {
			t.Errorf("%s on a %q event returned nil error, want a mismatch error", c.name, e.Type)
		}
	}

	// The one accessor matching e.Type must succeed.
	if _, err := e.AsProgress(); err != nil {
		t.Errorf("AsProgress on a matching event: %v", err)
	}
}

// TestTheGUIStructsMatchTheCLIsJSONTags guards the failure mode the task
// brief calls out explicitly: the GUI cannot import internal/events (see
// module_test.go), so every JSON tag in this file is a hand-copied
// duplicate of internal/events/events.go, and a rename or removal on the
// CLI side would otherwise make this decoder silently read a zero for that
// field forever.
//
// It reads the CLI's events.go as text, and for each struct name this file
// also defines, extracts the CLI's json tags for that struct with a
// targeted regexp over the struct body. It then asserts every tag our
// matching Go struct uses (via reflection) is present in that set.
//
// This is deliberately one-directional: a field the CLI *adds* is harmless
// (json decoding ignores unknown keys), so it is not required to appear
// here. Only a tag we rely on vanishing from the CLI's struct — a rename or
// a removal — fails this test, which is the direction that actually causes
// silent data loss.
func TestTheGUIStructsMatchTheCLIsJSONTags(t *testing.T) {
	src, err := os.ReadFile("../internal/events/events.go")
	if err != nil {
		t.Fatalf("reading the CLI's events.go: %v", err)
	}
	body := string(src)

	structRe := func(name string) *regexp.Regexp {
		return regexp.MustCompile(`(?s)type ` + name + ` struct \{(.*?)\n\}`)
	}
	tagRe := regexp.MustCompile(`json:"([^"]*)"`)

	cliTags := func(structName string) map[string]bool {
		m := structRe(structName).FindStringSubmatch(body)
		if m == nil {
			t.Fatalf("could not find `type %s struct { ... }` in internal/events/events.go — "+
				"has the CLI's file changed shape? this guard needs updating", structName)
		}
		tags := map[string]bool{}
		for _, tm := range tagRe.FindAllStringSubmatch(m[1], -1) {
			name := strings.Split(tm[1], ",")[0]
			if name != "" && name != "-" {
				tags[name] = true
			}
		}
		return tags
	}

	ourTags := func(v interface{}) map[string]bool {
		typ := reflect.TypeOf(v)
		tags := map[string]bool{}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			tag, ok := f.Tag.Lookup("json")
			if !ok {
				continue
			}
			name := strings.Split(tag, ",")[0]
			if name != "" && name != "-" {
				tags[name] = true
			}
		}
		return tags
	}

	cases := []struct {
		structName string
		ours       interface{}
	}{
		{"Version", Version{}},
		{"Stream", Stream{}},
		{"Probe", Probe{}},
		{"FastRule", FastRule{}},
		{"Stage", Stage{}},
		{"Progress", Progress{}},
		{"Language", Language{}},
		{"Saved", Saved{}},
		{"Warning", Warning{}},
		{"Done", Done{}},
		{"Error", Error{}},
	}

	for _, c := range cases {
		cli := cliTags(c.structName)
		ours := ourTags(c.ours)
		for tag := range ours {
			if !cli[tag] {
				t.Errorf("gui.%s uses json tag %q, but internal/events.%s no longer has it "+
					"(renamed or removed on the CLI side — update gui/events.go to match)",
					c.structName, tag, c.structName)
			}
		}
	}
}
