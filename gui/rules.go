// rules.go holds the File screen's decisions — the ones a user would see get
// wrong — in Go rather than in the page's JavaScript.
//
// The reason is testability, not taste. This project ships no node toolchain
// (see TestTheTrackedTreeHasNoNodeToolchain), so a rule written in app.js has
// no harness at all: the only way to find out that a checkbox is enabled when
// it should not be, or that an output mode is offered when the container
// cannot take it, would be to open the window and look. The same rules in Go
// are table-tested, and the frontend reaches them over the Wails binding.
//
// None of the three reimplements a rule the CLI owns. Each one reads what the
// CLI stated on its own probe event — `fast`, `embeds_subtitles`, and a
// track's `lang` — and turns it into the answer one control needs. Where the
// CLI said nothing, that is reported as nothing said, never as "no".
package main

import (
	"fmt"
	"slices"
	"strings"
)

// FastState is FastAvailable's answer in the shape the checkbox reads it: the
// enabled state, plus the sentence to print beside a disabled box. The spec
// requires the reason rather than a dead control, so the two travel together.
type FastState struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}

// FastAvailable answers the question the "fast mode" checkbox asks, for the
// source and output mode currently chosen, from the rule the CLI stated on its
// probe event rather than from a second copy of that rule.
//
// A nil Fast is an older sub-translator, one built before the probe event
// carried the rule. Fast mode is then simply unavailable: it is not assumed
// available (the CLI would reject the run, and the window would have offered
// something that cannot work), and it is not reported as the rule refusing the
// combination either, because no rule was stated. The reason says which of
// those two it is.
//
// It reads two of the rule's three fields, and the third is skipped on
// purpose rather than overlooked. `fast.min_chunk_minutes` is a real clause of
// the CLI's own check — validateFastFlags in progressive.go rejects a
// -chunk below it — but it is a condition on a flag this window never sends.
// There is no chunk-length control on the File screen, so the CLI applies its
// own default and the floor cannot be violated from here; consulting the field
// would mean inventing a chunk value to test it against. If a chunk control is
// ever added, this is the function that has to grow a third check, and
// TestFastAvailableIgnoresTheChunkFloor is the test that has to stop passing.
//
// The returned reason is empty exactly when the first return is true.
func FastAvailable(p *Probe, source, mode string) (bool, string) {
	if p == nil {
		return false, "Fast mode needs the file probed first."
	}
	if p.Fast == nil {
		return false, "This sub-translator did not say which runs accept fast mode, " +
			"so the window will not offer it — it is older than this window expects."
	}
	if ok, why := permits(p.Fast.Sources, source, "source"); !ok {
		return false, why
	}
	if ok, why := permits(p.Fast.Modes, mode, "output"); !ok {
		return false, why
	}
	return true, ""
}

// permits reports whether value is in the rule's list of accepted values for
// what ("source" or "output"), and when it is not, says so in terms of the
// list the CLI sent rather than in terms of a rule hardcoded here. That is the
// whole point of the rule travelling as data: if the CLI ever accepts -fast
// with -mode mux, this sentence changes with it and no code here does.
//
// An empty list is distinguished from a value that misses: a present-but-empty
// `sources` accepts nothing at all, and saying "needs the source to be
// nothing, not audio" would be nonsense.
func permits(allowed []string, value, what string) (bool, string) {
	if len(allowed) == 0 {
		return false, fmt.Sprintf("This sub-translator lists no %s that fast mode accepts.", what)
	}
	if slices.Contains(allowed, value) {
		return true, ""
	}
	return false, fmt.Sprintf("Fast mode needs the %s to be %s, not %s.", what, orList(allowed), value)
}

// orList renders a rule's accepted values as prose: "audio", "srt or mux",
// "srt, mux or both".
func orList(values []string) string {
	switch len(values) {
	case 0:
		return "nothing"
	case 1:
		return values[0]
	}
	return strings.Join(values[:len(values)-1], ", ") + " or " + values[len(values)-1]
}

// OutputMode is one choice in the File screen's output control: the value the
// run sends as -mode, and the words the control shows for it.
type OutputMode struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// The three notes OutputModes can return. They are constants so the tests can
// assert which one came back rather than matching on a fragment of prose, and
// so the two reasons for offering only the sidecar cannot drift into one
// sentence — which is the whole distinction this rule exists to keep.
const (
	// EmbedRefusedNote is the AVI case: the CLI probed the container and said
	// no. The window states it, because sub-translator would accept -mode mux
	// here and quietly write a sidecar instead (resolveMode's downgrade).
	EmbedRefusedNote = "This container cannot hold an embedded subtitle track, " +
		"so only a .srt beside the film is offered — sub-translator would fall back to one anyway."

	// EmbedUnknownNote is not the AVI case and must not read like it. A probe
	// event without embeds_subtitles is an older sub-translator that never
	// reported the answer. Offering the embedding modes on that silence would
	// rewrite a multi-gigabyte container on a guess, so they are withheld —
	// but the window says it is withholding them for want of an answer, not
	// because this file was refused.
	EmbedUnknownNote = "This sub-translator did not report whether the container can hold " +
		"an embedded subtitle track — nobody said it cannot — so only a .srt beside the film is offered."

	// EmbedNoProbeNote covers a call made before anything has been probed.
	EmbedNoProbeNote = "Output modes need the file probed first."
)

// OutputModes returns the output modes the File screen may offer for this
// file, and the note explaining the set when it is not the full one.
//
// probe.embeds_subtitles is a *bool with three states and all three are
// distinct here: true means the container accepts an embedded track and all
// three modes are offered; false is the AVI case and only the sidecar is;
// nil means nobody said, which is also only the sidecar — but with a
// different note, because "we were not told" is not "you cannot". Collapsing
// nil into false would make an older CLI look like an AVI, and collapsing it
// into true would offer a container rewrite the CLI may refuse.
//
// The sidecar is always first and always present: it is the CLI's own default
// -mode and the only mode that works on every container.
func OutputModes(p *Probe) ([]OutputMode, string) {
	sidecar := OutputMode{Value: "srt", Label: "A .srt file beside the film"}
	embed := OutputMode{Value: "mux", Label: "A new video file with the track inside"}
	both := OutputMode{Value: "both", Label: "Both the .srt and the new video file"}

	switch {
	case p == nil:
		return []OutputMode{sidecar}, EmbedNoProbeNote
	case p.EmbedsSubtitles == nil:
		return []OutputMode{sidecar}, EmbedUnknownNote
	case !*p.EmbedsSubtitles:
		return []OutputMode{sidecar}, EmbedRefusedNote
	}
	return []OutputMode{sidecar, embed, both}, ""
}

// SourceLangState is SourceLanguage's answer: whether the run needs a source
// language at all, and what to put in the field when the file already says.
type SourceLangState struct {
	Needed  bool   `json:"needed"`
	Prefill string `json:"prefill"`
}

// SourceLanguage answers whether this choice of source needs -from, and what
// the picker should start with.
//
// This is the control the spec's File screen was missing (correction I-1).
// `-source sub` requires -from unconditionally: ordinary.go reaches
//
//	if from == "" && source == sourceSub { from = promptLang("Source") }
//
// and promptLang reads stdin, which under this window is not a terminal — so
// an empty -from ends the run with "no source language given" and a Retry
// button that can never succeed.
//
// Note what that code does *not* do: it never reads the chosen track's `lang`
// tag. So -from is required for every subtitle-source run, not only for the
// untagged tracks the brief called out — a track tagged lang=eng needs it
// just as much. The tag is a default for the field, not a substitute for it,
// which is why this returns a prefill rather than an answer. An untagged
// track — common in the wild, as the CLI's own formatTracks comment says —
// simply prefills nothing and leaves Start disabled until the user chooses.
//
// Transcription needs no source language: whisper detects it, and the same
// condition above skips the prompt for -source audio.
func SourceLanguage(p *Probe, source string, track int) (bool, string) {
	if source != "sub" {
		return false, ""
	}
	if p == nil {
		return true, ""
	}
	for _, s := range p.Subtitles {
		if s.Index == track {
			return true, s.Lang
		}
	}
	return true, ""
}
