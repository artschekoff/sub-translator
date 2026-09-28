package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// These strings are a contract with anything driving the tool. Renaming one
// silently breaks a consumer that switches on it, so they are pinned here
// rather than left to whatever the constant happens to say.
func TestErrKindsAreTheDocumentedStrings(t *testing.T) {
	want := map[errKind]string{
		errMissingKey:       "missing_key",
		errInvalidKey:       "invalid_key",
		errWhisperNotFound:  "whisper_not_found",
		errNoModel:          "no_model",
		errAmbiguousModel:   "ambiguous_model",
		errNoAudioTrack:     "no_audio_track",
		errNoSubtitleTrack:  "no_subtitle_track",
		errProbeFailed:      "probe_failed",
		errOutputUnwritable: "output_unwritable",
		errRateLimited:      "rate_limited",
		errTranscriptEmpty:  "transcript_empty",
		errCancelled:        "cancelled",
		errOther:            "other",
	}
	for k, s := range want {
		if string(k) != s {
			t.Errorf("kind %q, want %q", string(k), s)
		}
	}
	if len(allErrKinds()) != len(want) {
		t.Errorf("allErrKinds() has %d entries, want %d — add the new one to both",
			len(allErrKinds()), len(want))
	}
}

func TestAllErrKindsHasNoDuplicates(t *testing.T) {
	seen := map[errKind]bool{}
	for _, k := range allErrKinds() {
		if seen[k] {
			t.Errorf("kind %q appears twice", k)
		}
		seen[k] = true
	}
}

// Every kind must be lowercase with underscores: a consumer matching on these
// should not have to guess at casing.
func TestErrKindsAreSnakeCase(t *testing.T) {
	for _, k := range allErrKinds() {
		for _, r := range string(k) {
			if (r < 'a' || r > 'z') && r != '_' {
				t.Errorf("kind %q contains %q; want lowercase and underscores only", k, r)
			}
		}
	}
}

func TestErrOtherIsNotInTheClassifiedSet(t *testing.T) {
	classified := allErrKinds()
	i := slices.Index(classified, errOther)
	if i < 0 {
		t.Fatal("errOther must be in allErrKinds so the list is exhaustive")
	}
	if classified[len(classified)-1] != errOther {
		t.Error("errOther should be listed last: it is the fallback, not a peer")
	}
}

// readRepoFile reads a file from the repository root, which is the working
// directory of a test in this package.
func readRepoFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

const (
	readmePath = "README.md"
	specPath   = "docs/superpowers/specs/2026-09-26-desktop-gui-design.md"
)

// The kinds are a contract a consumer switches on, and the two documents a
// consumer is written from are the README's table and the spec's. A kind that
// exists in the code and in neither of them is a kind nobody can route on.
func TestEveryErrKindIsDocumented(t *testing.T) {
	readme := readRepoFile(t, readmePath)
	spec := readRepoFile(t, specPath)
	for _, k := range allErrKinds() {
		// A table row, not a mention in passing: a kind explained in prose but
		// missing from the table a consumer reads its switch off is undocumented
		// where it counts.
		row := regexp.MustCompile("(?m)^\\| `" + regexp.QuoteMeta(string(k)) + "` \\|")
		if !row.MatchString(readme) {
			t.Errorf("kind %q has no row in the README's table", k)
		}
		if !row.MatchString(spec) {
			t.Errorf("kind %q has no row in the spec's table", k)
		}
	}
}

// stageNameInSource finds the stage names the code actually emits, by scanning
// the source rather than requiring a bespoke runtime test for each one. That
// used to be the only way to reach two of the five — extract-subs and mux —
// but is not any more, though the two reach the compiled binary by different
// routes. TestTheSubtitlePathEmitsTheExtractSubsStage drives extract-subs on
// its own: the run fails before translation is ever attempted, so it needs no
// translate stub at all. mux is reached only past a successful translation, so
// TestTheOrdinaryRunEmitsTheContainerDoneEvent and TestTheMuxWriteIsClassified
// depend on stubTranslateEndpoint, which lets a compiled-binary test point
// translation at a local server instead of the real endpoint.
// This scan still earns its place regardless: it is what stands between any of
// the five and being deleted with the suite green, because the emitted set,
// the README's table and the spec's list have to agree, and removing an emit
// breaks that agreement whether or not a runtime test happens to cover it.
var stageNameInSource = regexp.MustCompile(`(?s)events\.Stage\{[^}]*Name:\s*"([a-z-]+)"`)

// stageRowInREADME matches a row of the README's stage table.
var stageRowInREADME = regexp.MustCompile("(?m)^\\| `([a-z-]+)` \\|")

func TestEveryStageNameIsEmittedAndDocumented(t *testing.T) {
	emitted := map[string]bool{}
	// Every source file, not a hand-written list of the two that happened to
	// hold the emits: lifting main()'s pipeline into ordinary.go moved
	// extract-subs and mux out of a hard-coded {main.go, progressive.go} and
	// failed this test for a reason that had nothing to do with what it checks.
	// goFiles is the same list the stderr test reads, and it cannot go stale.
	for _, file := range goFiles(t) {
		for _, m := range stageNameInSource.FindAllStringSubmatch(readRepoFile(t, file), -1) {
			emitted[m[1]] = true
		}
	}
	// A sanity floor: a regexp that matched nothing would make every assertion
	// below vacuous.
	if len(emitted) < 2 {
		t.Fatalf("found %d stage names in the source (%v); the scan itself is broken", len(emitted), emitted)
	}

	readme := readRepoFile(t, readmePath)
	header := strings.Index(readme, "| `stage.name` | The phase |")
	if header < 0 {
		t.Fatal("the README has no stage-name table")
	}
	// The five rows following the header are the stage table; the tables further
	// down the README (the kinds, the exit codes) are not read here.
	documented := map[string]bool{}
	for _, m := range stageRowInREADME.FindAllStringSubmatch(readme[header:], 5) {
		documented[m[1]] = true
	}
	if len(documented) != 5 {
		t.Fatalf("the README's stage table has %d rows (%v), want the five phases", len(documented), documented)
	}

	for name := range emitted {
		if !documented[name] {
			t.Errorf("the code emits stage %q and the README's table does not list it", name)
		}
	}
	for name := range documented {
		if !emitted[name] {
			t.Errorf("the README's table lists stage %q and nothing emits it", name)
		}
	}

	spec := readRepoFile(t, specPath)
	for name := range emitted {
		if !strings.Contains(spec, "`"+name+"`") {
			t.Errorf("the code emits stage %q and the spec does not name it", name)
		}
	}
}

// goFiles are the package's own source files, which two of the tests below read
// rather than re-deriving what they claim from a copy.
func goFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			out = append(out, name)
		}
	}
	if len(out) < 5 {
		t.Fatalf("found %d source files (%v); the scan itself is broken", len(out), out)
	}
	return out
}

// Every write this process makes to stderr has to go through proseErr, the one
// serialised writer that also knows whether the stream is mid-line. A write
// straight to os.Stderr is outside both guarantees, and it reopens the failure
// the whole round is about the moment anything is emitted alongside it — which is
// exactly what `model pull`'s carriage-return progress did. The one permitted
// mention is the line that builds proseErr itself.
func TestNothingWritesStderrOutsideTheSharedWriter(t *testing.T) {
	const allowed = "var proseErr = events.NewSyncWriter(os.Stderr)"
	for _, file := range goFiles(t) {
		for i, line := range strings.Split(readRepoFile(t, file), "\n") {
			if !strings.Contains(line, "os.Stderr") || strings.Contains(line, allowed) {
				continue
			}
			t.Errorf("%s:%d writes stderr outside proseErr:\n\t%s", file, i+1, strings.TrimSpace(line))
		}
	}
}
