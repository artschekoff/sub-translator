package main

import (
	"strings"
	"testing"
)

// theRule is the rule the real CLI sends today, taken from an actual run:
//
//	./bin/sub-translator -probe -events json Se7en_1080p_EuReKA.mkv
//	  "fast": {"sources":["audio"],"modes":["srt"],"min_chunk_minutes":1}
//
// It is a function rather than a package-level var so no test can mutate the
// fixture another test reads.
func theRule() *FastRule {
	return &FastRule{Sources: []string{"audio"}, Modes: []string{"srt"}, MinChunkMinutes: 1}
}

// everySource and everyMode are the complete value sets the CLI's own
// parseSource and parseMode accept (source.go, mode.go). Testing the cross
// product means no source/mode pair the window can produce goes unchecked.
var (
	everySource = []string{"auto", "sub", "audio"}
	everyMode   = []string{"srt", "mux", "both"}
)

// TestFastAvailableOverEverySourceAndModePair walks all nine combinations
// against the rule the CLI actually states, and checks both halves of the
// answer: the enabled state, and that a disabled box carries a reason while an
// enabled one carries none.
func TestFastAvailableOverEverySourceAndModePair(t *testing.T) {
	rule := theRule()

	for _, source := range everySource {
		for _, mode := range everyMode {
			// The CLI accepts -fast only for -source audio -mode srt.
			want := source == "audio" && mode == "srt"

			got, reason := FastAvailable(&Probe{Fast: rule}, source, mode)
			if got != want {
				t.Errorf("FastAvailable(source=%q, mode=%q) = %v, want %v (reason: %q)",
					source, mode, got, want, reason)
			}
			if got && reason != "" {
				t.Errorf("FastAvailable(source=%q, mode=%q) is available but gave a reason %q; "+
					"an enabled checkbox must have nothing to explain", source, mode, reason)
			}
			if !got && reason == "" {
				t.Errorf("FastAvailable(source=%q, mode=%q) is unavailable with no reason; "+
					"the spec requires the reason spelled out rather than a dead control", source, mode)
			}
		}
	}
}

// TestFastAvailableNamesWhichHalfOfTheRuleRefused checks the reason points at
// the control the user has to change. "Fast mode is unavailable" with no
// subject is the failure this guards: the user cannot tell whether to change
// the track or the output.
func TestFastAvailableNamesWhichHalfOfTheRuleRefused(t *testing.T) {
	cases := []struct {
		name         string
		source, mode string
		wantIn       string
	}{
		{"a subtitle source is refused by name", "sub", "srt", "source"},
		{"an auto source is refused by name", "auto", "srt", "source"},
		{"a muxing output is refused by name", "audio", "mux", "output"},
		{"a both output is refused by name", "audio", "both", "output"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, reason := FastAvailable(&Probe{Fast: theRule()}, c.source, c.mode)
			if ok {
				t.Fatalf("FastAvailable(%q, %q) = true, want false", c.source, c.mode)
			}
			if !strings.Contains(reason, c.wantIn) {
				t.Errorf("reason %q does not name %q, so it does not say which control to change",
					reason, c.wantIn)
			}
		})
	}
}

// TestFastAvailableReportsTheSourceBeforeTheOutput pins the order when both
// halves refuse. Without it the answer for ("sub", "mux") is whichever check
// happens to run first, and the sentence a user sees changes with an unrelated
// edit.
func TestFastAvailableReportsTheSourceBeforeTheOutput(t *testing.T) {
	ok, reason := FastAvailable(&Probe{Fast: theRule()}, "sub", "mux")
	if ok {
		t.Fatal("FastAvailable(sub, mux) = true, want false")
	}
	if !strings.Contains(reason, "source") {
		t.Errorf("with both halves refusing, reason %q should name the source first", reason)
	}
	if strings.Contains(reason, "output") {
		t.Errorf("reason %q names both halves at once; one sentence, one thing to fix", reason)
	}
}

// TestFastAvailableWithNoRuleStated is the older-CLI case, over the same nine
// pairs: a nil Fast means the probe event never carried the rule, so fast mode
// is unavailable everywhere. Assuming it available — the tempting reading of
// "the field is missing, so no restriction" — would offer a checkbox whose run
// the CLI then rejects.
func TestFastAvailableWithNoRuleStated(t *testing.T) {
	for _, source := range everySource {
		for _, mode := range everyMode {
			ok, reason := FastAvailable(&Probe{Fast: nil}, source, mode)
			if ok {
				t.Errorf("FastAvailable(source=%q, mode=%q) with a nil Fast = true; "+
					"a CLI that never stated the rule must not be assumed to allow fast mode",
					source, mode)
			}
			if !strings.Contains(reason, "older") {
				t.Errorf("reason %q for a nil Fast does not say the CLI is older; "+
					"it must not read like the rule refusing this combination", reason)
			}
		}
	}
}

// TestFastAvailableWithAnEmptyRuleList covers a rule that is present but
// accepts nothing. "needs the source to be nothing, not audio" is not a
// sentence, so this case has its own.
func TestFastAvailableWithAnEmptyRuleList(t *testing.T) {
	cases := []struct {
		name string
		rule *FastRule
	}{
		{"no sources accepted", &FastRule{Sources: nil, Modes: []string{"srt"}}},
		{"no modes accepted", &FastRule{Sources: []string{"audio"}, Modes: nil}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, reason := FastAvailable(&Probe{Fast: c.rule}, "audio", "srt")
			if ok {
				t.Fatalf("FastAvailable with %s = true, want false", c.name)
			}
			if !strings.Contains(reason, "lists no") {
				t.Errorf("reason %q should say the CLI lists nothing that fast mode accepts", reason)
			}
		})
	}
}

// TestFastAvailableWithNoProbe guards the nil *Probe the window can reach by
// asking about fast mode before a file is loaded. It must answer, not panic.
func TestFastAvailableWithNoProbe(t *testing.T) {
	ok, reason := FastAvailable(nil, "audio", "srt")
	if ok {
		t.Error("FastAvailable(nil, ...) = true; nothing is known about the file yet")
	}
	if reason == "" {
		t.Error("FastAvailable(nil, ...) gave no reason")
	}
}

// TestFastAvailableFollowsTheRuleItIsGiven proves the answer comes from the
// data and not from a copy of today's rule: a CLI that starts accepting -fast
// with -mode mux changes this function's answer with no edit here.
func TestFastAvailableFollowsTheRuleItIsGiven(t *testing.T) {
	widened := &FastRule{Sources: []string{"audio", "sub"}, Modes: []string{"srt", "mux"}}

	for _, source := range []string{"audio", "sub"} {
		for _, mode := range []string{"srt", "mux"} {
			if ok, reason := FastAvailable(&Probe{Fast: widened}, source, mode); !ok {
				t.Errorf("FastAvailable(source=%q, mode=%q) against a widened rule = false (%q); "+
					"the rule is data, and this function must read it rather than hold its own copy",
					source, mode, reason)
			}
		}
	}
	if ok, _ := FastAvailable(&Probe{Fast: widened}, "auto", "srt"); ok {
		t.Error("a widened rule that still excludes auto must still exclude auto")
	}
}

// boolPtr is how the three states of probe.embeds_subtitles are built in these
// tests: nil, &false and &true.
func boolPtr(b bool) *bool { return &b }

// modeValues reduces OutputModes' first return to the -mode strings, which is
// what the assertions are actually about.
func modeValues(modes []OutputMode) []string {
	out := make([]string, 0, len(modes))
	for _, m := range modes {
		out = append(out, m.Value)
	}
	return out
}

// TestOutputModesOverAllThreeStatesOfEmbedsSubtitles is the table the brief
// asks for: every state of the pointer, the set offered, and the note that
// explains it. The two notes for the two single-mode cases must be different
// constants — that difference is the rule.
func TestOutputModesOverAllThreeStatesOfEmbedsSubtitles(t *testing.T) {
	cases := []struct {
		name      string
		embeds    *bool
		wantModes []string
		wantNote  string
	}{
		{
			name:      "true offers the sidecar, the embed and both",
			embeds:    boolPtr(true),
			wantModes: []string{"srt", "mux", "both"},
			wantNote:  "",
		},
		{
			name:      "false is the AVI case and offers only the sidecar",
			embeds:    boolPtr(false),
			wantModes: []string{"srt"},
			wantNote:  EmbedRefusedNote,
		},
		{
			name:      "nil is nobody saying, and is not silently treated as false",
			embeds:    nil,
			wantModes: []string{"srt"},
			wantNote:  EmbedUnknownNote,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			modes, note := OutputModes(&Probe{EmbedsSubtitles: c.embeds})

			got := modeValues(modes)
			if strings.Join(got, ",") != strings.Join(c.wantModes, ",") {
				t.Errorf("OutputModes offered %v, want %v", got, c.wantModes)
			}
			if note != c.wantNote {
				t.Errorf("note = %q, want %q", note, c.wantNote)
			}
			for _, m := range modes {
				if m.Label == "" {
					t.Errorf("mode %q has no label; a nameless choice in the control says nothing", m.Value)
				}
			}
		})
	}
}

// TestOutputModesDistinguishesUnknownFromRefused is the "do not treat nil as
// false silently" requirement stated as an assertion. Both cases offer the
// same one mode, so the note is the only place the difference can show — and
// if the two ever become one string, this fails.
func TestOutputModesDistinguishesUnknownFromRefused(t *testing.T) {
	_, refused := OutputModes(&Probe{EmbedsSubtitles: boolPtr(false)})
	_, unknown := OutputModes(&Probe{EmbedsSubtitles: nil})

	if refused == "" || unknown == "" {
		t.Fatalf("both withholding cases must explain themselves: refused=%q unknown=%q", refused, unknown)
	}
	if refused == unknown {
		t.Errorf("a container that cannot embed and a CLI that never said read identically: %q\n"+
			"nil must not be reported as false", refused)
	}
	if !strings.Contains(unknown, "did not report") {
		t.Errorf("the unknown note %q does not say the answer is missing", unknown)
	}
	if !strings.Contains(refused, "cannot hold") {
		t.Errorf("the refused note %q does not say the container refused", refused)
	}
}

// TestOutputModesAlwaysOffersTheSidecarFirst pins the one mode that works on
// every container, in the CLI's own default position.
func TestOutputModesAlwaysOffersTheSidecarFirst(t *testing.T) {
	for _, embeds := range []*bool{nil, boolPtr(false), boolPtr(true)} {
		modes, _ := OutputModes(&Probe{EmbedsSubtitles: embeds})
		if len(modes) == 0 || modes[0].Value != "srt" {
			t.Errorf("with embeds_subtitles=%v the first mode offered is %v, want srt first",
				embeds, modeValues(modes))
		}
	}
}

// TestOutputModesWithNoProbe guards the call made before a file is loaded.
func TestOutputModesWithNoProbe(t *testing.T) {
	modes, note := OutputModes(nil)
	if len(modes) != 1 || modes[0].Value != "srt" {
		t.Errorf("OutputModes(nil) offered %v, want just srt", modeValues(modes))
	}
	if note != EmbedNoProbeNote {
		t.Errorf("note = %q, want %q", note, EmbedNoProbeNote)
	}
}

// probeWithTracks builds a probe carrying the subtitle tracks the source-
// language tests choose between: one tagged, one untagged — the pair the
// CLI's own formatTracks comment calls common in the wild.
func probeWithTracks() *Probe {
	return &Probe{
		Audio: []Stream{{Index: 1, Lang: "rus"}, {Index: 2, Lang: ""}},
		Subtitles: []Stream{
			{Index: 4, Lang: "eng", Title: "Original"},
			{Index: 5, Lang: "", Title: ""},
		},
	}
}

// TestSourceLanguageOverEverySourceAndTrack is correction I-1's rule as a
// table. The case that matters most is the tagged track still needing the
// control: ordinary.go prompts for -from whenever -source is sub, and never
// reads the track's lang tag, so "only untagged tracks need it" would send a
// run that stops at a prompt this window cannot answer.
func TestSourceLanguageOverEverySourceAndTrack(t *testing.T) {
	p := probeWithTracks()

	cases := []struct {
		name        string
		source      string
		track       int
		wantNeeded  bool
		wantPrefill string
	}{
		{"transcription needs none — whisper detects it", "audio", 1, false, ""},
		{"transcription of an untagged audio track still needs none", "audio", 2, false, ""},
		{"a tagged subtitle track needs one, prefilled from the tag", "sub", 4, true, "eng"},
		{"an untagged subtitle track needs one with nothing to prefill", "sub", 5, true, ""},
		{"a track index the probe never reported needs one, unprefilled", "sub", 99, true, ""},
		{"auto is the CLI's default and is never what this window sends", "auto", 4, false, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			needed, prefill := SourceLanguage(p, c.source, c.track)
			if needed != c.wantNeeded {
				t.Errorf("SourceLanguage(source=%q, track=%d) needed = %v, want %v",
					c.source, c.track, needed, c.wantNeeded)
			}
			if prefill != c.wantPrefill {
				t.Errorf("SourceLanguage(source=%q, track=%d) prefill = %q, want %q",
					c.source, c.track, prefill, c.wantPrefill)
			}
		})
	}
}

// TestSourceLanguageNeverPrefillsFromAnAudioTrack guards the mix-up that would
// look right on the Se7en fixture, where audio #1 and subtitle #4 both exist:
// the lang must come from the subtitle list, matched on the stream index the
// CLI reports, not from whichever track happens to share a position.
func TestSourceLanguageNeverPrefillsFromAnAudioTrack(t *testing.T) {
	p := &Probe{
		Audio:     []Stream{{Index: 1, Lang: "rus"}},
		Subtitles: []Stream{{Index: 1, Lang: "eng"}},
	}
	if _, prefill := SourceLanguage(p, "sub", 1); prefill != "eng" {
		t.Errorf("prefill = %q, want eng — the subtitle track's tag, not the audio track's", prefill)
	}
	if needed, _ := SourceLanguage(p, "audio", 1); needed {
		t.Error("transcription must not require a source language")
	}
}

// TestSourceLanguageWithNoProbe guards the nil *Probe: a subtitle source still
// needs a language, and there is simply nothing to prefill it with.
func TestSourceLanguageWithNoProbe(t *testing.T) {
	needed, prefill := SourceLanguage(nil, "sub", 4)
	if !needed {
		t.Error("SourceLanguage(nil, sub, 4) said no source language is needed")
	}
	if prefill != "" {
		t.Errorf("prefill = %q, want empty", prefill)
	}
}

// TestOrListReadsAsProse covers the sentence builder the reasons go through,
// because a rule that ever accepts two sources would otherwise read
// "audio sub" and nobody would notice until it shipped.
func TestOrListReadsAsProse(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, "nothing"},
		{[]string{"audio"}, "audio"},
		{[]string{"srt", "mux"}, "srt or mux"},
		{[]string{"srt", "mux", "both"}, "srt, mux or both"},
	}
	for _, c := range cases {
		if got := orList(c.in); got != c.want {
			t.Errorf("orList(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
