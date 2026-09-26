# CLI Event Protocol Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the CLI a machine-readable event stream (`-events json`), a probe-only mode (`-probe`), and error kinds, so a wrapper can drive it without parsing prose — and so anyone scripting the tool gets the same benefit.

**Architecture:** A small `internal/events` package owns the event types and a nil-safe `Emitter` that writes newline-delimited JSON to a writer. `main` holds one package-level emitter, disabled unless `-events json` is passed, and calls it alongside the existing human output rather than instead of it. Error kinds arrive through a `fatalKindf` variant so the thirty existing `fatalf` call sites do not all have to change.

**Tech Stack:** Go 1.26, standard library only.

**Spec:** `docs/superpowers/specs/2026-09-26-desktop-gui-design.md` (Part 1)

## Global Constraints

- Go version floor: **1.26** (`go.mod` says `go 1.26.2`) — do not lower it, do not add a toolchain directive.
- **No new third-party dependencies.** The root `go.mod` has zero requires today and must still have zero when this plan is done.
- All code must pass `make validate` (runs `gofmt -w .`, `go vet ./...`, `go test ./... -v`) and `go test -race ./...`.
- **Without `-events json` the CLI's output must not change by one character.** This is the binding constraint of the plan; every task is subject to it.
- Events go to **stderr**. stdout keeps carrying the human output, so the two compose.
- `-probe` does no work, writes no files, and needs no API key.
- No test may execute `whisper-cli`, `ffmpeg`, `ffprobe`, or reach the network. Use the stub-binary pattern already in `progressive_e2e_test.go` and `httptest`.
- Conventional commit messages, matching the existing history.
- Do not reformat or restructure code unrelated to this feature.
- Module path is `github.com/artschekoff/sub-translator`.

---

## File Structure

| File | Status | Responsibility |
|---|---|---|
| `internal/events/events.go` | Create | Event types, the `Emitter`, NDJSON encoding. No knowledge of the pipeline. |
| `internal/events/events_test.go` | Create | Encoding shape, nil-safety, concurrency, unknown-field absence. |
| `errkind.go` | Create | The `errKind` enumeration and `fatalKindf`, in package `main` beside `fatalf`. |
| `errkind_test.go` | Create | Kind values are stable strings; the enumeration is complete. |
| `main.go` | Modify | `-events` and `-probe` flags, the package-level emitter, probe-only path, event calls, classified `fatalKindf` call sites. |
| `progressive.go` | Modify | Stage, progress, saved and warning events from the chunk loop. |
| `main_test.go` | Modify | Subprocess tests: stdout unchanged, `-probe` does nothing, every classified error carries its kind. |
| `internal/media/media.go` | Modify | `ContainerDuration`, for the probe event's running time. |
| `internal/media/media_test.go` | Modify | Argument construction for the duration probe. |
| `README.md` | Modify | Document `-events json`, `-probe`, the event list and the error kinds. |

`internal/whisper`, `internal/srt`, `internal/translate` and `internal/config` are **not modified** by this plan. `internal/media` gains one function and is otherwise untouched.

---

### Task 1: The events package

**Files:**
- Create: `internal/events/events.go`
- Test: `internal/events/events_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces, in package `events`:
  - `type Kind string` with constants `KindVersion`, `KindProbe`, `KindStage`, `KindProgress`, `KindLanguage`, `KindSaved`, `KindWarning`, `KindDone`, `KindError`
  - `type Stream struct { Index int; Lang, Title, Codec string }`
  - `type Event interface { … }` (unexported methods)
  - Event structs: `Version`, `Probe`, `Stage`, `Progress`, `Language`, `Saved`, `Warning`, `Done`, `Error`
  - `type Emitter struct { … }`
  - `func New(w io.Writer) *Emitter` — a nil writer yields a disabled emitter
  - `func (e *Emitter) Emit(ev Event)` — safe on a nil receiver and when disabled
  - `const ProtocolVersion = 1`

- [ ] **Step 1: Write the failing test**

Create `internal/events/events_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/events/ -v`
Expected: FAIL — the package does not exist yet.

- [ ] **Step 3: Write minimal implementation**

Create `internal/events/events.go`:

```go
// Package events carries the machine-readable stream the CLI writes under
// -events json: one JSON object per line, so a wrapper can drive the tool
// without parsing output meant for a person.
//
// The stream is deliberately one-way. The caller states its intent in argv and
// reads what happened; nothing here asks the caller a question, which is what
// separates this from a JSON-RPC protocol.
package events

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// ProtocolVersion is carried by the first event of every run. A consumer that
// understands fewer versions than it sees should say so rather than guess.
const ProtocolVersion = 1

type Kind string

const (
	KindVersion  Kind = "version"
	KindProbe    Kind = "probe"
	KindStage    Kind = "stage"
	KindProgress Kind = "progress"
	KindLanguage Kind = "language"
	KindSaved    Kind = "saved"
	KindWarning  Kind = "warning"
	KindDone     Kind = "done"
	KindError    Kind = "error"
)

// Stream is one audio or subtitle track as the probe reports it.
type Stream struct {
	Index int    `json:"index"`
	Lang  string `json:"lang"`
	Title string `json:"title"`
	Codec string `json:"codec"`
}

// base is embedded by every event so the type tag and timestamp are written the
// same way everywhere and cannot be forgotten at a call site.
type base struct {
	Type Kind   `json:"type"`
	T    string `json:"t"`
}

func (b *base) setBase(k Kind, t time.Time) {
	b.Type = k
	b.T = t.UTC().Format(time.RFC3339)
}

// Event is implemented by embedding base. The unexported methods keep the set
// closed to this package, so a consumer's switch cannot be surprised.
type Event interface {
	kind() Kind
	setBase(Kind, time.Time)
}

type Version struct {
	base
	Events int    `json:"events"`
	Tool   string `json:"tool"`
}

func (*Version) kind() Kind { return KindVersion }

type Probe struct {
	base
	Duration  float64  `json:"duration,omitempty"`
	Audio     []Stream `json:"audio"`
	Subtitles []Stream `json:"subtitles"`
}

func (*Probe) kind() Kind { return KindProbe }

type Stage struct {
	base
	Name  string `json:"name"`
	Chunk int    `json:"chunk,omitempty"`
	Of    int    `json:"of,omitempty"`
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
}

func (*Stage) kind() Kind { return KindStage }

type Progress struct {
	base
	Pct int `json:"pct"`
}

func (*Progress) kind() Kind { return KindProgress }

type Language struct {
	base
	Detected string `json:"detected"`
}

func (*Language) kind() Kind { return KindLanguage }

type Saved struct {
	base
	Path      string `json:"path"`
	Covers    string `json:"covers"`
	Watchable bool   `json:"watchable"`
}

func (*Saved) kind() Kind { return KindSaved }

type Warning struct {
	base
	Kind  string `json:"kind"`
	Count int    `json:"count"`
	Total int    `json:"total"`
	First []int  `json:"first,omitempty"`
}

func (*Warning) kind() Kind { return KindWarning }

type Done struct {
	base
	Path   string `json:"path"`
	Covers string `json:"covers"`
}

func (*Done) kind() Kind { return KindDone }

type Error struct {
	base
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

func (*Error) kind() Kind { return KindError }

// Emitter writes events as newline-delimited JSON. A zero writer disables it,
// so every call site can emit unconditionally instead of guarding.
type Emitter struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
}

func New(w io.Writer) *Emitter {
	return &Emitter{w: w, now: time.Now}
}

// Emit writes one event. It is safe on a nil receiver and on a disabled
// emitter, and safe to call from several goroutines: translation runs a worker
// pool whose progress callbacks arrive concurrently, and interleaved writes
// would produce lines no consumer could parse.
func (e *Emitter) Emit(ev Event) {
	if e == nil || e.w == nil {
		return
	}
	ev.setBase(ev.kind(), e.now())

	line, err := json.Marshal(ev)
	if err != nil {
		// An event that cannot be encoded is a programming error, not a
		// condition the run should die of: the human output still carries
		// everything a person needs.
		return
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.w.Write(append(line, '\n'))
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/events/ -race -v && make validate`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/events/
git commit -m "feat: add the machine-readable event stream types"
```

---

### Task 2: Error kinds

**Files:**
- Create: `errkind.go`
- Test: `errkind_test.go`

**Interfaces:**
- Consumes: the existing `fatalf` and `cleanupRunTmpDir` in package `main`.
- Produces, in package `main`:
  - `type errKind string` with constants `errMissingKey`, `errWhisperNotFound`, `errNoModel`, `errAmbiguousModel`, `errNoAudioTrack`, `errNoSubtitleTrack`, `errProbeFailed`, `errRateLimited`, `errTranscriptEmpty`, `errCancelled`, `errOther`
  - `func allErrKinds() []errKind`
  - `func fatalKindf(kind errKind, format string, args ...any)`

Note for the implementer: `fatalf` stays exactly as it is and keeps its thirty call sites. `fatalKindf` is the variant used where a caller can act on the reason; everything else reports `errOther` implicitly by staying on `fatalf`.

- [ ] **Step 1: Write the failing test**

Create `errkind_test.go`:

```go
package main

import (
	"slices"
	"testing"
)

// These strings are a contract with anything driving the tool. Renaming one
// silently breaks a consumer that switches on it, so they are pinned here
// rather than left to whatever the constant happens to say.
func TestErrKindsAreTheDocumentedStrings(t *testing.T) {
	want := map[errKind]string{
		errMissingKey:      "missing_key",
		errWhisperNotFound: "whisper_not_found",
		errNoModel:         "no_model",
		errAmbiguousModel:  "ambiguous_model",
		errNoAudioTrack:    "no_audio_track",
		errNoSubtitleTrack: "no_subtitle_track",
		errProbeFailed:     "probe_failed",
		errRateLimited:     "rate_limited",
		errTranscriptEmpty: "transcript_empty",
		errCancelled:       "cancelled",
		errOther:           "other",
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run TestErrKind -v`
Expected: FAIL — `errKind` and its constants are undefined.

- [ ] **Step 3: Write minimal implementation**

Create `errkind.go`:

```go
package main

// errKind classifies a fatal error so a program driving this tool can act on the
// reason instead of matching on the wording. The strings are a contract: a
// wrapper switches on them, so they are pinned by a test and must not be
// renamed casually.
type errKind string

const (
	errMissingKey      errKind = "missing_key"
	errWhisperNotFound errKind = "whisper_not_found"
	errNoModel         errKind = "no_model"
	errAmbiguousModel  errKind = "ambiguous_model"
	errNoAudioTrack    errKind = "no_audio_track"
	errNoSubtitleTrack errKind = "no_subtitle_track"
	errProbeFailed     errKind = "probe_failed"
	errRateLimited     errKind = "rate_limited"
	errTranscriptEmpty errKind = "transcript_empty"
	errCancelled       errKind = "cancelled"

	// errOther is what an unclassified failure reports. It exists so a consumer
	// always receives a kind, never an absent field.
	errOther errKind = "other"
)

// allErrKinds lists every kind, fallback last, for the test that keeps this file
// and the documentation in step.
func allErrKinds() []errKind {
	return []errKind{
		errMissingKey,
		errWhisperNotFound,
		errNoModel,
		errAmbiguousModel,
		errNoAudioTrack,
		errNoSubtitleTrack,
		errProbeFailed,
		errRateLimited,
		errTranscriptEmpty,
		errCancelled,
		errOther,
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test . -run TestErrKind -v && make validate`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add errkind.go errkind_test.go
git commit -m "feat: classify fatal errors with a stable kind"
```

---

### Task 3: The `-events` flag and `fatalKindf`

**Files:**
- Modify: `main.go`
- Modify: `errkind.go`

**Interfaces:**
- Consumes: `events.New`, `events.Emitter`, `events.Version`, `events.Error`, `events.ProtocolVersion` (Task 1); `errKind` and its constants (Task 2).
- Produces, in package `main`:
  - package-level `var ev *events.Emitter`
  - `func fatalKindf(kind errKind, format string, args ...any)`

- [ ] **Step 1: Add the emitter and the classified exit**

In `main.go`, add the package-level emitter next to the existing `runTmpDir` declaration:

```go
// ev carries the machine-readable event stream. It is disabled unless
// -events json is passed, and Emit is a no-op when disabled, so the call sites
// below need no guards.
var ev *events.Emitter
```

Append to `errkind.go`:

```go
// fatalKindf reports a fatal error with a kind attached, for the failures a
// caller can do something about. Everything else keeps using fatalf, which
// reports no kind because there is nothing useful to say about it.
func fatalKindf(kind errKind, format string, args ...any) {
	ev.Emit(&events.Error{Kind: string(kind), Message: fmt.Sprintf(format, args...)})
	fatalf("%s", fmt.Sprintf(format, args...))
}
```

Add `"fmt"` and `"github.com/artschekoff/sub-translator/internal/events"` to `errkind.go`'s imports.

- [ ] **Step 2: Declare the flag and construct the emitter**

Declare the flag beside the existing ones in `main.go`:

```go
	eventsFlag := flag.String("events", "", "emit machine-readable events on stderr: json")
```

Immediately after `flag.Parse()` and the `-version` check, before anything else happens:

```go
	switch *eventsFlag {
	case "":
		// Disabled: Emit is a no-op, so nothing below needs a guard.
	case "json":
		ev = events.New(os.Stderr)
		ev.Emit(&events.Version{Events: events.ProtocolVersion, Tool: version})
	default:
		fatalf("invalid -events %q: the only supported value is json", *eventsFlag)
	}
```

Add `"github.com/artschekoff/sub-translator/internal/events"` to `main.go`'s imports.

- [ ] **Step 3: Write the failing test**

Append to `main_test.go`:

```go
// buildCLI compiles the tool once for a subprocess test and returns its path.
func buildCLI(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "sub-translator")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// The event stream is an addition, not a replacement: a person running the tool
// in a terminal must see exactly what they saw before. Anything else makes
// -events json a breaking change for every existing user and script.
func TestEventsFlagLeavesStdoutUnchanged(t *testing.T) {
	bin := buildCLI(t)

	run := func(extra ...string) (string, string) {
		t.Helper()
		args := append([]string{"-version"}, extra...)
		cmd := exec.Command(bin, args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		_ = cmd.Run()
		return stdout.String(), stderr.String()
	}

	plainOut, plainErr := run()
	jsonOut, jsonErr := run("-events", "json")

	if plainOut != jsonOut {
		t.Errorf("stdout differs with -events json:\nwithout: %q\nwith:    %q", plainOut, jsonOut)
	}
	if plainErr != "" {
		t.Errorf("a plain run wrote to stderr: %q", plainErr)
	}
	if !strings.Contains(jsonErr, `"type":"version"`) {
		t.Errorf("stderr carries no version event: %q", jsonErr)
	}
}

func TestEventsFlagRejectsAnythingButJSON(t *testing.T) {
	bin := buildCLI(t)
	cmd := exec.Command(bin, "-events", "yaml", "-to", "es", "movie.mkv")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("want a non-zero exit for an unsupported -events value")
	}
	if !strings.Contains(stderr.String(), "json") {
		t.Errorf("error %q does not name the supported value", stderr.String())
	}
}
```

Check `main_test.go`'s import block and add `"bytes"`, `"os/exec"`, `"path/filepath"` and `"strings"` if any are missing.

- [ ] **Step 4: Run tests**

Run: `go test . -run TestEvents -v && make validate`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add main.go errkind.go main_test.go
git commit -m "feat: add -events json and the classified fatal exit"
```

---

### Task 4: Classify the error call sites

**Files:**
- Modify: `main.go`

**Interfaces:**
- Consumes: `fatalKindf` and the `errKind` constants (Tasks 2 and 3).
- Produces: no new API.

Convert exactly these call sites from `fatalf` to `fatalKindf`, leaving the message text byte-identical. Find each with `grep -n "fatalf(" main.go`.

| Message begins with | Kind |
|---|---|
| `translation needs an OpenAI API key` | `errMissingKey` |
| `whisper-cli not found` / `whisper binary not found at` | `errWhisperNotFound` |
| `no whisper model found` | `errNoModel` |
| `whisper model not found:` | `errNoModel` |
| `several whisper models found` | `errAmbiguousModel` |
| `no audio tracks in` | `errNoAudioTrack` |
| `no audio stream at index` | `errNoAudioTrack` |
| `no subtitle tracks in` | `errNoSubtitleTrack` |
| `no subtitle stream at index` | `errNoSubtitleTrack` |
| `no subtitle track tagged lang=` | `errNoSubtitleTrack` |
| `probe failed:` | `errProbeFailed` |
| `input file not found:` | `errProbeFailed` |
| `transcript is empty` | `errTranscriptEmpty` |

Every other `fatalf` stays as it is.

The `errWhisperNotFound`, `errNoModel` and `errAmbiguousModel` messages come back from `resolveWhisper` as errors rather than being formatted at the call site, so classify them where that error is handled by matching on the wrapped error. Prefer `errors.Is` against sentinels if `internal/whisper` already exports them; if it does not, match the prefix with `strings.HasPrefix` and say so in your report — do not modify `internal/whisper`, which is out of scope for this plan.

`errRateLimited` is reported where a total translation failure is handled: the error returned by `TranslateAll` when every block failed. `errCancelled` is reported from the signal handler, before `os.Exit(130)`.

- [ ] **Step 1: Write the failing test**

Append to `main_test.go`:

```go
// A wrapper routes on the kind — a missing key sends the user to settings, a
// missing model offers a download. An unclassified failure would leave it with
// nothing to do but show red text.
func TestClassifiedErrorsCarryTheirKind(t *testing.T) {
	bin := buildCLI(t)
	cfg := t.TempDir()

	tests := []struct {
		name string
		args []string
		env  []string
		kind string
	}{
		{
			name: "missing key",
			args: []string{"-events", "json", "-to", "es", "movie.mkv"},
			kind: "missing_key",
		},
		{
			name: "input file not found",
			args: []string{"-events", "json", "-to", "es", "/nonexistent/movie.mkv"},
			env:  []string{"OPENAI_API_KEY=sk-test"},
			kind: "probe_failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(bin, tt.args...)
			cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+cfg)
			cmd.Env = append(cmd.Env, tt.env...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err == nil {
				t.Fatalf("want a non-zero exit\nstderr: %s", stderr.String())
			}

			var found string
			for _, line := range strings.Split(stderr.String(), "\n") {
				var m map[string]any
				if json.Unmarshal([]byte(line), &m) != nil {
					continue
				}
				if m["type"] == "error" {
					found, _ = m["kind"].(string)
				}
			}
			if found != tt.kind {
				t.Errorf("error kind = %q, want %q\nstderr: %s", found, tt.kind, stderr.String())
			}
		})
	}
}
```

Add `"encoding/json"` and `"os"` to the import block if missing.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run TestClassifiedErrors -v`
Expected: FAIL — no `error` event is emitted, so `found` is empty.

- [ ] **Step 3: Convert the call sites**

Work through the table above. For example:

```go
	if _, err := os.Stat(input); err != nil {
		fatalKindf(errProbeFailed, "input file not found: %s", input)
	}
```

- [ ] **Step 4: Run tests**

Run: `go test . -v && make validate`
Expected: PASS, every pre-existing test included.

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: attach error kinds to the failures a caller can act on"
```

---

### Task 5: `-probe`

**Files:**
- Modify: `main.go`
- Test: `main_test.go`

**Interfaces:**
- Consumes: `events.Probe`, `events.Stream` (Task 1); the existing `media.Probe`, `media.AudioStreams`, `media.SubtitleStreams`, `formatTracks`.
- Produces: no new API.

- [ ] **Step 1: Write the failing test**

Append to `main_test.go`:

```go
// -probe is what a wrapper calls to build a track list before the user has
// chosen anything, so it must need nothing: no API key, no target language, and
// it must not write a file or start any work.
func TestProbeNeedsNoKeyAndDoesNoWork(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()

	// A stub ffprobe: -probe must go no further than this.
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "calls.log")
	stub := "#!/bin/sh\necho \"$@\" >> " + log + "\n" + `cat <<'JSON'
{"streams":[
 {"index":0,"codec_type":"video","codec_name":"h264","tags":{"language":"rus"}},
 {"index":1,"codec_type":"audio","codec_name":"ac3","tags":{"language":"eng","title":"Original"}}
],"format":{"duration":"120.5"}}
JSON`
	if err := os.WriteFile(filepath.Join(binDir, "ffprobe"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	film := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(film, []byte("not a film"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "-events", "json", "-probe", film)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "cfg"),
	)
	cmd.Env = append(cmd.Env, "OPENAI_API_KEY=")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("probe failed: %v\nstderr: %s", err, stderr.String())
	}

	var probe map[string]any
	for _, line := range strings.Split(stderr.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["type"] == "probe" {
			probe = m
		}
	}
	if probe == nil {
		t.Fatalf("no probe event:\n%s", stderr.String())
	}
	audio, _ := probe["audio"].([]any)
	if len(audio) != 1 {
		t.Errorf("audio = %#v, want one track", probe["audio"])
	}
	if probe["duration"] != 120.5 {
		t.Errorf("duration = %v, want 120.5", probe["duration"])
	}

	// Nothing beyond the probe may have run.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".srt") {
			t.Errorf("-probe wrote %s", e.Name())
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run TestProbeNeedsNoKey -v`
Expected: FAIL — `-probe` is not a flag.

- [ ] **Step 3: Implement**

Declare the flag beside the others:

```go
	probeOnly := flag.Bool("probe", false, "list the file's tracks and exit")
```

Immediately after the input path is known and `media.Probe` has returned — and **before** the translate options are validated, so `-probe` needs no key — insert:

```go
	if *probeOnly {
		emitProbe(input, streams)
		return
	}
```

Add the helper at the bottom of `main.go`:

```go
// emitProbe reports what the file contains and nothing else. It is what a
// wrapper calls to build a track list before the user has chosen anything, so
// it must not need a key, a target language, or do any work.
func emitProbe(input string, streams []media.Stream) {
	toEvent := func(list []media.Stream) []events.Stream {
		out := make([]events.Stream, 0, len(list))
		for _, s := range list {
			out = append(out, events.Stream{
				Index: s.Index,
				Lang:  s.Tags.Language,
				Title: s.Tags.Title,
				Codec: s.CodecName,
			})
		}
		return out
	}

	subs := media.SubtitleStreams(streams)
	p := &events.Probe{
		Audio:     toEvent(media.AudioStreams(streams)),
		Subtitles: toEvent(subs),
	}
	// A duration we cannot read is not worth failing over: the file is still
	// translatable, and the field is omitted rather than reported as zero.
	if d, err := media.ContainerDuration(input); err == nil {
		p.Duration = d.Seconds()
	}
	ev.Emit(p)

	if len(subs) > 0 {
		fmt.Print(formatTracks(subs))
	} else {
		fmt.Printf("No subtitle tracks in %s.\n", filepath.Base(input))
	}
}
```

**`media.ContainerDuration` does not exist yet, and this task adds it.** The duration is what a window shows as "2:19:08", so emitting a placeholder zero would be a lie the consumer cannot detect. `media.Probe` currently asks ffprobe for `-show_streams` only, and the duration lives in the `format` object, so add it here:

```go
// In internal/media/media.go, extend the parsed result and the ffprobe call.

type ProbeResult struct {
	Streams []Stream `json:"streams"`
	Format  struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// ContainerDuration reports how long the file runs. ffprobe gives it as a
// string of seconds in the format object, which is why Probe now asks for
// -show_format as well as -show_streams.
func ContainerDuration(path string) (time.Duration, error) {
	out, err := exec.Command("ffprobe",
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		path,
	).Output()
	if err != nil {
		return 0, fmt.Errorf("ffprobe duration: %w", err)
	}
	var result ProbeResult
	if err := json.Unmarshal(out, &result); err != nil {
		return 0, fmt.Errorf("parse ffprobe output: %w", err)
	}
	secs, err := strconv.ParseFloat(strings.TrimSpace(result.Format.Duration), 64)
	if err != nil {
		return 0, fmt.Errorf("parse duration %q: %w", result.Format.Duration, err)
	}
	return time.Duration(secs * float64(time.Second)), nil
}
```

Leave `Probe` itself untouched — a second, cheap ffprobe call is simpler than changing a function every other path depends on, and the probe-only mode is the only caller that needs a duration.

A failure here is **not fatal**: a container whose duration ffprobe cannot read is still perfectly translatable. Emit the probe event with `duration` absent (the field is `omitempty`) and carry on.

Add `"strconv"`, `"strings"` and `"time"` to `internal/media/media.go`'s imports if they are not already there — `strings` and `time` are.

- [ ] **Step 4: Run tests**

Run: `go test . -v && make validate`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: add -probe to list a file's tracks and exit"
```

---

### Task 6: Pipeline events

**Files:**
- Modify: `main.go`
- Modify: `progressive.go`

**Interfaces:**
- Consumes: everything from Tasks 1–5.
- Produces: no new API.

Emit alongside the existing human output — never instead of it. Every one of these sits next to a `fmt.Printf` that already says the same thing to a person.

| Where | Event |
|---|---|
| after `media.Probe` succeeds on a normal run | `&events.Probe{…}` — reuse the conversion from `emitProbe` |
| before audio extraction | `&events.Stage{Name: "extract"}` |
| before whisper on the whole file | `&events.Stage{Name: "transcribe"}` |
| whisper's progress callback | `&events.Progress{Pct: pct}` |
| after the language is detected | `&events.Language{Detected: lang}` |
| before translation | `&events.Stage{Name: "translate"}` |
| translation's progress callback | `&events.Progress{Pct: done * 100 / total}` |
| after the untranslated-blocks warning | `&events.Warning{Kind: "blocks_untranslated", Count: …, Total: …, First: …}` |
| after the final SRT is written | `&events.Done{Path: …, Covers: …}` |

In `progressive.go`'s chunk loop, additionally:

| Where | Event |
|---|---|
| at the top of each chunk | `&events.Stage{Name: "transcribe", Chunk: c.Index + 1, Of: len(plan), From: formatClock(c.Start), To: formatClock(c.Start + c.Dur)}` |
| after each chunk's file is written | `&events.Saved{Path: outPath, Covers: formatClock(covered), Watchable: true}` |

To reach the emitter from `progressive.go`, use the package-level `ev` — it is in package `main`, the same package.

Refactor `emitProbe`'s stream conversion into a small `probeEvent(streams []media.Stream) *events.Probe` so both the `-probe` path and the normal run use one implementation.

- [ ] **Step 1: Write the failing test**

Append to `main_test.go`. This drives a full `-fast` run against the stub binaries already used by `progressive_e2e_test.go` — read that file first and reuse its `progressiveStubs` helper rather than writing new stubs.

```go
// The event stream is what a wrapper renders its progress from. This pins the
// order and the presence of the events a run produces, so a refactor cannot
// quietly stop emitting one.
func TestFastRunEmitsTheExpectedEventSequence(t *testing.T) {
	stubs := progressiveStubs(t, 3)     // three chunks, as the e2e test sets up
	bin := buildCLI(t)

	cmd := exec.Command(bin,
		"-events", "json", "-fast", "-chunk", "1",
		"-to", "es", "-from", "en", "-source", "audio", stubs.film)
	cmd.Env = stubs.env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run failed: %v\nstderr: %s", err, stderr.String())
	}

	var kinds []string
	var savedCount int
	for _, line := range strings.Split(stderr.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		kinds = append(kinds, m["type"].(string))
		if m["type"] == "saved" {
			savedCount++
			if m["watchable"] != true {
				t.Errorf("saved event is not marked watchable: %v", m)
			}
		}
	}

	if len(kinds) == 0 || kinds[0] != "version" {
		t.Fatalf("first event = %v, want version", kinds)
	}
	for _, want := range []string{"probe", "stage", "progress", "saved", "done"} {
		if !slices.Contains(kinds, want) {
			t.Errorf("no %q event in the stream: %v", want, kinds)
		}
	}
	if savedCount != 3 {
		t.Errorf("%d saved events, want one per chunk (3)", savedCount)
	}
	if kinds[len(kinds)-1] != "done" {
		t.Errorf("last event = %q, want done", kinds[len(kinds)-1])
	}
}
```

Add `"slices"` to the import block if missing. If `progressiveStubs` does not expose `film` and `env` in the shape used above, adapt the call to whatever it returns — read it first and say in your report what you used.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run TestFastRunEmits -v`
Expected: FAIL — only the version event is emitted.

- [ ] **Step 3: Add the emit calls**

Work through both tables. Each call goes beside the `fmt.Printf` that already reports the same thing.

- [ ] **Step 4: Run tests**

Run: `go test . -race -v && make validate`
Expected: PASS.

Then confirm the binding constraint by hand and put the output in your report:

```bash
go build -o /tmp/st .
/tmp/st -version              > /tmp/a.out 2>/tmp/a.err
/tmp/st -version -events json > /tmp/b.out 2>/tmp/b.err
diff /tmp/a.out /tmp/b.out && echo "stdout identical"
```

- [ ] **Step 5: Commit**

```bash
git add main.go progressive.go main_test.go
git commit -m "feat: emit pipeline events alongside the human output"
```

---

### Task 7: Documentation

**Files:**
- Modify: `README.md`
- Modify: `main.go` (the `usage` constant)

- [ ] **Step 1: Add the flags to `usage`**

Add to the Flags block in `main.go`'s `usage` constant, keeping the existing alignment:

```
  -events        emit machine-readable events on stderr: json
  -probe         list the file's tracks and exit
```

- [ ] **Step 2: Add a README section**

After the Configuration section, add:

```markdown
## Driving it from another program

`-events json` makes the tool write newline-delimited JSON to **stderr**, one object per line, while stdout keeps the same human-readable output it has always had. Nothing changes without the flag.

```bash
sub-translator -events json -to es movie.mkv 2> events.jsonl
```

```json
{"type":"version","t":"…","events":1,"tool":"1.4.0"}
{"type":"probe","t":"…","duration":8348.674,"audio":[{"index":3,"lang":"eng","title":"Original","codec":"ac3"}],"subtitles":[]}
{"type":"stage","t":"…","name":"transcribe","chunk":1,"of":14,"from":"0:00:00","to":"0:10:00"}
{"type":"progress","t":"…","pct":42}
{"type":"saved","t":"…","path":"/…/movie.es.srt","covers":"0:10:00","watchable":true}
{"type":"done","t":"…","path":"/…/movie.es.srt","covers":"2:19:08"}
```

The first line is always a `version` event carrying the protocol number, so a consumer can refuse a stream it does not understand rather than guess.

`-probe` lists a file's tracks and exits without doing any work. It needs no API key and no target language:

```bash
sub-translator -events json -probe movie.mkv
```

Failures carry a `kind` so a program can act on the reason instead of matching on wording:

| `kind` | Meaning |
|---|---|
| `missing_key` | no OpenAI key in the environment or the config |
| `whisper_not_found` | `whisper-cli` is neither configured nor on `PATH` |
| `no_model` | no ggml model configured or discovered |
| `ambiguous_model` | several models found, none chosen |
| `no_audio_track` | the file has no audio to transcribe |
| `no_subtitle_track` | `-source sub` on a file with none |
| `probe_failed` | ffprobe could not read the file |
| `rate_limited` | the translation endpoint refused everything |
| `transcript_empty` | the audio produced no speech |
| `cancelled` | interrupted |
| `other` | anything not yet classified |

This is the same idea as `git status --porcelain` or `ffmpeg -progress`: a stable contract for programs, separate from the output meant for people.
```

- [ ] **Step 3: Add the flags to the README's flag list**

Mirror `usage` exactly.

- [ ] **Step 4: Verify**

Run: `make validate`, then `go build -o /tmp/st . && /tmp/st -h` and confirm the README's flag list matches the binary's output exactly.

- [ ] **Step 5: Commit**

```bash
git add README.md main.go
git commit -m "docs: document the event stream, -probe and the error kinds"
```

---

## Self-Review

**Spec coverage.** Part 1 of the spec has three pieces: `-events json` → Tasks 1, 3, 6; `-probe` → Task 5; typed errors → Tasks 2, 4. The event list in the spec maps one-to-one onto the table in Task 6, and the error-kind table onto Task 2's constants. The spec's constraint that stdout must not change is tested in Task 3 and verified by hand in Task 6. Part 2 of the spec (the GUI) is a separate plan and is deliberately not covered here.

**One deliberate widening.** Task 5 adds `media.ContainerDuration` to `internal/media`. The self-review caught the alternative — emitting a placeholder zero — as a lie the consumer could not detect, and the duration is what a window shows as the film's length. `Probe` itself is left alone; a second cheap ffprobe call beats changing a function every other path depends on.

**Type consistency.** `events.Emitter`, `events.New` and the event structs are named identically in Tasks 1, 3, 5 and 6. `errKind` and its constants match between Tasks 2, 3 and 4. `ev` is the package-level emitter in Tasks 3, 5 and 6. `probeEvent` is introduced in Task 6 as the shared conversion that Task 5's `emitProbe` originally held.

**Placeholders.** None: every step carries the code it asks for.
