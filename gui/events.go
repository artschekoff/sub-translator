// events.go decodes the CLI's `-events json` stream: one JSON object per
// line on stderr, interleaved with prose meant for a person. The GUI cannot
// import the CLI's internal/events package (see module_test.go), so every
// JSON tag below is a hand-copied duplicate of internal/events/events.go and
// must be kept in sync by hand. TestTheGUIStructsMatchTheCLIsJSONTags in
// events_test.go guards the dangerous half of that duplication: a rename or
// removal of a field on the CLI side. An added field is harmless — decoding
// into these structs simply ignores it — so that guard checks only that
// every tag used here still exists on the CLI's matching struct, not the
// other direction.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Stream is one audio or subtitle track as the probe reports it.
type Stream struct {
	Index int    `json:"index"`
	Lang  string `json:"lang"`
	Title string `json:"title"`
	Codec string `json:"codec"`
}

// FastRule is the condition under which -fast is accepted.
type FastRule struct {
	Sources         []string `json:"sources"`
	Modes           []string `json:"modes"`
	MinChunkMinutes int      `json:"min_chunk_minutes"`
}

// Probe carries what the CLI learned about the input file before it starts
// translating.
//
// EmbedsSubtitles is a *bool, not a bool, on purpose: false means this
// container cannot take an embedded subtitle track (the AVI case), nil means
// the probe didn't say. A plain bool would conflate "no" with "unknown", and
// the CLI made and then fixed that exact mistake once already.
type Probe struct {
	Duration        float64   `json:"duration,omitempty"`
	Audio           []Stream  `json:"audio"`
	Subtitles       []Stream  `json:"subtitles"`
	Fast            *FastRule `json:"fast,omitempty"`
	EmbedsSubtitles *bool     `json:"embeds_subtitles,omitempty"`
}

// Stage names the phase of work currently running, and — during -fast —
// which chunk of how many, covering which timespan.
type Stage struct {
	Name  string `json:"name"`
	Chunk int    `json:"chunk,omitempty"`
	Of    int    `json:"of,omitempty"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
}

// Progress is a coarse percent-complete update.
type Progress struct {
	Pct int `json:"pct"`
}

// Language is the source language the CLI detected.
type Language struct {
	Detected string `json:"detected"`
}

// Saved names one file a run has written and says whether it is usable yet.
type Saved struct {
	Path      string `json:"path"`
	Covers    string `json:"covers,omitempty"`
	Watchable bool   `json:"watchable"`
}

// Warning reports that some blocks could not be translated.
type Warning struct {
	Kind  string `json:"kind"`
	Count int    `json:"count"`
	Total int    `json:"total"`
	First []int  `json:"first,omitempty"`
}

// Done marks the end of a successful run.
type Done struct {
	Path   string `json:"path"`
	Covers string `json:"covers,omitempty"`
}

// Error carries a run that failed.
type Error struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// Event is one line of the decoded stream. Raw holds the whole JSON object,
// so an event type this GUI has never heard of still arrives with its Type
// and T intact — it can be logged or ignored — instead of being dropped the
// way a tagged union would have to drop it.
type Event struct {
	Type string
	T    string
	Raw  json.RawMessage
}

// envelope is the part of every event line this decoder must read to route
// it, independent of which event kind it turns out to be.
type envelope struct {
	Type string `json:"type"`
	T    string `json:"t"`
}

// Decode reads newline-delimited events from r, calling on once per event in
// order. Lines that are blank, or whose first non-space byte is not '{', are
// prose meant for a person and are skipped. A line that starts with '{' but
// fails to parse is treated as a bug, not prose, and stops the decode with an
// error naming the line: the CLI's protocol guarantees every line is whole,
// so a malformed '{'-leading line means something upstream is broken. If on
// returns an error, Decode stops and returns that error.
func Decode(r io.Reader, on func(Event) error) error {
	sc := bufio.NewScanner(r)
	// The default 64 KB scanner buffer is too small for a warning event
	// carrying thousands of "first" indices; 4 MB gives ample headroom for
	// any one line this protocol emits.
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for sc.Scan() {
		line := bytes.TrimRight(sc.Bytes(), "\r")

		trimmed := bytes.TrimLeft(line, " \t")
		if len(trimmed) == 0 || trimmed[0] != '{' {
			continue
		}

		var env envelope
		if err := json.Unmarshal(line, &env); err != nil {
			return fmt.Errorf("events: malformed line %s: %w", line, err)
		}

		raw := make(json.RawMessage, len(line))
		copy(raw, line)

		ev := Event{Type: env.Type, T: env.T, Raw: raw}
		if err := on(ev); err != nil {
			return err
		}
	}
	return sc.Err()
}

// wrongType reports that an accessor was called on an event of a different
// kind than the one it decodes.
func wrongType(want, got string) error {
	return fmt.Errorf("events: not a %q event (type=%q)", want, got)
}

func (e Event) AsProbe() (*Probe, error) {
	if e.Type != "probe" {
		return nil, wrongType("probe", e.Type)
	}
	var p Probe
	if err := json.Unmarshal(e.Raw, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (e Event) AsStage() (*Stage, error) {
	if e.Type != "stage" {
		return nil, wrongType("stage", e.Type)
	}
	var s Stage
	if err := json.Unmarshal(e.Raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (e Event) AsProgress() (*Progress, error) {
	if e.Type != "progress" {
		return nil, wrongType("progress", e.Type)
	}
	var p Progress
	if err := json.Unmarshal(e.Raw, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (e Event) AsLanguage() (*Language, error) {
	if e.Type != "language" {
		return nil, wrongType("language", e.Type)
	}
	var l Language
	if err := json.Unmarshal(e.Raw, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

func (e Event) AsSaved() (*Saved, error) {
	if e.Type != "saved" {
		return nil, wrongType("saved", e.Type)
	}
	var s Saved
	if err := json.Unmarshal(e.Raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (e Event) AsWarning() (*Warning, error) {
	if e.Type != "warning" {
		return nil, wrongType("warning", e.Type)
	}
	var w Warning
	if err := json.Unmarshal(e.Raw, &w); err != nil {
		return nil, err
	}
	return &w, nil
}

func (e Event) AsDone() (*Done, error) {
	if e.Type != "done" {
		return nil, wrongType("done", e.Type)
	}
	var d Done
	if err := json.Unmarshal(e.Raw, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func (e Event) AsError() (*Error, error) {
	if e.Type != "error" {
		return nil, wrongType("error", e.Type)
	}
	var er Error
	if err := json.Unmarshal(e.Raw, &er); err != nil {
		return nil, err
	}
	return &er, nil
}
