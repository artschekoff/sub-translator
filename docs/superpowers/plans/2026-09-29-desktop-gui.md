# Desktop GUI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A macOS window you drop a film into, which shows its audio and subtitle tracks, translates the one you pick, and ships as an unsigned DMG.

**Architecture:** A Wails v2 app in `gui/` as its own Go module, so Wails and its cgo stay out of the root module, which keeps zero dependencies and cross-compiles to five platforms. The window never links the CLI's packages: it spawns `sub-translator` and reads its `-events json` stream, which is the contract the previous two branches built and tested. The frontend is plain HTML, CSS and JavaScript — three screens and a progress bar do not earn a node toolchain.

**Tech Stack:** Go 1.26, Wails v2 (pinned in `gui/go.mod`), plain HTML/CSS/JS, `sub-translator` as a subprocess.

**Spec:** `docs/superpowers/specs/2026-09-26-desktop-gui-design.md` — Part 2 is what this plan builds; Part 1 is the protocol it consumes and is already implemented and tested. Read both.

## Global Constraints

- **The root module keeps zero third-party dependencies.** `go.mod` at the repo root has no `require` block and `make build-all` cross-compiles to five platforms. Every Wails dependency lives in `gui/go.mod`. A task that adds anything to the root `go.mod` has failed.
- **The GUI never imports the CLI's packages.** It spawns the binary and reads its stream. This is what lets the CLI keep cross-compiling and the GUI keep cgo.
- **The GUI holds no copy of the CLI's rules.** Fast-mode availability comes from `probe.fast`; whether a container accepts an embedded track comes from `probe.embeds_subtitles`; every setting is read and written through `sub-translator config`. Reimplementing any of these is the defect this constraint exists to prevent.
- **Non-JSON lines on the CLI's stderr are human prose and must be skipped.** The CLI guarantees every line it writes is whole and that an event never starts mid-line; a `{`-leading line that does not parse is a bug to surface, not to ignore.
- **Exactly one terminal event per run** — one `done` or one `error` — and the exit code agrees with it: 0 after `done`, 1 after `error`, 2 for a usage error, 130 when a cancel is the ending.
- **No node toolchain.** No `package.json`, no bundler, no framework.
- **The window never changes size between screens.** One card, three contents.
- **The DMG is unsigned.** No signing, no notarisation, no Developer ID.

---

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `gui/go.mod`, `gui/go.sum` | the module that confines Wails | 1 |
| `gui/main.go` | Wails app, window options, the glass flags, `OnFileDrop` | 1, 7 |
| `gui/locate.go` + test | finding the CLI: bundle, `PATH`, remembered path | 2 |
| `gui/guistate.go` + test | the one thing the GUI owns: `~/Library/Application Support/sub-translator/gui.json` | 2 |
| `gui/events.go` + test | the event types and the stream decoder | 3 |
| `gui/runner.go` + test | `ProbeFile`, `Run`, `Cancel`; spawning and terminal-event discipline | 4, 5 |
| `gui/settings.go` + test | reading and writing through `sub-translator config` | 6 |
| `gui/frontend/index.html` | the shell and the four screens' markup | 7, 8 |
| `gui/frontend/app.css` | the scrim, the type scale, the accent, dark mode | 7 |
| `gui/frontend/app.js` | screen state, event handling, error routing | 7, 8 |
| `Makefile` | `make gui`, `make dmg` | 9 |
| the spec | the I-1 correction and the packaging note | 8, 9 |

Order matters: Tasks 2–6 are Go with no window, each testable on its own, and they are where the risk is. The UI comes after the thing it drives is proven.

---

## Task 1: the module that confines Wails

**Why first:** everything else lives in it, and the constraint it must not break — the root module's zero dependencies — is the one this plan can most easily violate by accident.

**Files:**
- Create: `gui/go.mod`, `gui/main.go`, `gui/frontend/index.html`, `gui/wails.json`
- Test: `gui/module_test.go`
- Modify: `Makefile`

**Interfaces:**
- Produces: a buildable `gui` module and `make gui`.

- [ ] **Step 1: Write the failing test**

`gui/module_test.go`. It guards the constraint, not the window:

```go
package main

import (
	"os"
	"strings"
	"testing"
)

// TestTheRootModuleStaysDependencyFree is the constraint this whole module
// exists to protect: Wails needs cgo and a dozen dependencies, and the CLI
// cross-compiles to five platforms because it has none. Keeping them in
// separate modules is what lets both be true.
func TestTheRootModuleStaysDependencyFree(t *testing.T) {
	body, err := os.ReadFile("../go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "require") {
		t.Errorf("the root go.mod has a require block:\n%s\n"+
			"Wails belongs in gui/go.mod. The root module cross-compiles to five "+
			"platforms and cannot take a cgo dependency.", body)
	}
}

// TestTheGUIDoesNotImportTheCLI pins the other half of the separation: the
// window drives the binary through its event stream, so importing its packages
// would couple them and drag the GUI's build constraints back into the CLI.
func TestTheGUIDoesNotImportTheCLI(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	scanned := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		body, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		if strings.Contains(string(body), "artschekoff/sub-translator/internal") {
			t.Errorf("%s imports the CLI's internal packages", e.Name())
		}
	}
	// A scan that found no files would pass silently.
	if scanned == 0 {
		t.Fatal("scanned no .go files in gui/")
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `cd gui && go test ./...`
Expected: FAIL — there is no module yet, so this is a setup failure. Create the module in Step 3, then expect these two to pass and stay passing.

- [ ] **Step 3: Create the module and the minimal app**

```bash
mkdir -p gui/frontend
cd gui
go mod init github.com/artschekoff/sub-translator/gui
go get github.com/wailsapp/wails/v2@v2.12.0
```

**Pin the version explicitly and record it.** The installed `wails` CLI is **v2.12.0** while the spec's prose says v2.14.0; the library version in `gui/go.mod` is what matters for the build, and it must match or trail the CLI. If you choose a different version, say so in your report and check that `runtime.OnFileDrop` exists in it — Task 7 depends on that function and it is the one API this plan cannot work around.

`gui/main.go` for now is the smallest thing that opens a window; Task 7 replaces its options block.

- [ ] **Step 4: Run the tests and build**

Run: `cd gui && go test ./... && go build ./...`
Run: `cd .. && make build-all` — still five platforms, still no root dependency.

- [ ] **Step 5: Add `make gui`**

In the root `Makefile`, beside the existing targets:

```make
gui:
	cd gui && wails build
```

- [ ] **Step 6: Mutation-verify the guard**

Add `require github.com/wailsapp/wails/v2 v2.12.0` to the **root** `go.mod`, confirm `TestTheRootModuleStaysDependencyFree` goes red, then remove it and confirm `make build-all` still works. Report what you saw.

- [ ] **Step 7: Commit**

```bash
git add gui Makefile
git commit -m "feat(gui): a Wails module that keeps the CLI dependency-free"
```

---

## Task 2: finding the CLI, and the one piece of state the GUI owns

**Why:** every other task needs a path to the binary, and the lookup has a bootstrap trap the spec calls out: the GUI reads the shared config **by running the CLI**, so the CLI's own location cannot live there — needing the CLI to find the CLI is circular.

**Files:**
- Create: `gui/locate.go`, `gui/locate_test.go`, `gui/guistate.go`, `gui/guistate_test.go`

**Interfaces:**
- Produces:
  - `func LocateCLI() (string, error)` — a bundled binary beside the executable, then `$PATH`, then the remembered path
  - `func RememberCLI(path string) error` and `func RememberedCLI() string`
  - `var ErrCLINotFound = errors.New("sub-translator not found")`

- [ ] **Step 1: Write the failing tests**

```go
// TestLocateCLIPrefersTheBundledBinary: the DMG ships the CLI inside the .app,
// so a copy beside the GUI executable wins over whatever is on PATH — a user
// with an old sub-translator in /usr/local/bin must still get the one we shipped.
func TestLocateCLIPrefersTheBundledBinary(t *testing.T) { /* ... */ }

// TestLocateCLIFallsBackToPath covers the developer case: no bundle, one on PATH.
func TestLocateCLIFallsBackToPath(t *testing.T) { /* ... */ }

// TestLocateCLIUsesTheRememberedPathLast is the file picker's result.
func TestLocateCLIUsesTheRememberedPathLast(t *testing.T) { /* ... */ }

// TestLocateCLIReportsNotFoundRatherThanGuessing: with none of the three, the
// window opens on Settings with a picker. A wrong guess here produces an
// error the user cannot act on.
func TestLocateCLIReportsNotFoundRatherThanGuessing(t *testing.T) {
	// assert errors.Is(err, ErrCLINotFound)
}

// TestTheRememberedPathIsNotInTheSharedConfig is the bootstrap guard. The GUI
// reads the shared config by RUNNING the CLI, so the CLI's location cannot be
// stored there. This test asserts the GUI's own file is used and that nothing
// in locate.go or guistate.go shells out to `config`.
func TestTheRememberedPathIsNotInTheSharedConfig(t *testing.T) { /* ... */ }
```

Write these out fully. Each needs a temp directory, a fake executable made with `os.WriteFile` + `os.Chmod(0o755)`, and `t.Setenv` for `PATH` and `HOME`. The last one is a source scan over `locate.go` and `guistate.go` for the string `"config"` — it must find none, and must fail if it scanned no files.

- [ ] **Step 2: Run them and watch them fail** — `cd gui && go test -run Locate ./...`, expected: compile failure, `LocateCLI` undefined.

- [ ] **Step 3: Implement**

```go
// LocateCLI finds the sub-translator binary, in the order the spec fixes:
// a copy beside this executable (how the DMG ships it), then $PATH, then a path
// the user picked earlier.
//
// The remembered path deliberately does NOT live in the shared config file: the
// GUI reads that config by running the CLI, so storing the CLI's location there
// would mean needing the CLI to find the CLI. It is the one piece of state this
// window owns.
func LocateCLI() (string, error) {
	if self, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(self), "sub-translator")
		if isExecutableFile(candidate) {
			return candidate, nil
		}
	}
	if p, err := exec.LookPath("sub-translator"); err == nil {
		return p, nil
	}
	if p := RememberedCLI(); p != "" && isExecutableFile(p) {
		return p, nil
	}
	return "", ErrCLINotFound
}
```

`guistate.go` reads and writes `~/Library/Application Support/sub-translator/gui.json` with one field, `{"cli":"…"}`, creating the directory at `0o755` and the file at `0o644`. Use `os.CreateTemp` plus `os.Rename` rather than writing in place, matching how the CLI writes its own files — a half-written state file on a crash would send the next launch to the picker for no reason.

- [ ] **Step 4: Run the tests** → PASS.

- [ ] **Step 5: Mutation-verify** — reorder `LocateCLI` so `$PATH` precedes the bundle, confirm `TestLocateCLIPrefersTheBundledBinary` goes red; make `RememberedCLI` read through `config get`, confirm the bootstrap guard goes red. Restore both and report.

- [ ] **Step 6: Commit**

```bash
git add gui/locate.go gui/locate_test.go gui/guistate.go gui/guistate_test.go
git commit -m "feat(gui): find the CLI without needing the CLI to find it"
```

---

## Task 3: the event decoder

**Why:** this is the contract the previous two branches built, and every screen reads it. It is a pure function over an `io.Reader`, so it can be tested exhaustively without spawning anything.

**Files:**
- Create: `gui/events.go`, `gui/events_test.go`

**Interfaces:**
- Produces:
  - one struct per event type, with the JSON tags **copied from the CLI's `internal/events/events.go`** — `Stream{Index int, Lang, Title, Codec string}`, `Probe{Duration float64, Audio, Subtitles []Stream, Fast *FastRule, EmbedsSubtitles *bool}`, `FastRule{Sources, Modes []string, MinChunkMinutes int}`, `Stage{Name string, Chunk, Of int, From, To string}`, `Progress{Pct int}`, `Language{Detected string}`, `Saved{Path, Covers string, Watchable bool}`, `Warning{Kind string, Count, Total int, First []int}`, `Done{Path, Covers string}`, `Error{Kind, Message string}`
  - `func Decode(r io.Reader, on func(Event) error) error` — one callback per event, skipping prose; a callback error stops the decode and is returned
  - `type Event struct { Type string; T string; Raw json.RawMessage }`, with one typed accessor per event kind:
    ```go
    func (e Event) AsProbe() (*Probe, error)
    func (e Event) AsStage() (*Stage, error)
    func (e Event) AsProgress() (*Progress, error)
    func (e Event) AsLanguage() (*Language, error)
    func (e Event) AsSaved() (*Saved, error)
    func (e Event) AsWarning() (*Warning, error)
    func (e Event) AsDone() (*Done, error)
    func (e Event) AsError() (*Error, error)
    ```

**This shape is fixed, not a choice.** Four later tasks consume `Event`, and a tagged union or a different callback signature would leave each of them guessing at an interface they cannot see. `Raw` plus accessors is chosen for one reason worth knowing: an event type this GUI has never heard of still arrives with its `Type` and `T` intact and can be logged or ignored, where a union would have to drop it or fail. The CLI's protocol carries a `version` event precisely so a consumer can outlive a change, and a decoder that cannot represent an unknown event throws that away.

- [ ] **Step 1: Write the failing tests**

The decoder's whole job is in its edge cases, so the table is the test:

```go
func TestDecodeSkipsProseAndReadsEvents(t *testing.T) {
	in := strings.Join([]string{
		`{"type":"version","t":"2026-09-29T01:00:00Z","events":1,"tool":"1.4.0"}`,
		`Extracting audio track #1...`,                       // prose: skipped
		`warning: 12 of 1834 blocks could not be translated`, // prose: skipped
		`{"type":"progress","t":"…","pct":42}`,
		``,                                                    // blank: skipped
		`{"type":"done","t":"…","path":"/tmp/film.es.srt"}`,
	}, "\n")
	// assert exactly three events, in order, with the right types
}

// TestDecodeFailsOnAMalformedJSONLine is the rule the CLI's own guarantees earn
// us: it promises every line is whole and that an event never starts mid-line,
// so a '{'-leading line that does not parse is a bug to surface, not skip.
func TestDecodeFailsOnAMalformedJSONLine(t *testing.T) {
	// input: `{"type":"done"` with no closing brace → Decode returns an error
	// naming the line
}

func TestDecodeHandlesALineLongerThanTheDefaultScannerBuffer(t *testing.T) {
	// a warning event with 50,000 `first` indices: bufio.Scanner's 64 KB default
	// would fail; the decoder must raise its buffer
}

func TestDecodeReadsTheFastRuleAndTheEmbedFlag(t *testing.T) {
	// probe with fast{sources,modes,min_chunk_minutes} and embeds_subtitles:false
	// — assert EmbedsSubtitles is non-nil and false, NOT nil
}

func TestDecodeDistinguishesAnAbsentEmbedFlagFromFalse(t *testing.T) {
	// probe with no embeds_subtitles → EmbedsSubtitles is nil.
	// This is why the field is a *bool: false means "this container cannot take
	// a track", nil means "nobody said", and a plain bool conflates them.
}
```

- [ ] **Step 2: Run them and watch them fail** — compile failure, `Decode` undefined.

- [ ] **Step 3: Implement**

Use `bufio.Scanner` with `sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)`. For each line: trim trailing `\r`; skip if blank or if the first non-space byte is not `{`; otherwise unmarshal and return an error naming the line if it fails.

- [ ] **Step 4: Run the tests** → PASS.

- [ ] **Step 5: Mutation-verify** — make the malformed-line case skip instead of erroring, confirm its test goes red; drop the `Buffer` call, confirm the long-line test goes red; change `EmbedsSubtitles` to a plain `bool`, confirm the absent-versus-false test goes red. Restore all three and report each message.

- [ ] **Step 6: Commit**

```bash
git add gui/events.go gui/events_test.go
git commit -m "feat(gui): decode the CLI's event stream, prose and all"
```

---

## Task 4: probing a file

**Files:**
- Create: `gui/runner.go`, `gui/runner_test.go`

**Interfaces:**
- Consumes: `LocateCLI`, `Decode` and the event types.
- Produces: `func ProbeFile(ctx context.Context, cli, path string) (*Probe, error)` — **not** `Probe`, which is already the struct type Task 3 defines; Go forbids a function and a type sharing an identifier in one package

- [ ] **Step 1: Write the failing tests**

Test against a **stub `sub-translator`** — a shell script that prints a canned stream and exits — not the real CLI, so the tests stay fast and deterministic:

```go
// TestProbeReturnsTracksAndTheRulesTheWindowNeeds
func TestProbeReturnsTracksAndTheRulesTheWindowNeeds(t *testing.T) {
	// stub prints version + probe with two audio tracks, one subtitle track,
	// fast{sources:["audio"],modes:["srt"],min_chunk_minutes:1},
	// embeds_subtitles:true, duration:8348.674 → assert every field arrives
}

// TestProbeSurfacesProbeFailed: the real CLI answers an unreadable file with
// version, then error{probe_failed}, then exit 1. The window must show that
// message, not "no tracks".
func TestProbeSurfacesProbeFailed(t *testing.T) {
	// assert the returned error carries kind "probe_failed" and the message
}

// TestProbeSurvivesACancelledProbe: a cancel during -probe now yields
// error{cancelled} and exit 130 — documented behaviour, and the window must
// not read it as a probe failure.
func TestProbeSurvivesACancelledProbe(t *testing.T) { /* ... */ }

// TestProbeReportsASignalledExitWithAnEmptyStream. There is a window of a few
// milliseconds at startup, before the CLI has armed its handler, in which a
// cancel kills it with nothing on the stream. The CLI's own documentation says
// to read that as "cancelled before the tool could answer", and the window must
// not present it as a crash.
func TestProbeReportsASignalledExitWithAnEmptyStream(t *testing.T) { /* ... */ }
```

- [ ] **Step 2: Run them and watch them fail.**

- [ ] **Step 3: Implement**

`Probe` runs `cli -probe -events json <path>`, decodes stderr, keeps the `probe` event, and maps endings: a `done`/`probe` with exit 0 is success; an `error` event is returned with its kind; a signalled exit with no events is the startup window. Read stdout to `io.Discard` — it carries the human track listing and must not fill a pipe.

- [ ] **Step 4: Run the tests** → PASS.

- [ ] **Step 5: Mutation-verify** — make `Probe` ignore the `error` event and report "no tracks", confirm the `probe_failed` test goes red; drop the stdout drain and feed the stub 200 KB of prose on stdout, confirm it hangs or fails. Report both.

- [ ] **Step 6: Commit**

```bash
git add gui/runner.go gui/runner_test.go
git commit -m "feat(gui): probe a file and surface what went wrong"
```

---

## Task 5: running a translation, and cancelling it

**Files:**
- Modify: `gui/runner.go`, `gui/runner_test.go`

**Interfaces:**
- Produces:
  - `type RunOptions struct { Input, To, From, Mode, Out string; Track int; Source string; Fast bool }`
  - `func Start(ctx context.Context, cli string, o RunOptions, on func(Event) error) (*Run, error)` — returns as soon as the child is started, streaming events to `on` as they arrive
  - `func (r *Run) Wait() (*Outcome, error)` — blocks until the run ends

**Not `func Run(...)`.** A package-level `Run` function and a `Run` type cannot coexist in one package, and — setting the compile error aside — a blocking call can never hand back a live handle for `Cancel` to act on mid-flight. My first draft of this block asked for both, which was impossible in two independent ways.
  - `type Outcome struct { Done *Done; Err *Error; ExitCode int; Saved []Saved; Warning *Warning }`
  - `func (r *Run) Cancel() error` — sends SIGINT and waits

- [ ] **Step 1: Write the failing tests**

```go
// TestRunReportsExactlyOneEnding: the CLI guarantees one terminal event and an
// exit code that agrees. The window's completion screen is built on that, so
// the runner must refuse a stream that breaks it rather than pick one.
func TestRunRefusesTwoTerminalEvents(t *testing.T) {
	// stub prints done then error{cancelled} → Run returns an error saying the
	// stream broke its contract, naming both
}

func TestRunPairsTheExitCodeWithTheEnding(t *testing.T) {
	// table: done/0 ok; error/1 ok; error{cancelled}/130 ok;
	// done/1 → error; error/0 → error
}

// TestRunCollectsEverySavedFile: -mode both emits one saved per file, so
// "Show in Finder" can reach the sidecar as well as the container.
func TestRunCollectsEverySavedFile(t *testing.T) { /* ... */ }

// TestCancelSendsSIGINTAndWaitsFor130 — with a stub that traps INT, prints
// error{cancelled} and exits 130.
func TestCancelSendsSIGINTAndWaitsFor130(t *testing.T) { /* ... */ }

// TestCancelDoesNotOrphanTheChild: the CLI kills its own process group, so the
// runner must wait for the process rather than returning while it dies. A GUI
// that quits here leaves an ffmpeg running.
func TestCancelDoesNotOrphanTheChild(t *testing.T) { /* ... */ }
```

- [ ] **Step 2: Run them and watch them fail.**

- [ ] **Step 3: Implement**

Build argv from `RunOptions`, passing `-events json` always. Stream events to `on` as they arrive. On `Cancel`, `cmd.Process.Signal(os.Interrupt)` and then `cmd.Wait`; do **not** kill the group yourself — the CLI does that, and doing it here would race its own handler.

- [ ] **Step 4: Run the tests** → PASS. Then run them 20 times: `go test -race -count=20 -run 'Cancel' ./...` — cancellation tests that pass once prove little.

- [ ] **Step 5: Mutation-verify** — accept a second terminal event, confirm red; return from `Cancel` without `Wait`, confirm the orphan test goes red. Report both.

- [ ] **Step 6: Commit**

```bash
git add gui/runner.go gui/runner_test.go
git commit -m "feat(gui): run a translation and cancel it without orphans"
```

---

## Task 6: settings, through the CLI and only through the CLI

**Why:** the spec requires the window hold no second copy of the validation rules, so every setting is read and written with `sub-translator config`. There is a trap: `config list` prints **aligned columns** (`  translate.model      (unset)`), and parsing that would make a prose format into an API. Read each key with `config get` instead.

**Files:**
- Create: `gui/settings.go`, `gui/settings_test.go`

**Interfaces:**
- Produces:
  - `var SettingKeys = []string{"translate.api-key", "translate.model", "translate.workers", "whisper.bin", "whisper.model", "whisper.vad-model", "whisper.chunk-minutes", "models.dir"}`
  - `func ReadSettings(ctx context.Context, cli string) (map[string]string, error)`
  - `func WriteSetting(ctx context.Context, cli, key, value string) error`

The keys above are the ones the spec's Settings screen names. The CLI also has `whisper.language`, `whisper.threads` and `whisper.vad-threshold`; leave them out of the screen and say so in your report rather than adding controls the spec did not ask for.

- [ ] **Step 1: Write the failing tests**

```go
// TestReadSettingsUsesGetNotList is the point of the task: `config list`
// prints aligned columns for a human, and parsing them would turn a prose
// layout into this window's API. Asserted by the calls the stub records.
func TestReadSettingsUsesGetNotList(t *testing.T) {
	// stub logs its argv; assert one `config get <key>` per key and no `config list`
}

func TestReadSettingsTreatsUnsetAsEmpty(t *testing.T) { /* `(unset)` → "" */ }

// TestWriteSettingSurfacesTheCLIsRejection: the CLI validates values — a
// negative worker count, a whisper binary that is not executable. The window
// shows what it said instead of validating in parallel.
func TestWriteSettingSurfacesTheCLIsRejection(t *testing.T) { /* ... */ }

// TestTheAPIKeyIsNeverLogged guards the one value here that must not leak:
// assert WriteSetting's error text for translate.api-key does not contain the
// value it was given.
func TestTheAPIKeyIsNeverLogged(t *testing.T) { /* ... */ }
```

- [ ] **Step 2: Run them and watch them fail.**

- [ ] **Step 3: Implement.** One `exec.CommandContext` per key. Trim trailing newline. Map exactly `(unset)` to `""`.

- [ ] **Step 4: Run the tests** → PASS.

- [ ] **Step 5: Mutation-verify** — reimplement `ReadSettings` over `config list` output, confirm `TestReadSettingsUsesGetNotList` goes red; include the value in `WriteSetting`'s error, confirm the key test goes red. Report both.

- [ ] **Step 6: Commit**

```bash
git add gui/settings.go gui/settings_test.go
git commit -m "feat(gui): read and write settings through the CLI"
```

---

## Task 7: the window, and the glass

**Files:**
- Modify: `gui/main.go`
- Create: `gui/rules.go`, `gui/rules_test.go`, `gui/frontend/index.html`, `gui/frontend/app.css`, `gui/frontend/app.js`
- Modify: `docs/superpowers/specs/2026-09-26-desktop-gui-design.md` (the I-1 correction in Step 4)

**Interfaces:**
- Consumes: everything from Tasks 2–6, bound as Wails methods.
- Produces: the Drop and File screens, and the shell the other two fill.

- [ ] **Step 1: the window options**

The glass is the operating system's, not a CSS imitation:

**The translucency flags are per-platform, not on `options.App`.** I checked this against the module rather than the spec's prose, because the spec reads as though they were top-level and they are not: in v2.12.0 `WindowIsTranslucent` and `WebviewIsTransparent` are fields of `mac.Options` (`pkg/options/mac/mac.go:22-23`), `windows.Options` (`windows.go:86-87`) and `linux.Options` (`linux.go:22`). Setting them at the top level does not compile.

`options.App` also has `BackgroundColour *RGBA`, which Wails **defaults to opaque** if left nil (`options.go:171-172`). A transparent webview over an opaque window background shows no blur, so it has to be set explicitly — this is the piece the spec's description omits and the one most likely to produce a window that looks like the imitation rather than the real thing.

```go
	err := wails.Run(&options.App{
		Title:  "sub-translator",
		Width:  520,
		Height: 560,
		// Wails fills this with an opaque colour when it is nil, and an opaque
		// window background defeats a transparent webview: the blur never shows.
		BackgroundColour: &options.RGBA{R: 0, G: 0, B: 0, A: 0},
		Mac: &mac.Options{
			// The compositor's own blur. This pair is the documented way to a
			// frosty window; a CSS backdrop-filter over an opaque page is the
			// imitation of it.
			WindowIsTranslucent:  true,
			WebviewIsTransparent: true,
			TitleBar:             &mac.TitleBar{TitlebarAppearsTransparent: true, FullSizeContent: true},
			Appearance:           mac.DefaultAppearance,
		},
		Windows: &windows.Options{
			WindowIsTranslucent:  true,
			WebviewIsTransparent: true,
			BackdropType:         windows.Mica,
		},
		OnStartup:     app.startup,
		OnBeforeClose: app.beforeClose, // func(ctx) (prevent bool) — Task 8 Step 5
		Bind:          []interface{}{app},
	})
```

**The window must not change size between screens** — do not set a per-screen height.

`runtime.OnFileDrop`'s callback is `func(x, y int, paths []string)`, not `func(paths []string)` — the coordinates come first and Task 7 Step 3 ignores them.

- [ ] **Step 2: the scrim, which is what makes glass legible**

In `app.css`. A user's wallpaper can be a white beach or a black poster, so text never floats on the blur:

```css
:root {
  --scrim: rgba(255, 255, 255, 0.72);
  --fg: #1c1c1e;
  --hairline: rgba(28, 28, 30, 0.08);
  --accent: -apple-system-blue;
  --radius-card: 10px;
  --radius-control: 6px;
  --size-body: 13px;
  --size-hint: 11px;
  --size-title: 20px;
}
@media (prefers-color-scheme: dark) {
  :root { --scrim: rgba(28, 28, 30, 0.72); --fg: #f2f2f7; --hairline: rgba(242, 242, 247, 0.08); }
}
html, body { background: transparent; }          /* the blur comes from the OS */
body { font: var(--size-body) -apple-system, "Segoe UI Variable", sans-serif; color: var(--fg); }
.card { background: var(--scrim); border-radius: var(--radius-card); }
.tnum { font-variant-numeric: tabular-nums; }    /* ticking numbers must not jitter */
```

Three sizes only, one accent on exactly three things (the selected radio, the progress fill, the primary button), hairlines at 8%, no shadows or gradients.

- [ ] **Step 3: the Drop screen**

`runtime.OnFileDrop` gives absolute paths. More than one file takes the first **and says so** — silently ignoring the rest is how a user ends up translating the wrong film. A "Choose…" button opens the native dialog.

- [ ] **Step 4: the File screen, including the control the spec is missing**

Filename and duration; audio and subtitle tracks as **one** radio list, because picking audio means transcription and picking a subtitle track means translating what is there; a target-language picker; output mode; a save path; a fast-mode checkbox.

Three rules the window must read rather than reimplement:
- fast mode is enabled only when the chosen source and output are in `probe.fast.sources` / `.modes`, and disabled with the reason spelled out rather than shown dead
- the embed and both output modes are offered only when `probe.embeds_subtitles` is true, with the AVI case explained
- when the file has no subtitle tracks the section **says so** rather than showing an empty box — that is the case the transcription feature exists for

**And the spec's gap, I-1:** `-source sub` requires `-from`, and `probe.subtitles[].lang` is empty for untagged tracks, which the CLI's own comment calls common. With no control for it the window would send a run that fails and offer a Retry that can never succeed. So: when a chosen subtitle track has no `lang`, show a source-language picker and require it before Start. Amend the spec's File screen paragraph to include the control and say why.

- [ ] **Step 5: Test what can be tested without a browser**

Wails has no headless harness here, so test the **decisions**, not the DOM. Put fast-mode availability and output-mode availability in **Go** (`gui/rules.go`), table-test them, and have the frontend call them over the Wails binding. A rule written in JavaScript cannot be tested without the toolchain this plan forbids; the same rule in Go can, and these two are the ones that get a control wrong in a way a user sees.

```go
// FastAvailable answers the question the checkbox asks, from the rule the CLI
// stated rather than a copy of it.
func FastAvailable(p *Probe, source, mode string) (bool, string)
```

Table-test it over every source/mode pair against a `Fast` rule, including a nil `Fast` (an older CLI: fast mode is then simply unavailable, not assumed).

- [ ] **Step 6: Verify by hand and record it**

`make gui`, then: drop a real film, confirm the tracks match `sub-translator -probe` on the same file, confirm the window does not resize between Drop and File, and confirm the blur is visible in the margins and behind the title bar but not behind paragraphs. Report what you saw, with the film's track count.

- [ ] **Step 7: Commit**

```bash
git add gui/main.go gui/rules.go gui/rules_test.go gui/frontend docs/superpowers/specs/2026-09-26-desktop-gui-design.md
git commit -m "feat(gui): the drop and file screens on real OS glass"
```

---

## Task 8: the Run screen, the Settings screen, and where errors lead

**Files:**
- Modify: `gui/frontend/index.html`, `gui/frontend/app.css`, `gui/frontend/app.js`, `gui/main.go`
- Create: `gui/routing.go`, `gui/routing_test.go`

**Interfaces:**
- Produces: `func RoutesToSettings(kind string) (field string, ok bool)`

- [ ] **Step 1: Write the failing test for the routing**

This is the spec's stated requirement — a missing key is not a failure to report, it is a setup step to complete — and it is a decision, so it belongs in Go:

```go
func TestRoutesToSettings(t *testing.T) {
	for _, tc := range []struct{ kind, field string; want bool }{
		{"missing_key", "translate.api-key", true},
		{"invalid_key", "translate.api-key", true},
		{"whisper_not_found", "whisper.bin", true},
		{"no_model", "whisper.model", true},
		{"ambiguous_model", "whisper.model", true},
		{"rate_limited", "", false},          // waited out on the Run screen
		{"output_unwritable", "", false},     // the user picks another path there
		{"no_audio_track", "", false},
		{"transcript_empty", "", false},
		{"cancelled", "", false},
		{"other", "", false},
	} {
		// assert field and ok
	}
}

// TestEveryErrorKindIsRouted: the CLI has thirteen kinds and a stream can carry
// any of them. A kind this table has never seen must still land somewhere —
// the Run screen with its message — rather than leaving the window blank.
func TestEveryErrorKindIsRouted(t *testing.T) {
	// drive RoutesToSettings with an invented kind, assert ok is false and the
	// caller has a message to show
}
```

- [ ] **Step 2: Run them and watch them fail.**

- [ ] **Step 3: Implement the routing and both screens**

The **Run screen**: the stage in words, a progress bar, and for `-fast` the growing coverage with a prominent line the moment the file is watchable — `saved` with `watchable:true`, which only the fast path sends. A Cancel button. On completion: the path, a "Show in Finder" button **per saved file** (so `-mode both` reaches the sidecar), and any `warning` about untranslated blocks, because a file twelve blocks short must not look identical to one that is not.

The **Settings screen**: the eight keys from Task 6, the API key masked with a Show toggle, and — above a divider, labelled as the one setting this window keeps for itself — the path to the CLI.

An `error` event whose kind routes to Settings switches screen, focuses the field, and shows the message above it. Every other kind stays on Run with the message and a Retry button.

- [ ] **Step 4: Run the tests** → PASS.

- [ ] **Step 5: the closing-while-running guard**

Wails' `OnBeforeClose` asks first — "transcription is at 34%, leaving now loses it" — and on confirmation cancels the child and waits before quitting. One window, one lifetime, no background orphans.

- [ ] **Step 6: Mutation-verify** — route `rate_limited` to Settings, confirm the table goes red; make `RoutesToSettings` panic on an unknown kind, confirm the second test goes red. Report both.

- [ ] **Step 7: Verify by hand**

With no API key set, start a run and confirm the window lands on Settings with the key field focused. Then set a bad key and confirm `invalid_key` does the same rather than offering Retry. Then cancel a real run mid-transcription and confirm the window reports what the partial file covers, and that `ps` shows no surviving `ffmpeg` or `whisper-cli`. Report all three.

- [ ] **Step 8: Commit**

```bash
git add gui/routing.go gui/routing_test.go gui/frontend gui/main.go
git commit -m "feat(gui): the run and settings screens, and errors that lead somewhere"
```

---

## Task 9: `make dmg`, unsigned

**Files:**
- Modify: `Makefile`, `README.md`, the spec's Packaging section

- [ ] **Step 1: the target**

```make
# Unsigned on purpose: no Developer ID, no notarisation. A first run needs
# right-click → Open, or `xattr -d com.apple.quarantine`, which the README says.
dmg: build gui
	rm -rf build/dmg && mkdir -p build/dmg
	cp -R gui/build/bin/$(BINARY).app build/dmg/
	cp $(BIN_DIR)/$(BINARY) build/dmg/$(BINARY).app/Contents/MacOS/
	hdiutil create -volname $(BINARY) -srcfolder build/dmg \
		-ov -format UDZO build/$(BINARY).dmg
```

Use the existing `$(BINARY)` and `$(BIN_DIR)` variables (`Makefile:2-3`) rather than hardcoding `sub-translator` and `bin` — the `build` target already does, and a second spelling of the same name is a rename waiting to break one of them.

The `cp` of the CLI into `Contents/MacOS/` is what makes `LocateCLI`'s first branch find it. **Verify both paths against what `wails build` actually produces** rather than trusting these lines: I could not confirm the `.app` output location from the module's source, and the bundle layout is the one thing here that a wrong guess makes silently wrong — the DMG would build, install, and then fail to find its own CLI at runtime.

- [ ] **Step 2: Verify the bundle finds its own CLI**

Build the DMG, mount it, copy the app elsewhere, remove `sub-translator` from `$PATH`, and launch. It must work — that is the branch Task 2 wrote first and the one the DMG depends on. Report the steps you ran.

- [ ] **Step 3: Document it**

README: `make gui`, `make dmg`, and what an unsigned app asks of a first-time user. Spec's Packaging section: whatever differs from what it currently says.

- [ ] **Step 4: Commit**

```bash
git add Makefile README.md docs/superpowers/specs/2026-09-26-desktop-gui-design.md
git commit -m "feat: build an unsigned DMG"
```

---

## Self-Review

**Spec coverage.** Part 2's Structure → Task 1; Finding the CLI → Task 2; the Drop and File screens → Task 7; Run and Settings → Task 8; Visual design → Task 7 Steps 1–2; "Errors lead somewhere" → Task 8; Cancelling → Tasks 5 and 8; Packaging → Task 9. The event stream Part 1 defines is consumed in Task 3 and driven in Tasks 4–5. The spec's S6 (no copy of the rules) is enforced by Task 6's `config get` requirement and Task 7's `FastAvailable`; I-1's missing source-language control is fixed in Task 7 Step 4 together with the spec paragraph that omits it.

**Placeholders.** The test bodies in Tasks 2, 4, 5 and 6 are specified by name, purpose and assertion rather than written out, because each depends on a stub harness whose shape the implementer must choose; every one says what it must assert and why. Task 7's HTML and JS are described structurally with the CSS that carries the design decisions given verbatim — the decisions that can be got wrong are in the CSS custom properties and in `FastAvailable`, and both are concrete. No step says "add error handling" or "write tests for the above".

**Type consistency.** `Probe`, `FastRule`, `Stream`, `Saved`, `Done`, `Error` are defined in Task 3 with tags copied from the CLI and consumed in Tasks 4, 5, 7 and 8. `LocateCLI`/`RememberedCLI` (Task 2) are used by Tasks 4, 5, 6 and 8. `SettingKeys` (Task 6) is consumed by Task 8's Settings screen. `FastAvailable` (Task 7) and `RoutesToSettings` (Task 8) are each defined where first used.

**Two risks worth naming.** The installed `wails` CLI is v2.12.0 while the spec's prose says v2.14.0; Task 1 pins the library and requires the implementer to confirm `runtime.OnFileDrop` exists in whatever it pins, because Task 7 cannot work around its absence. And the frontend has no test harness by design, which is why both decisions that can be wrong — fast-mode availability and error routing — are pure Go functions with table tests rather than JavaScript.

**What I checked against the module rather than the spec, and what it cost.** Every Wails symbol this plan names was verified in `v2.12.0`'s source: `runtime.OnFileDrop` at `pkg/runtime/draganddrop.go:9` (signature `func(x, y int, paths []string)`), `OnBeforeClose` at `pkg/options/options.go:64`, `mac.DefaultAppearance`, `windows.Mica`. One check changed the plan: the spec reads as though `WindowIsTranslucent` and `WebviewIsTransparent` were fields of `options.App`, and they are per-platform — `mac.Options`, `windows.Options`, `linux.Options` — so the code block I first wrote would not have compiled. The same check turned up `BackgroundColour`, which Wails fills with an opaque colour when left nil and which would have produced a window with no visible blur and no error message to explain it. The two preceding plans in this project shipped eight errors of exactly this shape — a symbol that does not exist, or exists with a different signature — and every one was caught by an implementer rather than by me. This is the first one caught before dispatch.
