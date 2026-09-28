# CLI Follow-Ups Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the CLI-side gaps the event-protocol branch deliberately deferred, so the GUI is built on a tool whose default path is tested, whose long silent phases report progress, and whose rules it can read instead of copy.

**Architecture:** Eight tasks in the root Go module, each independently mergeable. The first makes the ordinary run drivable end-to-end in a test (one environment variable), which is what makes the rest testable at all; the middle ones turn `main()`'s 34 exit sites into returned errors and lift the pipeline into a function; the last three add progress reporting, one new error kind, and a machine-readable statement of the fast-mode rule.

**Tech Stack:** Go 1.26.2, zero third-party dependencies, `whisper.cpp` and `ffmpeg`/`ffprobe` as subprocesses, `internal/events` for the JSON Lines stream.

**Spec:** `docs/superpowers/specs/2026-09-26-desktop-gui-design.md` — Part 1 (the protocol, as built) and its "Still open, deliberately" list, which is what this plan closes. Part 2 describes the GUI that consumes all of it and is the subject of the *next* plan.

## Global Constraints

- **Zero third-party dependencies.** The root `go.mod` has no `require` block. `make build-all` cross-compiles to five platforms and must keep doing so; nothing may need cgo.
- **stdout is a contract.** No task may change one byte of stdout on any existing path. `TestAFullRunLeavesStdoutUnchangedUnderEvents` must pass at every commit.
- **Exit codes are a contract:** 0 success, 1 fatal, 2 usage error, 130 cancelled. No task changes any of them.
- **Events use `omitempty`.** An absent field means unknown. Never emit a zero value that reads as a fact.
- **Every line written to stderr is whole.** `events.SyncWriter` tracks whether the last byte was a newline (a carriage return does not count) and the emitter breaks a mid-line stream under the same lock. Any new stderr writer goes through `proseErr`; `TestNothingWritesStderrOutsideTheSharedWriter` fails on anything that does not.
- **Every fatal path emits exactly one `error` event**, `fatalKindf` with its kind and `fatalf` with `other`. Not two, not zero.
- **Every new test must be mutation-verified:** break the code it covers, watch the assertion fail, restore it, and report what you saw. Four defects on this project passed a green suite, including one test that searched a whole file for a string and so could not fail.

---

## File Structure

| File | Responsibility | Tasks |
|---|---|---|
| `main.go` | flags, the ordinary pipeline, fatal helpers, classifiers | 1, 3, 4, 5, 6, 8 |
| `errkind.go` | the `errKind` constants and `fatalKindf` | 4 |
| `fatal.go` *(new)* | `fatalError`, `kindErrf`, and the single place a returned error becomes an exit | 3 |
| `ordinary.go` *(new)* | `runOrdinary` — the non-`-fast` pipeline lifted out of `main()` | 3 |
| `progressive.go` | the `-fast` pipeline; the fast-mode rule's validators | 4, 6, 7 |
| `internal/translate/translator.go` | `Options.BaseURL` — stops being only a test seam | 1 |
| `internal/media/media.go` | ffmpeg invocation, stderr capture, progress parsing | 5, 8 |
| `internal/media/progress.go` *(new)* | parsing `time=` out of ffmpeg's progress lines | 5 |
| `internal/events/events.go` | the `Probe` event gains the fast-mode rule | 6 |
| `internal/proc/group_unix.go`, `group_windows.go` *(new)* | build-tagged process-group creation and killing | 8 |
| `README.md`, the spec | the new kind, the new probe field, `OPENAI_BASE_URL` | 1, 4, 6, 8 |

Task order matters in two places only: Task 2 cannot be written before Task 1, and Task 4 is much smaller after Task 3. Everything else is independent.

---

## Task 1: `OPENAI_BASE_URL`

**Why first:** `Options.BaseURL` is currently reachable only from inside the package, so no test can drive the *compiled binary* through a successful translation. That is the single reason the ordinary run's event stream has no test, and the reason the branch before this one could not close its own Major finding. One environment variable removes it, and it is independently useful — Azure endpoints, corporate proxies and local OpenAI-compatible servers all need it.

**Files:**
- Modify: `main.go:802-808` (`resolveTranslateOptions`)
- Modify: `internal/translate/translator.go:67` (the field comment)
- Test: `main_test.go`
- Modify: `README.md`, `docs/superpowers/specs/2026-09-26-desktop-gui-design.md`

**Interfaces:**
- Consumes: nothing.
- Produces: `OPENAI_BASE_URL` honoured by the compiled binary. Task 2 and every later task that wants a real-binary run depends on this.

- [ ] **Step 1: Write the failing test**

In `main_test.go`. It drives the real binary against an `httptest` server and asserts the translation went there, which is only possible if the env var is honoured.

```go
// TestTheCompiledBinaryHonoursOPENAI_BASE_URL is the test that the ordinary
// run's whole event stream depends on: without it no test can drive a real
// process through a successful translation, because the only other way to
// reach a stub endpoint is a package variable a subprocess cannot see.
func TestTheCompiledBinaryHonoursOPENAI_BASE_URL(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"{\"1\":\"hola\"}"}}]}`)
	}))
	defer srv.Close()

	bin := buildCLI(t)
	dir := t.TempDir()
	stubs := progressiveStubs(t, dir, dialogueSRT)

	cmd := exec.Command(bin, "-to", "es", "-source", "sub", filepath.Join(dir, "film.mkv"))
	cmd.Env = append(os.Environ(),
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"OPENAI_API_KEY=sk-test",
		"OPENAI_BASE_URL="+srv.URL,
		"HOME="+dir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, out)
	}
	if got := atomic.LoadInt32(&hits); got == 0 {
		t.Fatalf("the stub endpoint was never called, so OPENAI_BASE_URL was ignored\n%s", out)
	}
}
```

Read `progressiveStubs` and `dialogueSRT` in `progressive_e2e_test.go` before writing this and adapt the call to their real signatures; they already build stub `ffmpeg`/`ffprobe`/`whisper-cli` binaries on a temp `PATH`. If `progressiveStubs` does not produce a subtitle track, add a sibling helper rather than changing it — other tests depend on its current output.

- [ ] **Step 2: Run it and watch it fail**

Run: `go test -run TestTheCompiledBinaryHonoursOPENAI_BASE_URL ./... -v`
Expected: FAIL, "the stub endpoint was never called" — the binary went to the real endpoint (or failed to reach it).

- [ ] **Step 3: Honour the variable**

In `main.go`, `resolveTranslateOptions`:

```go
func resolveTranslateOptions(c *config.Config, workersFlag int) translate.Options {
	return translate.Options{
		APIKey: firstNonEmpty(os.Getenv("OPENAI_API_KEY"), c.Translate.APIKey),
		// Environment only, deliberately not config: this redirects every request
		// the run makes, so it belongs to one invocation rather than to a file a
		// user forgets they wrote. A persisted wrong endpoint fails in a way that
		// looks like the network rather than like a setting.
		BaseURL: os.Getenv("OPENAI_BASE_URL"),
		Model:   firstNonEmpty(c.Translate.Model, translate.DefaultModel),
		Workers: resolveWorkers(workersFlag, c.Translate.Workers),
	}
}
```

Then correct the field comment at `internal/translate/translator.go:67`, which currently calls it a test seam:

```go
	BaseURL  string // OPENAI_BASE_URL, or empty for the real endpoint
```

- [ ] **Step 4: Run the test and the suite**

Run: `go test -run TestTheCompiledBinaryHonoursOPENAI_BASE_URL ./... -v` → PASS
Run: `make validate` and `go test -race ./...` → clean

- [ ] **Step 5: Mutation-verify**

Remove the `BaseURL:` line, run the test, confirm it fails with "the stub endpoint was never called", restore it. Report what you saw.

- [ ] **Step 6: Document it**

Add `OPENAI_BASE_URL` to the README beside `OPENAI_API_KEY`, saying it redirects every request and naming the three reasons someone wants it (an OpenAI-compatible local server, a proxy, Azure). Add one line to the spec's Part 1 noting the variable exists and that it is what makes a real-binary test of a whole run possible.

- [ ] **Step 7: Commit**

```bash
git add main.go internal/translate/translator.go main_test.go README.md docs/superpowers/specs/2026-09-26-desktop-gui-design.md
git commit -m "feat: honour OPENAI_BASE_URL"
```

---

## Task 2: the ordinary run's event stream, pinned by a real process

**Why:** Five emits on the default non-`-fast` path have no test — `main.go`'s `stage translate`, `progress`, `warning`, and both `done` sites. Deleting either `done` leaves the whole suite green today, and the GUI's default flow *is* the ordinary run. This is the gap the previous plan recorded as the first thing to close.

**Files:**
- Test: `main_test.go`
- Modify: nothing. If a test cannot be written without changing production code, stop and report it.

**Interfaces:**
- Consumes: `OPENAI_BASE_URL` from Task 1; `buildCLI`, `progressiveStubs`, `dialogueSRT` from the existing tests.
- Produces: nothing later tasks consume. Task 3 must keep these tests passing, which is the point — they are the safety net for the extraction.

- [ ] **Step 1: Write the failing tests**

```go
// TestTheOrdinaryRunEmitsItsWholeStream pins the default path's events. It is a
// real process, so it also pins that exit 0 and the done event agree — the pair
// no in-process test can assert.
func TestTheOrdinaryRunEmitsItsWholeStream(t *testing.T) {
	srv := stubTranslateEndpoint(t)          // see Step 3
	bin := buildCLI(t)
	dir := t.TempDir()
	stubs := progressiveStubs(t, dir, dialogueSRT)

	var stderr bytes.Buffer
	cmd := exec.Command(bin, "-events", "json", "-to", "es", "-source", "sub",
		filepath.Join(dir, "film.mkv"))
	cmd.Env = append(os.Environ(),
		"PATH="+stubs+string(os.PathListSeparator)+os.Getenv("PATH"),
		"OPENAI_API_KEY=sk-test", "OPENAI_BASE_URL="+srv.URL, "HOME="+dir)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("want exit 0, got %v\nstderr:\n%s", err, stderr.String())
	}

	events := parseEventStream(t, stderr.Bytes())   // see Step 3
	wantInOrder := []string{"version", "probe", "stage", "stage", "progress", "done"}
	if got := kindsOf(events); !containsSubsequence(got, wantInOrder) {
		t.Errorf("event kinds = %v, want %v as a subsequence", got, wantInOrder)
	}
	if n := countOf(events, "done"); n != 1 {
		t.Errorf("%d done events, want exactly one", n)
	}
	if n := countOf(events, "error"); n != 0 {
		t.Errorf("%d error events on a successful run, want none", n)
	}
	for _, e := range events {
		if e["type"] == "done" && e["path"] == "" {
			t.Error("the done event carries no path, so a consumer cannot open the result")
		}
	}
}

// TestTheOrdinaryRunReportsUntranslatedBlocks pins the warning event on the
// default path: a file twelve blocks short of translated must not look
// identical to one that is not.
func TestTheOrdinaryRunReportsUntranslatedBlocks(t *testing.T) {
	srv := stubTranslateEndpointRefusingEveryOtherBatch(t)   // see Step 3
	// ... same setup as above ...
	// assert: exit 0, exactly one warning event with kind "blocks_untranslated",
	// its count > 0, its total equal to the number of blocks in dialogueSRT,
	// and exactly one done event after it.
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test -run 'TestTheOrdinaryRun' ./... -v`
Expected: FAIL — the helpers in Step 3 do not exist yet, so this is a compile failure. That is the expected first failure; write the helpers, then expect assertion failures only if the emits are genuinely missing.

- [ ] **Step 3: Write the helpers**

```go
// stubTranslateEndpoint answers every batch with the same text it was given, so
// a run succeeds without a network. It counts calls so a caller can assert the
// request went here rather than to the real endpoint.
func stubTranslateEndpoint(t *testing.T) *httptest.Server { /* ... */ }

// parseEventStream returns one map per JSON line, skipping the prose lines the
// protocol's skip rule says to ignore. It fails the test on a line that starts
// with '{' and does not parse, because that is the corruption the stream's whole
// line-completeness machinery exists to prevent.
func parseEventStream(t *testing.T, stderr []byte) []map[string]any { /* ... */ }

func kindsOf(events []map[string]any) []string { /* ... */ }
func countOf(events []map[string]any, kind string) int { /* ... */ }
func containsSubsequence(got, want []string) bool { /* ... */ }
```

Write these out in full. `parseEventStream` is the important one: it must reject a `{`-leading line that does not parse rather than skipping it, or it will hide exactly the defect the previous branch spent a Critical and a Major fixing.

- [ ] **Step 4: Run them and the suite**

Run: `go test -run 'TestTheOrdinaryRun' ./... -v` → PASS
Run: `make validate`, `go test -race ./...` → clean

- [ ] **Step 5: Mutation-verify, once per emit**

Delete each of these in turn, confirm a test goes red, restore it, and report the failure message you saw:
1. the `done` emit at `main.go:473` (the sidecar path)
2. the `done` emit at `main.go:502` (the container path)
3. the `stage{translate}` emit at `main.go:413`
4. the `progress` emit in the translate callback
5. the `warning` emit at `main.go:435`

If any one of these does **not** turn a test red, the test does not pin it — fix the test, not the code, and say so in your report.

- [ ] **Step 6: Commit**

```bash
git add main_test.go
git commit -m "test: pin the ordinary run's event stream with a real process"
```

---

## Task 3: fatal errors become values, and the ordinary pipeline becomes a function

**Why:** `main()` runs from line 221 to line 671 and calls `fatalf` or `fatalKindf` 34 times. Each of those is an `os.Exit`, which is why the pipeline cannot be called from a test and why Task 2 had to drive a whole process to reach it. Turning the exits into returned errors and lifting the body into `runOrdinary` gives the default path the same testability the `-fast` path has had since it was written.

**This is the task that must not break anything.** Task 2's tests are the net: they assert the real binary's stream and exit codes from outside, so they cannot be satisfied by a wrong refactor.

**Files:**
- Create: `fatal.go`, `ordinary.go`
- Modify: `main.go` (the body of `main`, and the fatal helpers)
- Test: `fatal_test.go`, `ordinary_test.go`

**Interfaces:**
- Consumes: Task 2's tests as the regression net.
- Produces:
  - `type fatalError struct { kind errKind; msg string }` with `Error() string` and `Kind() errKind`
  - `func kindErrf(kind errKind, format string, args ...any) error`
  - `func errf(format string, args ...any) error` — the unclassified equivalent, kind `other`
  - `func exitWith(err error)` — emits exactly one `error` event and exits; the only place a returned error becomes an exit
  - `func runOrdinary(opts ordinaryOptions) error`

### Step group A — the error type

- [ ] **Step 1: Write the failing test** in `fatal_test.go`

```go
func TestAFatalErrorCarriesItsKind(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want errKind
	}{
		{"classified", kindErrf(errMissingKey, "no key"), errMissingKey},
		{"unclassified", errf("something broke"), errOther},
		{"wrapped classified", fmt.Errorf("stage: %w", kindErrf(errNoModel, "no model")), errNoModel},
		{"a plain error", errors.New("plain"), errOther},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := kindOf(tc.err); got != tc.want {
				t.Errorf("kindOf = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAFatalErrorKeepsItsMessageExactly(t *testing.T) {
	err := kindErrf(errRateLimited, "translate: %v", errors.New("429"))
	if got, want := err.Error(), "translate: 429"; got != want {
		t.Errorf("Error() = %q, want %q — the message is part of the contract", got, want)
	}
}
```

The `wrapped classified` case is the one that matters: `runOrdinary` will wrap errors as they travel up, and a kind that does not survive `%w` would be silently downgraded to `other`.

- [ ] **Step 2: Run it and watch it fail** — `go test -run TestAFatalError ./...`, expected: compile failure, `kindErrf` undefined.

- [ ] **Step 3: Write `fatal.go`**

```go
package main

import (
	"errors"
	"fmt"

	"github.com/artschekoff/sub-translator/internal/events"
)

// fatalError is a failure on its way up to main, carrying the kind a consumer
// acts on. It exists so the pipeline can return instead of calling os.Exit:
// every exit inside a 450-line function is a line a test cannot get past.
type fatalError struct {
	kind errKind
	msg  string
}

func (e *fatalError) Error() string { return e.msg }
func (e *fatalError) Kind() errKind { return e.kind }

// kindErrf builds a classified failure. It is the value-returning twin of
// fatalKindf, which exits.
func kindErrf(kind errKind, format string, args ...any) error {
	return &fatalError{kind: kind, msg: fmt.Sprintf(format, args...)}
}

// errf builds an unclassified failure — the twin of fatalf.
func errf(format string, args ...any) error {
	return &fatalError{kind: errOther, msg: fmt.Sprintf(format, args...)}
}

// kindOf reads the kind off an error, through any number of %w wrappings. An
// error that carries no kind is errOther: honest, because nothing has claimed
// to know what it is.
func kindOf(err error) errKind {
	var fe *fatalError
	if errors.As(err, &fe) {
		return fe.kind
	}
	return errOther
}

// exitWith is the one place a returned error becomes an exit, so "exactly one
// error event per fatal run" is a property of a single function rather than of
// 34 call sites.
func exitWith(err error) {
	ev.Emit(&events.Error{Kind: string(kindOf(err)), Message: err.Error()})
	fatalTail("%s", err.Error())
}
```

- [ ] **Step 4: Run it** → PASS. Then `make validate` and `go test -race ./...` → clean.

- [ ] **Step 5: Mutation-verify** — change `errors.As` to a type assertion (`fe, ok := err.(*fatalError)`), confirm the `wrapped classified` subtest goes red, restore.

- [ ] **Step 6: Commit**

```bash
git add fatal.go fatal_test.go
git commit -m "feat: let a fatal error travel as a value carrying its kind"
```

### Step group B — the extraction

- [ ] **Step 7: Define the options struct and the function signature** in `ordinary.go`

Do not invent the fields. Read `main.go:221-671` and list every value the pipeline reads from a flag or from config; those are the fields. Follow `runProgressive`'s existing signature style in `progressive.go` — it already takes what it needs rather than reaching for globals, and the two should read alike.

```go
// ordinaryOptions is everything the non-fast pipeline needs. It mirrors what
// runProgressive takes, so the two paths read alike and neither reaches for a
// flag variable behind the other's back.
type ordinaryOptions struct {
	// ... one field per value read from a flag or config in main.go:221-671 ...
}

// runOrdinary is the whole non-fast pipeline: probe, extract or read subtitles,
// transcribe, translate, write, and optionally mux. It returns instead of
// exiting so a test can drive it, and every error it returns carries a kind.
func runOrdinary(o ordinaryOptions) error {
```

- [ ] **Step 8: Move the body, converting each exit to a return**

Move `main.go`'s pipeline into `runOrdinary` **without changing any behaviour**: same order, same messages, same stdout, same emits. At each of the 34 sites:

- `fatalf("write SRT: %v", err)` becomes `return errf("write SRT: %v", err)`
- `fatalKindf(errNoAudioTrack, "...")` becomes `return kindErrf(errNoAudioTrack, "...")`

The message text must not change by one character — the spec says so and `TestEveryErrKindIsDocumented` and the classifier tests read those strings.

`main()` keeps: flag parsing, the emitter, `-version`, `-probe`, the subcommands, the input-file and key checks in their current order (that order was broken and reverted once — do not touch it), `setupRunTmpDir`, and then:

```go
	if err := runOrdinary(opts); err != nil {
		exitWith(err)
	}
```

- [ ] **Step 9: Run the full suite, especially Task 2's tests**

Run: `make validate`, `go test -race ./...`
Expected: everything passes, including `TestAFullRunLeavesStdoutUnchangedUnderEvents` and both of Task 2's tests, unchanged. **If you changed a test to make it pass, stop and report it** — a test that disagrees with a refactor is telling you the refactor changed behaviour.

- [ ] **Step 10: Prove stdout is byte-identical against the pre-task binary**

```bash
git stash && go build -o /tmp/st_before . && git stash pop && go build -o /tmp/st_after .
# run both over the same stub-driven ordinary run and diff stdout
```

Report the diff (it must be empty) and the exit codes (they must match).

- [ ] **Step 11: Write the in-process test the extraction was for** in `ordinary_test.go`

```go
// TestRunOrdinaryReturnsAClassifiedError is what the extraction bought: the
// default path's failures can now be asserted without spawning a process.
func TestRunOrdinaryReturnsAClassifiedError(t *testing.T) {
	// drive runOrdinary with stubs so that the audio track is missing;
	// assert the returned error's kindOf is errNoAudioTrack and that the
	// message is unchanged from the string the classifier tests expect.
}

// TestRunOrdinaryEndsInOneDoneEvent asserts in-process what Task 2 asserts
// through a process, so a later change that breaks it fails fast and locally.
func TestRunOrdinaryEndsInOneDoneEvent(t *testing.T) { /* ... */ }
```

- [ ] **Step 12: Mutation-verify** — make one converted site return an unclassified `errf` where it used to be `kindErrf`, confirm a test names the wrong kind, restore. Then delete the `exitWith` call in `main` and confirm a test notices that a failing run exits 0.

- [ ] **Step 13: Commit**

```bash
git add ordinary.go ordinary_test.go main.go
git commit -m "refactor: lift the ordinary pipeline into runOrdinary"
```

---

## Task 4: `output_unwritable` — a kind for a path the tool cannot write

**Why (spec S4):** The GUI picks the save path, so a read-only directory or a full disk is a first-class GUI error, and today it arrives as `other` — indistinguishable from a mangled model reply. The GUI cannot route it, so it cannot say "choose somewhere else".

**Files:**
- Modify: `errkind.go` (the constant and `allErrKinds`), `ordinary.go` (the write sites), `progressive.go` (its write sites)
- Modify: `README.md`, the spec's error table
- Test: `errkind_test.go`, `ordinary_test.go`

**Interfaces:**
- Consumes: `kindErrf` and `errKind` from Task 3.
- Produces: `errOutputUnwritable errKind = "output_unwritable"`.

- [ ] **Step 1: Write the failing test**

```go
func TestAnUnwritableOutputPathIsClassified(t *testing.T) {
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {   // readable, not writable
		t.Fatal(err)
	}
	// drive a run whose -out lands inside locked/, assert the error's kind is
	// errOutputUnwritable and that the message still names the path.
}

func TestScratchWritesAreNotOutputWrites(t *testing.T) {
	// A failure writing the run's temp SRT is NOT output_unwritable: the user
	// did not choose that path and cannot fix it by choosing another. Assert it
	// stays errOther.
}
```

The second test is the one that keeps the kind meaningful. Classify only the paths the user picked.

- [ ] **Step 2: Run it and watch it fail** — expected: the kind comes back `other`.

- [ ] **Step 3: Add the constant**

In `errkind.go`, after `errProbeFailed` (grouping the file-and-path problems), and in `allErrKinds()` in the same position — `TestEveryErrKindIsDocumented` reads that list:

```go
	// errOutputUnwritable is a path the user chose that cannot be written: a
	// read-only directory, a full disk, a name the filesystem refuses. Separate
	// from errOther because the GUI picks the save path, so this is the one
	// write failure a user can fix by choosing somewhere else.
	errOutputUnwritable errKind = "output_unwritable"
```

- [ ] **Step 4: Classify the user-chosen writes only**

The sites, as they read after Task 3: the sidecar SRT write (was `main.go:628`), the container mux (was `main.go:667`), and `progressive.go`'s per-chunk output write. Change each from `errf` to `kindErrf(errOutputUnwritable, …)` **keeping the message text exactly as it is**.

Leave alone: the scratch `translated.srt` write (was `main.go:642`) and the transcript sidecar write, which is already a warning rather than a fatal.

- [ ] **Step 5: Run the tests and the suite** → PASS, clean.

- [ ] **Step 6: Mutation-verify** — revert one site to `errf`, confirm red; then reclassify the scratch write *as* `errOutputUnwritable` and confirm `TestScratchWritesAreNotOutputWrites` goes red. Restore both.

- [ ] **Step 7: Document it**

Add the row to the README's kind table and to the spec's Part 1 table, worded like the others. In the spec, note that this kind stays on the Run screen with the message and a Retry button — it is not a settings problem — and remove S4 from the "Still open, deliberately" list.

- [ ] **Step 8: Commit**

```bash
git add errkind.go ordinary.go progressive.go errkind_test.go ordinary_test.go README.md docs/superpowers/specs/2026-09-26-desktop-gui-design.md
git commit -m "feat: classify an unwritable output path"
```

---

## Task 5: progress for the silent phases

**Why:** Capturing ffmpeg's stderr removed its progress counter, so extraction and muxing are silent — forty seconds of nothing on a feature film in the terminal, and no `progress` events for the window either. The lines are still being read; they are just being discarded.

**The approach, and why not `-progress`:** `ffmpegDiagnostics` already scans every stderr line, splitting on CR as well as LF, and drops the ones `isProgressLine` matches. Those lines carry `time=HH:MM:SS.ss`. Parsing what is already in hand costs nothing; `-progress pipe:3` would add a second pipe, a second fd to manage, and a second thing to drain.

**Files:**
- Create: `internal/media/progress.go`, `internal/media/progress_test.go`
- Modify: `internal/media/media.go` (`runFFmpeg`, `ExtractAudio`, `ExtractSubtitle`, `MuxSubtitles`, `SliceWAV`)
- Modify: `ordinary.go`, `progressive.go` (pass a callback)
- Test: `internal/media/media_test.go`, `main_test.go`

**Interfaces:**
- Consumes: `ContainerDuration` for the total; Task 3's `ordinaryOptions` for the wiring.
- Produces: `type Progress func(done, total time.Duration)`, and each exported media function gaining a final `onProgress Progress` parameter that may be nil.

- [ ] **Step 1: Write the failing test** in `internal/media/progress_test.go`

```go
func TestParseFFmpegTime(t *testing.T) {
	for _, tc := range []struct {
		line string
		want time.Duration
		ok   bool
	}{
		{"size=    1024kB time=00:01:23.45 bitrate= 100.0kbits/s speed=1.5x", 83*time.Second + 450*time.Millisecond, true},
		{"frame=  100 fps=25 q=-1.0 size=N/A time=00:00:04.00 bitrate=N/A speed=4x", 4 * time.Second, true},
		{"size=N/A time=N/A bitrate=N/A speed=N/A", 0, false},
		{"[matroska @ 0x1] Invalid data found when processing input", 0, false},
		{"time=01:02:03.99", 3723*time.Second + 990*time.Millisecond, true},
		{"", 0, false},
	} {
		got, ok := parseFFmpegTime(tc.line)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseFFmpegTime(%q) = %v, %v; want %v, %v", tc.line, got, ok, tc.want, tc.ok)
		}
	}
}

// TestRunFFmpegReportsProgress drives a stub ffmpeg that prints CR-terminated
// progress lines and asserts the callback saw them in order.
func TestRunFFmpegReportsProgress(t *testing.T) { /* ... */ }

// TestProgressLinesStillDoNotReachTheDiagnostics guards the thing this task
// could break: a progress line must still be dropped from the error tail.
func TestProgressLinesStillDoNotReachTheDiagnostics(t *testing.T) { /* ... */ }
```

- [ ] **Step 2: Run them and watch them fail** — compile failure, `parseFFmpegTime` undefined.

- [ ] **Step 3: Write `internal/media/progress.go`**

```go
package media

import (
	"strconv"
	"strings"
	"time"
)

// Progress reports how far through a piece of media ffmpeg has got. total is
// zero when the caller does not know the length, which is why it is a duration
// pair rather than a percentage — the media package does not decide how an
// unknown total is presented.
type Progress func(done, total time.Duration)

// parseFFmpegTime reads the media position out of one of ffmpeg's progress
// lines. Those lines end in a carriage return rather than a newline, which is
// why the scanner splits on both; here they arrive already split.
func parseFFmpegTime(line string) (time.Duration, bool) {
	i := strings.Index(line, "time=")
	if i < 0 {
		return 0, false
	}
	field := strings.Fields(line[i+len("time="):])
	if len(field) == 0 {
		return 0, false
	}
	return parseClock(field[0])
}

// parseClock turns HH:MM:SS.ss into a duration. "N/A" — which ffmpeg prints
// before it has read anything — is not a time and reports false rather than
// zero, so a caller cannot mistake "not yet known" for "at the start".
func parseClock(s string) (time.Duration, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	sec, err3 := strconv.ParseFloat(parts[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute +
		time.Duration(sec*float64(time.Second)), true
}
```

- [ ] **Step 4: Thread the callback through `runFFmpeg`**

`ffmpegDiagnostics` currently drops progress lines; give it the callback so it reports them as it goes, and keep dropping them from the tail. Keep the existing contract: drained to EOF before `Wait`, and the tail unchanged on failure.

- [ ] **Step 5: Wire the callers**

`ExtractAudio`, `ExtractSubtitle`, `MuxSubtitles` and `SliceWAV` each gain a final `onProgress Progress` parameter. A nil callback must be a no-op, so existing call sites can pass nil while you work.

In `ordinary.go` and `progressive.go`, pass a callback that does both halves of what was lost:

```go
	onProgress := func(done, total time.Duration) {
		if total <= 0 {
			return
		}
		pct := pctOfDuration(done, total)
		fmt.Printf("\r  extracting: %d%%   ", pct)
		ev.Emit(&events.Progress{Pct: pct})
	}
```

**stdout is a contract.** This adds a line to stdout on a path that previously printed nothing after "Extracting audio track #1...". That is a *new* stdout write, so `TestAFullRunLeavesStdoutUnchangedUnderEvents` still passes (it compares events-on against events-off), but the golden comparison in Task 3 Step 10 would now differ. That is expected and intended here — state it in your report, and make sure the printed percentage and the emitted one agree by construction, the way `pctOf` and `progressLine` already do. Reuse that mechanism rather than writing a second one.

- [ ] **Step 6: Run the tests and the suite** → PASS, clean, `-race` clean.

- [ ] **Step 7: Mutation-verify** — make `parseClock` accept `N/A` as zero, confirm the table test goes red on that row; break the callback wiring so progress is reported after `Wait` instead of during, confirm the ordering test goes red. Restore both.

- [ ] **Step 8: Document it** — remove the ffmpeg-progress item from the spec's carried list and note that extraction and muxing now report progress on both the terminal and the stream.

- [ ] **Step 9: Commit**

```bash
git add internal/media/ ordinary.go progressive.go main_test.go docs/superpowers/specs/2026-09-26-desktop-gui-design.md
git commit -m "feat: report progress while extracting and muxing"
```

---

## Task 6: the fast-mode rule, as data (spec S6)

**Why:** The GUI must disable its fast-mode checkbox with a reason rather than showing a dead control, and the spec forbids it holding a second copy of the rules — the same reason settings go through `sub-translator config`. Today the rule lives in two validators in `progressive.go` and nowhere a caller can read it. A GUI that reimplements "audio source and `.srt` output" is three lines of JavaScript that drift the first time the rule changes.

**Files:**
- Modify: `progressive.go:51-63` (`validateFastFlags`), `progressive.go:67+` (`validateFastSource`)
- Modify: `internal/events/events.go` (the `Probe` event)
- Modify: `main.go` (`probeEvent`)
- Test: `progressive_test.go`, `internal/events/events_test.go`, `main_test.go`
- Modify: `README.md`, the spec

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `var fastSourceModes = []sourceMode{sourceAudio}` and `var fastOutputModes = []outputMode{modeSRT}` in `progressive.go` — the single source of truth both the validators and the probe event read
  - `events.FastRule{Sources []string, Modes []string, MinChunkMinutes int}` and `events.Probe.Fast *FastRule`

- [ ] **Step 1: Write the failing test**

```go
// TestTheProbeEventStatesTheFastModeRule is what lets the GUI disable its
// checkbox without holding a copy of the rule.
func TestTheProbeEventStatesTheFastModeRule(t *testing.T) {
	// run the real binary with -probe -events json, parse the probe event,
	// assert fast.sources == ["audio"], fast.modes == ["srt"],
	// fast.min_chunk_minutes == 1
}

// TestTheStatedRuleIsTheEnforcedRule is the test that makes the single source
// of truth real rather than aspirational: for every combination the probe event
// says is allowed, validateFastFlags and validateFastSource must accept it, and
// for every combination it omits, they must refuse it.
//
// sourceAuto is absent on purpose. validateFastSource runs after resolveSource
// has turned auto into sub or audio — its own comment says so — so auto never
// reaches it, and a row for it would assert about a combination the program
// cannot produce.
func TestTheStatedRuleIsTheEnforcedRule(t *testing.T) {
	rule := fastRule()
	for _, src := range []sourceMode{sourceSub, sourceAudio} {
		for _, mode := range []outputMode{modeSRT, modeMux, modeBoth} {
			allowed := slices.Contains(rule.Sources, src.String()) &&
				slices.Contains(rule.Modes, mode.String())
			errFlags := validateFastFlags(true, mode, rule.MinChunkMinutes)
			errSource := validateFastSource(true, src)
			accepted := errFlags == nil && errSource == nil
			if accepted != allowed {
				t.Errorf("source=%s mode=%s: the rule says allowed=%v, the validators say %v",
					src, mode, allowed, accepted)
			}
		}
	}
}
```

The second test is the whole point. Without it the two can drift and nothing notices, which is the exact failure the "no copy of the rules" constraint exists to prevent — now inside one codebase instead of across two.

- [ ] **Step 2: Run them and watch them fail** — compile failure, `fastRule` undefined.

- [ ] **Step 3: Make one source of truth in `progressive.go`**

```go
// fastSourceModes and fastOutputModes are the fast-mode rule. The validators
// below read them, and so does the probe event, so a caller can be told the rule
// instead of reimplementing it — and the rule cannot be changed in one place
// only. TestTheStatedRuleIsTheEnforcedRule holds the two together.
var (
	fastSourceModes  = []sourceMode{sourceAudio}
	fastOutputModes  = []outputMode{modeSRT}
	fastMinChunkMins = 1
)
```

Rewrite `validateFastFlags` and `validateFastSource` to test membership in those slices, keeping **every error message exactly as it is today** — the messages are asserted by existing tests and read by users.

Add:

```go
// fastRule states the rule as data, for the probe event. The names come from
// each mode's String method, not from a conversion: both types are ints backed
// by iota, so string(m) would yield a control character rather than "srt".
func fastRule() *events.FastRule {
	r := &events.FastRule{MinChunkMinutes: fastMinChunkMins}
	for _, s := range fastSourceModes {
		r.Sources = append(r.Sources, s.String())
	}
	for _, m := range fastOutputModes {
		r.Modes = append(r.Modes, m.String())
	}
	return r
}
```

`sourceMode` is declared in `source.go:9` and `outputMode` in `mode.go:9`; both are
`int` with `iota` constants and a `String()` method, and the strings the method
returns (`"audio"`, `"sub"`, `"auto"`, `"srt"`, `"mux"`, `"both"`) are the same
ones `parseSource` and the `-mode` flag accept. Those are the names to put on the
wire, so a consumer can send back what it was given.

- [ ] **Step 4: Add the event field** in `internal/events/events.go`

```go
// FastRule is the condition under which -fast is accepted, stated so a caller
// can disable a control with a reason rather than copying the rule.
type FastRule struct {
	Sources         []string `json:"sources"`
	Modes           []string `json:"modes"`
	MinChunkMinutes int      `json:"min_chunk_minutes"`
}
```

and on `Probe`: `Fast *FastRule \`json:"fast,omitempty"\``.

- [ ] **Step 5: Fill it in `probeEvent`**, so both the `-probe` path and the ordinary run's probe event carry it — they already share that function, which is why this is one line.

- [ ] **Step 6: Run the tests and the suite** → PASS, clean.

- [ ] **Step 7: Mutation-verify** — add `modeMux` to `fastOutputModes` without touching the validators and confirm `TestTheStatedRuleIsTheEnforcedRule` goes red; then remove the `Fast` assignment from `probeEvent` and confirm the probe test goes red. Restore both.

- [ ] **Step 8: Document it** — the new `fast` object in the README's probe example and field list, and in the spec's Part 1. In Part 2, replace "mirroring what `validateFast` enforces" with a sentence saying the window reads the rule off the `probe` event, and remove S6 from "Still open, deliberately".

- [ ] **Step 9: Commit**

```bash
git add progressive.go internal/events/ main.go progressive_test.go main_test.go README.md docs/superpowers/specs/2026-09-26-desktop-gui-design.md
git commit -m "feat: state the fast-mode rule in the probe event"
```

---

## Task 7: kill the whole process group on a signal (spec M4)

**Why:** SIGINT reaches the CLI but not its ffmpeg child, which is reparented to PID 1. Since the stderr capture landed, the orphan dies of EPIPE on its next write — but a child that never writes again keeps running, and on a feature film that is a full ffmpeg pass continuing after the user cancelled.

**Files:**
- Create: `internal/proc/group_unix.go`, `internal/proc/group_windows.go`, `internal/proc/group_test.go`
- Modify: `internal/media/media.go` (every `exec.Command`), `internal/whisper/whisper.go`
- Modify: `main.go` (the signal handler at `main.go:57`)
- Modify: the spec

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `func proc.Detach(cmd *exec.Cmd)` — puts the child in its own process group before `Start`
  - `func proc.KillGroup(cmd *exec.Cmd) error` — kills that group
  - `func proc.KillAllGroups()` — kills every group started this run, for the signal handler

**Cross-compilation is a global constraint.** `SysProcAttr.Setpgid` does not exist on Windows and `CREATE_NEW_PROCESS_GROUP` does not exist on Unix, so this must be two build-tagged files. `make build-all` must keep building all five platforms; that is the gate. **Windows behaviour here is compile-verified only** — say so in your report rather than claiming it was tested.

- [ ] **Step 1: Write the failing test** in `internal/proc/group_test.go`

```go
//go:build unix

// TestKillGroupKillsAChildThatIgnoresSignals is the case that matters: a child
// that never writes again cannot be reached by EPIPE, so only the group kill
// gets it.
func TestKillGroupKillsAChildThatIgnoresSignals(t *testing.T) {
	cmd := exec.Command("sh", "-c", "trap '' INT; sleep 30")
	Detach(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := KillGroup(cmd); err != nil {
		t.Fatalf("KillGroup: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the child outlived KillGroup by five seconds")
	}
}
```

- [ ] **Step 2: Run it and watch it fail** — compile failure, `Detach` undefined.

- [ ] **Step 3: Write the two build-tagged files**

`group_unix.go` (`//go:build unix`): `Detach` sets `cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}`; `KillGroup` sends `syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)`, tolerating `ESRCH` because a child that already exited is the success case, not an error.

`group_windows.go` (`//go:build windows`): `Detach` sets `CreationFlags: windows CREATE_NEW_PROCESS_GROUP`; `KillGroup` falls back to `cmd.Process.Kill()`. Use `syscall`'s own constant rather than adding `golang.org/x/sys` — **a new dependency is forbidden.**

Both files also maintain the registry `KillAllGroups` walks, guarded by a mutex, with a comment naming its concurrency contract.

- [ ] **Step 4: Call `Detach` before every `Start`** in `internal/media` and `internal/whisper`, and `KillAllGroups` from the signal handler — **after** the `cancelled` event is emitted and **before** `cleanupRunTmpDir`, so the children are gone before the scratch directory they are writing into is removed.

- [ ] **Step 5: Run everything, including the cross-compile**

Run: `make validate`, `go test -race ./...`, `make build-all`
Expected: all clean, all five platforms.

- [ ] **Step 6: Verify on the real binary, and fix the test the orphan shaped**

The existing cancellation test writes the subprocess's stderr to a file rather than a pipe, because the orphan held the pipe open. With the orphan gone, change it to a pipe and confirm the drain returns promptly — that test comment currently documents a workaround this task removes, so the comment goes too.

Then, by hand: start a real transcription on a long file, SIGINT it, and confirm with `ps` that no `ffmpeg` or `whisper-cli` survives. Report the output.

- [ ] **Step 7: Mutation-verify** — remove the `Detach` call from `ExtractAudio`, confirm the signal-handling test goes red; make `KillGroup` treat `ESRCH` as an error, confirm a test covering an already-exited child goes red. Restore both.

- [ ] **Step 8: Document it** — remove M4 from the spec's "Still open, deliberately" list, leaving that list holding nothing, and delete the paragraph explaining the EPIPE consolation since it no longer applies.

- [ ] **Step 9: Commit**

```bash
git add internal/proc/ internal/media/media.go internal/whisper/whisper.go main.go main_test.go docs/superpowers/specs/2026-09-26-desktop-gui-design.md
git commit -m "fix: kill the whole process group when a run is cancelled"
```

---

## Task 8: README and spec, final pass

**Why:** Seven tasks each edited the documentation for their own change. This task reads the result as a whole, which is the only way to catch the sentence that was true before Task 5 and is not now.

**Files:**
- Modify: `README.md`, `docs/superpowers/specs/2026-09-26-desktop-gui-design.md`
- Modify: `main.go` (the `usage` constant, if any flag text is now wrong)

- [ ] **Step 1: Read the README's protocol section against the code**

Every event, every field, every kind, every exit code, every stage name. Check `internal/events/events.go` for which fields carry `omitempty`, and make sure absent-means-unknown is not documented as a default.

- [ ] **Step 2: Capture a real stream and compare it to the worked example**

Run the built binary with stubs, capture the real events for both an ordinary and a `-fast` run, and correct the example. Do not hand-edit it into agreement — capture it.

- [ ] **Step 3: Check the spec's "Still open, deliberately" list**

Tasks 4, 6 and 7 each removed an item. If the list is empty, say so and remove the heading rather than leaving an empty section.

- [ ] **Step 4: Check every claim of the form "X cannot happen"**

This project has shipped four defects behind comments asserting impossibility. Grep the README, the spec and the new files for such claims and verify each against the code. Report any you could not verify — do not soften the wording to make it defensible.

- [ ] **Step 5: Run the suite and commit**

```bash
make validate && go test -race ./... && gofmt -l .
git add README.md docs/superpowers/specs/2026-09-26-desktop-gui-design.md main.go
git commit -m "docs: bring the README and spec up to the CLI as built"
```

---

## Self-Review

**Spec coverage.** The spec's "Still open, deliberately" list held four items: M4 (Task 7), `OPENAI_BASE_URL` with the `runOrdinary` extraction (Tasks 1, 2, 3), S4 (Task 4), S6 (Task 6). The carried ffmpeg `-progress` item is Task 5. Part 1's protocol gains one field (Task 6) and one kind (Task 4); both are documented in Tasks 4, 6 and re-checked in Task 8. Part 2 is untouched except the two sentences Tasks 6 and 4 correct, because the GUI is the next plan.

**Placeholders.** Task 2's helper bodies and Task 3's `ordinaryOptions` fields are deliberately not written out: the first depends on the real signatures of `progressiveStubs` and `dialogueSRT`, which the implementer must read, and the second is a mechanical list derived from `main.go:221-671`. Both steps say exactly where to look and what the result must satisfy. Task 4's and Task 5's test bodies name their assertions precisely. No step says "add error handling" or "write tests for the above".

**Type consistency.** `errKind` and `errOther` come from the existing `errkind.go`; `kindErrf`/`errf`/`kindOf`/`exitWith` are defined in Task 3 and used in Tasks 4 and 5. `events.FastRule` is defined in Task 6 and consumed only by `probeEvent`. `media.Progress` is defined in Task 5 and is the type of the parameter added to four functions in that same task. `proc.Detach`/`KillGroup`/`KillAllGroups` are defined and consumed within Task 7.

**One risk worth naming.** Task 5 adds a stdout line to a path that had none, which is the only place in this plan that changes stdout. It cannot break `TestAFullRunLeavesStdoutUnchangedUnderEvents` (that test compares events-on to events-off), so the step says so explicitly and requires the report to state it — otherwise a reviewer holding the "stdout is a contract" constraint would be right to reject it.
