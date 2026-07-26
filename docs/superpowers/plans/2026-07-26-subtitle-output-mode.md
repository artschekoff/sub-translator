# Subtitle Output Mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `-mode srt|mux|both` flag that selects whether the translated subtitles are written as a sidecar `.srt` next to the video, embedded into a new container, or both — with the sidecar as the new default.

**Architecture:** A new `outputMode` type in the `main` package owns mode parsing and the AVI-style container downgrade, in its own file (`mode.go`) so it is unit-testable without touching ffmpeg. `main.go` keeps orchestration only: it parses the flag once, resolves the mode against the container's capabilities, then branches on two predicates (`writesSRT()`, `writesContainer()`) instead of the current tangle of `saveSRT`/`noMux`/`aviMode` booleans. No new packages, no new dependencies.

**Tech Stack:** Go 1.26, standard library only (`flag`, `strings`, `testing`). ffmpeg/ffprobe are invoked as subprocesses by `internal/media` and are not touched by this work.

## Global Constraints

- Go version floor: **1.26** (`go.mod` says `go 1.26.2`) — do not lower it, do not add a toolchain directive.
- **No new third-party dependencies.** `go.mod` has zero requires today and must still have zero when this plan is done.
- All code must pass `make validate` (which runs `gofmt -w .`, `go vet ./...`, `go test ./... -v`).
- Flag names and mode values are lowercase and exact: `-mode`, values `srt`, `mux`, `both`.
- The default mode is **`srt`**. A bare `sub-translator -to es movie.mkv` must NOT write a video file.
- The flags `-srt` and `-no-mux` are **removed**, not deprecated. Go's `flag` package already errors on unknown flags and prints usage, which is the desired behaviour for anyone still passing them.
- Commit messages use conventional commits (`feat:`, `test:`, `docs:`), matching the existing history.
- Do not reformat or restructure code unrelated to this feature.

---

## File Structure

| File | Status | Responsibility |
|---|---|---|
| `mode.go` | Create | `outputMode` type, `parseMode`, `resolveMode`, and the `writesSRT`/`writesContainer` predicates. Pure logic, no I/O. |
| `mode_test.go` | Create | Table tests for parsing, the predicates, and the container downgrade. |
| `main.go` | Modify | Replace `-srt`/`-no-mux` with `-mode`; rewrite the output section (currently lines 148–185) to branch on the mode; update the `usage` const. |
| `README.md` | Modify | Document the mode, make `-mode` prominent in Usage, fix every example that passes a removed flag. |

`internal/media`, `internal/srt` and `internal/translate` are **not modified**. `media.ContainerSupportsEmbeddedSubs`, `media.DefaultSRTPath` and `media.DefaultOutputPath` already provide everything the new logic needs.

---

### Task 1: Output mode type and resolution

**Files:**
- Create: `mode.go`
- Test: `mode_test.go`

**Interfaces:**
- Consumes: nothing — this task is self-contained and adds no imports beyond `fmt` and `strings`.
- Produces, all in package `main`:
  - `type outputMode int` with unexported constants `modeSRT`, `modeMux`, `modeBoth`
  - `func (m outputMode) String() string` — returns `"srt"`, `"mux"` or `"both"`
  - `func (m outputMode) writesSRT() bool`
  - `func (m outputMode) writesContainer() bool`
  - `func parseMode(s string) (outputMode, error)`
  - `func resolveMode(m outputMode, canEmbed bool) (outputMode, string)` — returns the effective mode plus a human-readable note (empty string when nothing was downgraded)

- [ ] **Step 1: Write the failing test**

Create `mode_test.go`:

```go
package main

import "testing"

func TestParseMode(t *testing.T) {
	tests := []struct {
		in      string
		want    outputMode
		wantErr bool
	}{
		{"srt", modeSRT, false},
		{"mux", modeMux, false},
		{"both", modeBoth, false},
		{"SRT", modeSRT, false},
		{"  mux  ", modeMux, false},
		{"", 0, true},
		{"nomux", 0, true},
		{"srt,mux", 0, true},
	}

	for _, tt := range tests {
		got, err := parseMode(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseMode(%q): want error, got mode %v", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMode(%q): unexpected error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseMode(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestParseModeErrorNamesValidValues(t *testing.T) {
	_, err := parseMode("bogus")
	if err == nil {
		t.Fatal("want error for bogus mode")
	}
	for _, want := range []string{"srt", "mux", "both", "bogus"} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestModePredicates(t *testing.T) {
	tests := []struct {
		mode           outputMode
		wantSRT        bool
		wantContainer  bool
		wantString     string
	}{
		{modeSRT, true, false, "srt"},
		{modeMux, false, true, "mux"},
		{modeBoth, true, true, "both"},
	}

	for _, tt := range tests {
		if got := tt.mode.writesSRT(); got != tt.wantSRT {
			t.Errorf("%v.writesSRT() = %v, want %v", tt.mode, got, tt.wantSRT)
		}
		if got := tt.mode.writesContainer(); got != tt.wantContainer {
			t.Errorf("%v.writesContainer() = %v, want %v", tt.mode, got, tt.wantContainer)
		}
		if got := tt.mode.String(); got != tt.wantString {
			t.Errorf("String() = %q, want %q", got, tt.wantString)
		}
	}
}

func TestResolveModeDowngradesWhenContainerCannotEmbed(t *testing.T) {
	for _, mode := range []outputMode{modeMux, modeBoth} {
		got, note := resolveMode(mode, false)
		if got != modeSRT {
			t.Errorf("resolveMode(%v, false) = %v, want modeSRT", mode, got)
		}
		if note == "" {
			t.Errorf("resolveMode(%v, false): want an explanatory note, got empty string", mode)
		}
	}
}

func TestResolveModeLeavesEmbeddableContainersAlone(t *testing.T) {
	for _, mode := range []outputMode{modeSRT, modeMux, modeBoth} {
		got, note := resolveMode(mode, true)
		if got != mode {
			t.Errorf("resolveMode(%v, true) = %v, want unchanged", mode, got)
		}
		if note != "" {
			t.Errorf("resolveMode(%v, true): want no note, got %q", mode, note)
		}
	}
}

func TestResolveModeSRTNeedsNoDowngrade(t *testing.T) {
	got, note := resolveMode(modeSRT, false)
	if got != modeSRT {
		t.Errorf("resolveMode(modeSRT, false) = %v, want modeSRT", got)
	}
	if note != "" {
		t.Errorf("want no note when nothing was downgraded, got %q", note)
	}
}

// contains is a tiny helper so the test file needs no extra imports.
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./ -run TestParseMode -v`

Expected: FAIL — compile error, `undefined: parseMode`, `undefined: modeSRT`, `undefined: outputMode`.

- [ ] **Step 3: Write the minimal implementation**

Create `mode.go`:

```go
package main

import (
	"fmt"
	"strings"
)

// outputMode selects what the tool produces for a translated subtitle track.
type outputMode int

const (
	// modeSRT writes a sidecar .srt next to the input video. This is the
	// default: it never rewrites the video file and works on any container.
	modeSRT outputMode = iota
	// modeMux writes a new container with the translated track embedded.
	modeMux
	// modeBoth writes the muxed container and the sidecar .srt.
	modeBoth
)

func (m outputMode) String() string {
	switch m {
	case modeMux:
		return "mux"
	case modeBoth:
		return "both"
	default:
		return "srt"
	}
}

// writesSRT reports whether the mode produces a sidecar .srt file.
func (m outputMode) writesSRT() bool { return m == modeSRT || m == modeBoth }

// writesContainer reports whether the mode produces a muxed video file.
func (m outputMode) writesContainer() bool { return m == modeMux || m == modeBoth }

func parseMode(s string) (outputMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "srt":
		return modeSRT, nil
	case "mux":
		return modeMux, nil
	case "both":
		return modeBoth, nil
	default:
		return 0, fmt.Errorf("invalid -mode %q: want srt, mux or both", s)
	}
}

// resolveMode downgrades container-writing modes when the input container
// cannot hold subtitle tracks (AVI). It returns the effective mode and a
// human-readable note, empty when nothing was downgraded.
func resolveMode(m outputMode, canEmbed bool) (outputMode, string) {
	if canEmbed || !m.writesContainer() {
		return m, ""
	}
	return modeSRT, fmt.Sprintf(
		"Note: this container can't embed subtitle tracks; -mode %s downgraded to srt.", m)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./ -v`

Expected: PASS for all six test functions.

- [ ] **Step 5: Check formatting and vet**

Run: `gofmt -l . && go vet ./...`

Expected: no output from either command.

- [ ] **Step 6: Commit**

```bash
git add mode.go mode_test.go
git commit -m "feat: add outputMode type for srt/mux/both selection"
```

---

### Task 2: Wire the mode into the CLI

**Files:**
- Modify: `main.go` — flag block (lines 44–53), mode parsing after the version check, the output section (lines 148–185), and the `usage` const (lines 14–42)

**Interfaces:**
- Consumes from Task 1: `parseMode(string) (outputMode, error)`, `resolveMode(outputMode, bool) (outputMode, string)`, `outputMode.writesSRT()`, `outputMode.writesContainer()`, constants `modeSRT`/`modeMux`/`modeBoth`.
- Consumes from `internal/media` (already exists, do not change): `ContainerSupportsEmbeddedSubs(path string) bool`, `DefaultSRTPath(input, lang string) string`, `DefaultOutputPath(input, lang string) string`, `MuxSubtitle(input, srtPath, lang, title, output string) error`.
- Produces: no new exported surface. The deliverable is CLI behaviour.

- [ ] **Step 1: Replace the two removed flags with `-mode`**

In `main.go`, inside `func main()`, delete these two lines:

```go
	saveSRT := flag.Bool("srt", false, "save translated .srt alongside MKV")
	noMux := flag.Bool("no-mux", false, "skip muxing back into MKV")
```

and change the `-out` description, so the flag block reads:

```go
	from := flag.String("from", "en", "source language")
	to := flag.String("to", "", "target language (required)")
	track := flag.Int("track", -1, "subtitle stream index (-1 = auto)")
	modeFlag := flag.String("mode", "srt", "output mode: srt, mux or both")
	out := flag.String("out", "", "output path (.srt in srt mode, container otherwise)")
	showVersion := flag.Bool("version", false, "print version and exit")
```

- [ ] **Step 2: Parse the mode right after the version check**

Immediately after the `if *showVersion { ... }` block and before the `if flag.NArg() < 1` check, insert:

```go
	mode, err := parseMode(*modeFlag)
	if err != nil {
		fatalf("%v", err)
	}
```

Note: `err` is declared here with `:=`. Further down, `streams, err := media.Probe(input)` currently uses `:=` too — that stays valid because `streams` is new, so no edit is needed there.

- [ ] **Step 3: Rewrite the output section**

Replace everything from the `// AVI can't embed subtitle tracks — force SRT-only output` comment through the closing brace of the `if writePath == srtOut { ... }` block (currently lines 148–185) with:

```go
	// AVI and friends can't embed subtitle tracks — fall back to a sidecar.
	mode, note := resolveMode(mode, media.ContainerSupportsEmbeddedSubs(input))
	if note != "" {
		fmt.Println(note)
	}

	tmpTranslated := filepath.Join(os.TempDir(), "sub_translator_dst.srt")
	defer os.Remove(tmpTranslated)

	if mode.writesSRT() {
		srtOut := media.DefaultSRTPath(input, *to)
		if mode == modeSRT && *out != "" {
			srtOut = *out
		}
		if err := srt.Write(srtOut, outBlocks); err != nil {
			fatalf("write SRT: %v", err)
		}
		fmt.Printf("Saved SRT: %s\n", srtOut)
	}

	if !mode.writesContainer() {
		fmt.Println("Done.")
		return
	}

	if err := srt.Write(tmpTranslated, outBlocks); err != nil {
		fatalf("write temp SRT: %v", err)
	}
```

`mode, note := resolveMode(...)` compiles because `note` is new; `mode` is reassigned in the same scope. `-out` steers the sidecar only in pure `srt` mode; in `mux` and `both` it names the container, which the existing mux block below already handles via `outMKV := *out`.

- [ ] **Step 4: Update the usage text**

In the `usage` const at the top of `main.go`, replace the `Flags:` and `Examples:` sections so the whole const reads:

```go
const usage = `sub-translator — subtitle translator for MKV, MP4, AVI and more

Usage:
  sub-translator [flags] <input>

Output modes (-mode):
  srt   write a translated .srt next to the input video (default)
  mux   write a new video file with the translated track embedded
  both  write the muxed video file and the .srt

Supported formats:
  MKV, MP4/M4V/MOV  — translated track embedded into output file
  AVI               — embedded subs not supported; -mode mux/both fall back to srt

Flags:
  -from    source language code (default: en)
  -to      target language code (required; prompted for if omitted)
  -track   subtitle stream index, -1 = auto-detect by -from lang (default: -1)
  -mode    output mode: srt, mux or both (default: srt)
  -out     output path (.srt in srt mode, container otherwise)
  -version print version and exit

Examples:
  sub-translator -to es movie.mkv
  sub-translator -to fr -mode mux movie.mp4
  sub-translator -to de -mode both movie.mkv
  sub-translator -to ru -track 3 movie.mkv
`
```

- [ ] **Step 5: Build and confirm the removed flags are gone**

```bash
make build
./bin/sub-translator -no-mux -to es /dev/null; echo "exit=$?"
```

Expected: `flag provided but not defined: -no-mux`, the usage text, and `exit=2`.

- [ ] **Step 6: Confirm an invalid mode is rejected**

```bash
./bin/sub-translator -mode nomux -to es /dev/null; echo "exit=$?"
```

Expected: `error: invalid -mode "nomux": want srt, mux or both` and `exit=1`.

- [ ] **Step 7: Build a fixture and verify the default writes no video**

```bash
SCRATCH=$(mktemp -d)
printf '1\n00:00:00,000 --> 00:00:01,000\nhello\n\n' > "$SCRATCH/a.srt"
ffmpeg -y -v error -f lavfi -i testsrc=duration=2:size=160x120:rate=5 -i "$SCRATCH/a.srt" \
  -map 0:v -map 1:0 -c:v libx264 -c:s srt "$SCRATCH/movie.mkv"

./bin/sub-translator -to es -track 1 "$SCRATCH/movie.mkv" 2>/dev/null | tail -3
ls "$SCRATCH"
```

Expected: output ends with `Saved SRT: .../movie.es.srt` then `Done.`; `ls` shows `movie.mkv` and `movie.es.srt` and **no** `movie.ES.mkv`.

- [ ] **Step 8: Verify `-mode mux` writes only the container**

```bash
rm -f "$SCRATCH/movie.es.srt"
./bin/sub-translator -to es -track 1 -mode mux "$SCRATCH/movie.mkv" 2>/dev/null | tail -2
ls "$SCRATCH"
ffprobe -v quiet -print_format json -show_streams "$SCRATCH/movie.ES.mkv" \
  | jq -r '.streams[] | "\(.index) \(.codec_type) \(.tags.language // "-")"'
```

Expected: `ls` shows `movie.ES.mkv` and **no** `movie.es.srt`; ffprobe shows a second subtitle stream tagged `es`.

- [ ] **Step 9: Verify `-mode both` writes both artifacts**

```bash
rm -f "$SCRATCH/movie.ES.mkv"
./bin/sub-translator -to es -track 1 -mode both "$SCRATCH/movie.mkv" 2>/dev/null | tail -2
ls "$SCRATCH"
```

Expected: `ls` shows both `movie.ES.mkv` and `movie.es.srt`.

- [ ] **Step 10: Verify the AVI downgrade**

```bash
ffmpeg -y -v error -i "$SCRATCH/movie.mkv" -map 0:v -c:v mpeg4 "$SCRATCH/movie.avi"
./bin/sub-translator -to es -track 1 -mode mux "$SCRATCH/movie.mkv" -out "$SCRATCH/ignored.mkv" >/dev/null 2>&1
./bin/sub-translator -to es -track 1 -mode mux "$SCRATCH/movie.mkv" 2>/dev/null | grep -i "downgraded" || echo "NO DOWNGRADE NOTE (expected for mkv)"
```

Expected: the MKV run prints no downgrade note. To exercise the downgrade itself the input must be an AVI carrying a subtitle track, which AVI cannot hold — so instead verify the unit test `TestResolveModeDowngradesWhenContainerCannotEmbed` from Task 1 covers it, and confirm here only that MKV is left alone.

- [ ] **Step 11: Run the full validation suite**

Run: `make validate`

Expected: `gofmt` silent, `go vet` silent, all Task 1 tests PASS.

- [ ] **Step 12: Commit**

```bash
git add main.go
git commit -m "feat: add -mode flag, default to writing a sidecar .srt

Replaces -srt and -no-mux with -mode srt|mux|both. The default is now srt,
so a plain run writes movie.es.srt next to the input and never rewrites the
video. -mode mux restores the previous embed-into-a-new-container behaviour
and -mode both does the two together."
```

---

### Task 3: Documentation

**Files:**
- Modify: `README.md` — the Features list, Supported Formats table, Usage flag block, Examples, Output naming table, and the `-to` note paragraph

**Interfaces:**
- Consumes from Task 2: the `-mode` flag and its three values; the fact that `-srt` and `-no-mux` no longer exist.
- Produces: nothing consumed by later tasks. This is the final task.

- [ ] **Step 1: Add a mode bullet to the Features list**

In the `## Features` section, insert this bullet directly after the `**Automatic track detection**` bullet:

```markdown
- **Sidecar or embedded** — writes a `.srt` next to your video by default, or embeds the translated track into a new container with `-mode mux`
```

- [ ] **Step 2: Update the Supported Formats table**

Replace the table under `## Supported Formats` with:

```markdown
| Container | `-mode srt` (default) | `-mode mux` | `-mode both` |
|-----------|:---------------------:|:-----------:|:------------:|
| MKV       | ✅ sidecar `.srt`     | ✅ embedded | ✅ |
| MP4 / M4V / MOV | ✅ sidecar `.srt` | ✅ embedded (`mov_text`) | ✅ |
| AVI       | ✅ sidecar `.srt`     | falls back to sidecar | falls back to sidecar |

AVI cannot carry subtitle tracks, so `-mode mux` and `-mode both` downgrade to a sidecar `.srt` and say so.
```

- [ ] **Step 3: Replace the Usage flag block**

Replace the fenced flag block under `## Usage` with:

```
Flags:
  -from    source language code  (default: en)
  -to      target language code  (required — prompted for if omitted)
  -track   subtitle stream index, -1 = auto-detect (default: -1)
  -mode    output mode: srt, mux or both  (default: srt)
  -out     output path (.srt in srt mode, container otherwise)
  -version print version and exit
```

Then add this paragraph immediately below that block:

```markdown
### Output modes

| Mode | What you get | When to use it |
|------|--------------|----------------|
| `srt` *(default)* | `movie.es.srt` next to the video | Fastest, non-destructive — your video file is never rewritten. Works with any player that loads sidecar subtitles. |
| `mux` | `movie.ES.mkv` with the translated track embedded | One self-contained file to copy to a TV, phone or media server that ignores sidecars. |
| `both` | `movie.ES.mkv` **and** `movie.es.srt` | You want the embedded track but also a plain-text copy to edit or reuse. |
```

- [ ] **Step 4: Fix every example that uses a removed flag**

Replace the fenced block under `### Examples` with:

```bash
# Default: write movie.es.srt next to the video
sub-translator -to es movie.mkv

# Omit -to and you'll be asked for it
sub-translator movie.mkv
# Target language code (e.g. es, fr, de, ja): fr

# Embed the translated track into a new container
sub-translator -to fr -mode mux movie.mkv

# Embedded track plus a sidecar .srt
sub-translator -to de -mode both movie.mp4

# Pick a specific subtitle track by stream index
sub-translator -to ru -track 3 movie.mkv

# Custom output path (names the .srt in srt mode)
sub-translator -to es -out /tmp/movie_es.srt movie.mkv

# Custom output path (names the container in mux mode)
sub-translator -to es -mode mux -out /tmp/movie_es.mkv movie.mkv
```

- [ ] **Step 5: Update the Output naming table**

Replace the table under `### Output naming` with:

```markdown
| Input | Flags | Output |
|-------|-------|--------|
| `movie.mkv` | *(default)* | `movie.es.srt` |
| `movie.mkv` | `-mode mux` | `movie.ES.mkv` |
| `movie.mkv` | `-mode both` | `movie.ES.mkv` + `movie.es.srt` |
| `movie.mp4` | `-to fr -mode mux` | `movie.FR.mp4` |
| `movie.avi` | `-mode mux` | `movie.es.srt` *(downgraded)* |
```

- [ ] **Step 6: Update the "The Problem" table row about AVI**

In the `## The Problem` table, replace the right-hand cell of the AVI row with:

```markdown
Detects the container limitation and writes an external `.srt` next to the video instead — which is also what the default mode does for every container.
```

- [ ] **Step 7: Verify no stale flag references remain**

Run: `grep -n -- "-no-mux\|-srt " README.md main.go`

Expected: no matches. (`-srt` followed by a space would only appear in an old example; the `.srt` file extension is unaffected by this pattern.)

- [ ] **Step 8: Confirm the README matches the binary**

```bash
make build && ./bin/sub-translator -h 2>&1 | head -30
```

Expected: the usage text lists `-mode` with the three values, and lists no `-srt` or `-no-mux`.

- [ ] **Step 9: Commit**

```bash
git add README.md
git commit -m "docs: document -mode srt|mux|both and the new sidecar default"
```

---

## Self-Review

**Spec coverage**

| Requirement from the request | Task |
|---|---|
| Mode selecting inject-into-video vs standalone sidecar | Task 1 (type + parsing), Task 2 (wiring) |
| Sidecar is the **default** | Task 2 Step 1 (`flag.String("mode", "srt", ...)`), verified in Task 2 Step 7 |
| Existing inject behaviour still reachable | Task 2 Step 1 (`-mode mux`), verified in Task 2 Step 8 |
| `-srt` / `-no-mux` removed per the answered question | Task 2 Step 1, verified in Task 2 Step 5 |
| `both` mode from the chosen flag design preview | Task 1 (`modeBoth`), Task 2 Step 9 |

**Placeholder scan:** No `TBD`, no "add error handling", no "similar to Task N". Every code step carries the literal code to write; every verification step carries the command and its expected output.

**Type consistency:** `parseMode`, `resolveMode`, `writesSRT`, `writesContainer`, `modeSRT`, `modeMux`, `modeBoth` are spelled identically in Task 1's implementation, Task 1's tests, and Task 2's wiring. `media.ContainerSupportsEmbeddedSubs`, `media.DefaultSRTPath`, `media.DefaultOutputPath` match the existing signatures in `internal/media/media.go`.

**Known rough edge, deliberately left in:** Task 2 Step 10 cannot fully exercise the AVI downgrade end-to-end, because producing an AVI that carries a subtitle track to translate is impossible — AVI is exactly the container that cannot hold one. The downgrade path is therefore covered by the unit test in Task 1 rather than a CLI run, and Step 10 only confirms MKV is left alone. This is a real limit of the fixture, not a gap in the logic.
