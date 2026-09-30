package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// writeStubCLI creates an executable shell script at dir/sub-translator that
// prints stdoutContent to stdout, then stderrContent to stderr, then exits
// with code. The content is written to sidecar files and cat'd, rather than
// embedded in the script text, so arbitrarily large or JSON-heavy payloads
// need no shell quoting.
func writeStubCLI(t *testing.T, dir, stdoutContent, stderrContent string, code int) string {
	t.Helper()

	outFile := filepath.Join(dir, "stub.stdout")
	errFile := filepath.Join(dir, "stub.stderr")
	if err := os.WriteFile(outFile, []byte(stdoutContent), 0o644); err != nil {
		t.Fatalf("writing stub stdout fixture: %v", err)
	}
	if err := os.WriteFile(errFile, []byte(stderrContent), 0o644); err != nil {
		t.Fatalf("writing stub stderr fixture: %v", err)
	}

	script := "#!/bin/sh\ncat '" + outFile + "'\ncat '" + errFile + "' 1>&2\nexit " + strconv.Itoa(code) + "\n"
	path := filepath.Join(dir, "sub-translator")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub CLI: %v", err)
	}
	return path
}

// writeSelfSignallingStub creates an executable shell script that sends
// itself SIGTERM before printing anything at all. Go's os/exec reports the
// resulting process as terminated by a signal (ExitCode() == -1), which is
// how the README says the startup-race cancel actually looks on the wire:
// no output, no controlled exit code.
func writeSelfSignallingStub(t *testing.T, dir string) string {
	t.Helper()

	script := "#!/bin/sh\nkill -TERM $$\n"
	path := filepath.Join(dir, "sub-translator")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing self-signalling stub: %v", err)
	}
	return path
}

const versionLine = `{"type":"version","t":"2026-09-30T00:00:00Z","events":1,"tool":"dev"}` + "\n"

// TestProbeReturnsTracksAndTheRulesTheWindowNeeds: the stub answers with a
// version event and a probe event carrying two audio tracks, one subtitle
// track, a -fast rule, embeds_subtitles:true and a duration — every field
// Probe must hand back to the window.
func TestProbeReturnsTracksAndTheRulesTheWindowNeeds(t *testing.T) {
	dir := t.TempDir()
	probeLine := `{"type":"probe","t":"2026-09-30T00:00:01Z",` +
		`"audio":[{"index":0,"lang":"eng","title":"English","codec":"aac"},` +
		`{"index":1,"lang":"jpn","title":"Japanese","codec":"aac"}],` +
		`"subtitles":[{"index":2,"lang":"eng","title":"English","codec":"subrip"}],` +
		`"fast":{"sources":["audio"],"modes":["srt"],"min_chunk_minutes":1},` +
		`"embeds_subtitles":true,"duration":8348.674}` + "\n"

	cli := writeStubCLI(t, dir, "human-readable track listing\n", versionLine+probeLine, 0)

	got, err := ProbeFile(context.Background(), cli, "movie.mkv")
	if err != nil {
		t.Fatalf("Probe() error = %v, want nil", err)
	}
	if got == nil {
		t.Fatal("Probe() returned nil *Probe with a nil error")
	}

	if len(got.Audio) != 2 {
		t.Errorf("len(Audio) = %d, want 2", len(got.Audio))
	} else {
		if got.Audio[0].Lang != "eng" || got.Audio[0].Codec != "aac" {
			t.Errorf("Audio[0] = %+v, want lang=eng codec=aac", got.Audio[0])
		}
		if got.Audio[1].Lang != "jpn" || got.Audio[1].Codec != "aac" {
			t.Errorf("Audio[1] = %+v, want lang=jpn codec=aac", got.Audio[1])
		}
	}
	if len(got.Subtitles) != 1 {
		t.Errorf("len(Subtitles) = %d, want 1", len(got.Subtitles))
	} else if got.Subtitles[0].Lang != "eng" || got.Subtitles[0].Codec != "subrip" {
		t.Errorf("Subtitles[0] = %+v, want lang=eng codec=subrip", got.Subtitles[0])
	}

	if got.Fast == nil {
		t.Fatal("Fast = nil, want a *FastRule")
	}
	if len(got.Fast.Sources) != 1 || got.Fast.Sources[0] != "audio" {
		t.Errorf("Fast.Sources = %v, want [audio]", got.Fast.Sources)
	}
	if len(got.Fast.Modes) != 1 || got.Fast.Modes[0] != "srt" {
		t.Errorf("Fast.Modes = %v, want [srt]", got.Fast.Modes)
	}
	if got.Fast.MinChunkMinutes != 1 {
		t.Errorf("Fast.MinChunkMinutes = %d, want 1", got.Fast.MinChunkMinutes)
	}

	if got.EmbedsSubtitles == nil || !*got.EmbedsSubtitles {
		t.Errorf("EmbedsSubtitles = %v, want a pointer to true", got.EmbedsSubtitles)
	}

	if got.Duration != 8348.674 {
		t.Errorf("Duration = %v, want 8348.674", got.Duration)
	}
}

// TestProbeSurfacesProbeFailed: the real CLI answers an unreadable file with
// version, then error{kind:probe_failed}, then exit 1. The window must be
// able to show that message, not invent "no tracks".
func TestProbeSurfacesProbeFailed(t *testing.T) {
	dir := t.TempDir()
	errLine := `{"type":"error","t":"2026-09-30T00:00:01Z","kind":"probe_failed",` +
		`"message":"probe failed: ffprobe: exit status 1"}` + "\n"
	cli := writeStubCLI(t, dir, "", versionLine+errLine, 1)

	got, err := ProbeFile(context.Background(), cli, "unreadable.mkv")
	if got != nil {
		t.Errorf("Probe() = %+v, want nil *Probe on failure", got)
	}
	if err == nil {
		t.Fatal("Probe() error = nil, want a probe_failed error")
	}
	if errors.Is(err, ErrCancelled) {
		t.Fatalf("Probe() error = %v, want it NOT to be ErrCancelled", err)
	}

	var pf *ProbeFailure
	if !errors.As(err, &pf) {
		t.Fatalf("Probe() error = %v (%T), want a *ProbeFailure", err, err)
	}
	if pf.Kind != "probe_failed" {
		t.Errorf("ProbeFailure.Kind = %q, want %q", pf.Kind, "probe_failed")
	}
	if pf.Message != "probe failed: ffprobe: exit status 1" {
		t.Errorf("ProbeFailure.Message = %q, want the CLI's own message", pf.Message)
	}
}

// TestProbeSurvivesACancelledProbe: a cancel during -probe now yields
// error{kind:cancelled} and exit 130 — the window must not read that as a
// probe failure (a *ProbeFailure), only as ErrCancelled.
func TestProbeSurvivesACancelledProbe(t *testing.T) {
	dir := t.TempDir()
	errLine := `{"type":"error","t":"2026-09-30T00:00:01Z","kind":"cancelled","message":"cancelled"}` + "\n"
	cli := writeStubCLI(t, dir, "", versionLine+errLine, 130)

	got, err := ProbeFile(context.Background(), cli, "movie.mkv")
	if got != nil {
		t.Errorf("Probe() = %+v, want nil *Probe on cancel", got)
	}
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("Probe() error = %v, want errors.Is(err, ErrCancelled)", err)
	}

	var pf *ProbeFailure
	if errors.As(err, &pf) {
		t.Fatalf("Probe() error = %v is a *ProbeFailure (kind=%q); a cancel must not be read as a probe failure", err, pf.Kind)
	}
}

// TestProbeReportsASignalledExitWithAnEmptyStream: in the few milliseconds
// before the CLI arms its signal handler, a cancel kills it with nothing on
// the stream at all — no version, no error. The README says to read that as
// "cancelled before the tool could answer", and Probe must not present it as
// a crash (a bare "running cli: exit status" error) or a probe failure.
func TestProbeReportsASignalledExitWithAnEmptyStream(t *testing.T) {
	dir := t.TempDir()
	cli := writeSelfSignallingStub(t, dir)

	got, err := ProbeFile(context.Background(), cli, "movie.mkv")
	if got != nil {
		t.Errorf("Probe() = %+v, want nil *Probe", got)
	}
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("Probe() error = %v, want errors.Is(err, ErrCancelled)", err)
	}
}

// TestProbeReturnsPromptlyAgainstAChattyChild: -probe also prints a
// human-readable track listing on stdout, which ProbeFile discards via
// cmd.Stdout = io.Discard rather than reading it. os/exec already guarantees
// that assignment (and even a nil Stdout, which it sends to /dev/null) never
// blocks the child, so this test is not proving "forgot to drain" — there is
// nothing left to forget. What it guards is a regression: a future change
// back to cmd.StdoutPipe() without a goroutine draining it, which would
// reintroduce exactly the hang this test would catch. The stub prints ~235
// KB of prose on stdout, comfortably past any platform's pipe buffer, to
// make that regression visible as a timeout rather than a flake.
func TestProbeReturnsPromptlyAgainstAChattyChild(t *testing.T) {
	dir := t.TempDir()
	probeLine := `{"type":"probe","t":"2026-09-30T00:00:01Z","audio":[],"subtitles":[]}` + "\n"
	bigStdout := strings.Repeat("track listing padding to exceed a pipe buffer\n", 5000) // ~235 KB
	cli := writeStubCLI(t, dir, bigStdout, versionLine+probeLine, 0)

	type result struct {
		probe *Probe
		err   error
	}
	done := make(chan result, 1)
	go func() {
		p, err := ProbeFile(context.Background(), cli, "movie.mkv")
		done <- result{p, err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Probe() error = %v, want nil", r.err)
		}
		if r.probe == nil {
			t.Fatal("Probe() returned nil *Probe with a nil error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Probe() did not return within 10s — an unread stdout pipe likely filled and blocked the child")
	}
}

// --- Run / Start / Cancel -------------------------------------------------

// writeSlowStub creates a stub CLI that emits version then stage
// immediately, sleeps for sleep, then emits done and exits 0.
// TestStartStreamsEventsAsTheyArriveNotAfterExit uses the gap between the
// stage event's arrival and Wait's return to prove events reach on() while
// the child is still running, not only after it exits.
func writeSlowStub(t *testing.T, dir string, sleep time.Duration) string {
	t.Helper()

	script := "#!/bin/sh\n" +
		"echo '" + strings.TrimRight(versionLine, "\n") + "' 1>&2\n" +
		"echo '{\"type\":\"stage\",\"t\":\"2026-09-30T00:00:00Z\",\"name\":\"translate\"}' 1>&2\n" +
		"sleep " + fmt.Sprintf("%.2f", sleep.Seconds()) + "\n" +
		"echo '{\"type\":\"done\",\"t\":\"2026-09-30T00:00:01Z\",\"path\":\"/out/movie.es.srt\"}' 1>&2\n" +
		"exit 0\n"
	path := filepath.Join(dir, "sub-translator")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing slow stub CLI: %v", err)
	}
	return path
}

// writeTrapCLI creates a stub CLI that traps SIGINT, emits version
// immediately, then blocks until interrupted, then sleeps briefly —
// simulating the real CLI's own graceful shutdown between the signal
// landing and its process-group kill actually finishing — touches a marker
// file, emits error{cancelled} and exits 130. TestCancelSendsSIGINTAndWaitsFor130
// and TestCancelDoesNotOrphanTheChild use the marker file to prove Cancel
// waits for that whole sequence rather than returning the moment the
// signal is sent.
func writeTrapCLI(t *testing.T, dir string) (cli, marker string) {
	t.Helper()

	marker = filepath.Join(dir, "marker")
	script := "#!/bin/sh\n" +
		"trapped=\n" +
		"trap 'trapped=1' INT\n" +
		"echo '" + strings.TrimRight(versionLine, "\n") + "' 1>&2\n" +
		"i=0\n" +
		"while [ -z \"$trapped\" ] && [ $i -lt 1000 ]; do\n" +
		"  sleep 0.01\n" +
		"  i=$((i+1))\n" +
		"done\n" +
		"sleep 0.3\n" +
		"touch '" + marker + "'\n" +
		"echo '{\"type\":\"error\",\"t\":\"2026-09-30T00:00:01Z\",\"kind\":\"cancelled\",\"message\":\"cancelled\"}' 1>&2\n" +
		"exit 130\n"
	path := filepath.Join(dir, "sub-translator")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing trap stub CLI: %v", err)
	}
	return path, marker
}

// runOpts is a minimal, valid RunOptions for the stub CLIs in this file,
// which ignore argv entirely — only the values matter for tests that
// inspect what Start built, which none here do.
func runOpts() RunOptions {
	return RunOptions{Input: "movie.mkv", From: "en", To: "es", Track: -1, Atrack: -1}
}

// writeSilentBlockingStub creates a stub that says nothing at all and blocks
// until it is signalled. `exec sleep` replaces the shell, so the SIGINT
// Cancel sends reaches a process whose default disposition is to die by it:
// a signalled exit with an empty stream, which is the startup-race cancel
// exactly as the README describes it. (If the exec has not happened yet the
// signal lands on the shell instead, which also dies by it with nothing
// written — the same shape either way, so the test has no race to lose.)
func writeSilentBlockingStub(t *testing.T, dir string) string {
	t.Helper()

	script := "#!/bin/sh\nexec sleep 30\n"
	path := filepath.Join(dir, "sub-translator")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing silent blocking stub: %v", err)
	}
	return path
}

// TestStartStreamsEventsAsTheyArriveNotAfterExit is the test the task
// brief's trap exists to prevent silently passing under: a refactor to the
// buffer-then-decode shape ProbeFile uses (correct for a short probe, wrong
// for a run that streams stage and progress events for minutes) would still
// pass every other test in this file, because none of them distinguish "the
// stage event arrived while the child was still running" from "the stage
// event arrived only once the child had already exited". This one does, by
// timing the gap between the two.
func TestStartStreamsEventsAsTheyArriveNotAfterExit(t *testing.T) {
	dir := t.TempDir()
	const sleep = 400 * time.Millisecond
	cli := writeSlowStub(t, dir, sleep)

	var stageAt time.Time
	got := make(chan struct{})
	r, err := Start(context.Background(), cli, runOpts(), func(ev Event) error {
		if ev.Type == "stage" {
			stageAt = time.Now()
			close(got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("stage event never reached on()")
	}

	outcome, err := r.Wait()
	waitReturnedAt := time.Now()
	if err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}
	if outcome.Done == nil {
		t.Fatal("outcome.Done is nil, want the stub's done event")
	}

	gap := waitReturnedAt.Sub(stageAt)
	if gap < sleep/2 {
		t.Fatalf("stage event arrived only %v before Wait() returned (the stub sleeps %v between "+
			"emitting stage and emitting done) — this is what buffering the whole stream until "+
			"the process exits, instead of decoding it live, would look like", gap, sleep)
	}
}

// TestRunRefusesTwoTerminalEvents: the CLI guarantees exactly one terminal
// event and an exit code that agrees with it. The window's completion
// screen is built on that guarantee, so a stream that violates it — here, a
// done followed by an error{cancelled} — must come back as an error naming
// both, not a silently-picked winner.
func TestRunRefusesTwoTerminalEvents(t *testing.T) {
	dir := t.TempDir()
	stderr := versionLine +
		`{"type":"done","t":"2026-09-30T00:00:01Z","path":"/out/movie.es.srt"}` + "\n" +
		`{"type":"error","t":"2026-09-30T00:00:02Z","kind":"cancelled","message":"cancelled"}` + "\n"
	cli := writeStubCLI(t, dir, "", stderr, 130)

	r, err := Start(context.Background(), cli, runOpts(), func(Event) error { return nil })
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	outcome, err := r.Wait()
	if outcome != nil {
		t.Errorf("Wait() outcome = %+v, want nil on a broken stream", outcome)
	}
	if err == nil {
		t.Fatal("Wait() error = nil, want an error naming both terminal events")
	}
	if !strings.Contains(err.Error(), "done") || !strings.Contains(err.Error(), "error") {
		t.Errorf("Wait() error = %q, want it to name both the done and the error event", err.Error())
	}
}

// TestRunPairsTheExitCodeWithTheEnding: the CLI guarantees the exit code
// agrees with which terminal event it sent — 0 for done, 130 for a
// cancelled error, otherwise 1 (or 2 for a usage error, not exercised
// here). A stream that disagrees with its own exit code is as broken as one
// with the wrong number of terminal events.
func TestRunPairsTheExitCodeWithTheEnding(t *testing.T) {
	cases := []struct {
		name    string
		stderr  string
		code    int
		wantErr bool
	}{
		{
			name:    "done and 0 agree",
			stderr:  versionLine + `{"type":"done","t":"x","path":"/out/movie.es.srt"}` + "\n",
			code:    0,
			wantErr: false,
		},
		{
			name:    "error and 1 agree",
			stderr:  versionLine + `{"type":"error","t":"x","kind":"other","message":"boom"}` + "\n",
			code:    1,
			wantErr: false,
		},
		{
			name:    "cancelled error and 130 agree",
			stderr:  versionLine + `{"type":"error","t":"x","kind":"cancelled","message":"cancelled"}` + "\n",
			code:    130,
			wantErr: false,
		},
		{
			name:    "done and 1 disagree",
			stderr:  versionLine + `{"type":"done","t":"x","path":"/out/movie.es.srt"}` + "\n",
			code:    1,
			wantErr: true,
		},
		{
			name:    "error and 0 disagree",
			stderr:  versionLine + `{"type":"error","t":"x","kind":"other","message":"boom"}` + "\n",
			code:    0,
			wantErr: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			cli := writeStubCLI(t, dir, "", c.stderr, c.code)

			r, err := Start(context.Background(), cli, runOpts(), func(Event) error { return nil })
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}
			outcome, err := r.Wait()

			if c.wantErr {
				if err == nil {
					t.Fatalf("Wait() error = nil, want an exit-code-disagreement error (outcome=%+v)", outcome)
				}
				if outcome != nil {
					t.Errorf("Wait() outcome = %+v, want nil alongside the error", outcome)
				}
				return
			}
			if err != nil {
				t.Fatalf("Wait() error = %v, want nil", err)
			}
			if outcome == nil {
				t.Fatal("Wait() outcome = nil, want a populated *Outcome")
			}
			if outcome.ExitCode != c.code {
				t.Errorf("outcome.ExitCode = %d, want %d", outcome.ExitCode, c.code)
			}
		})
	}
}

// TestRunCollectsEverySavedFile: -mode both emits one saved event per file
// (a sidecar and a container), so the window can offer "Show in Finder" for
// each. Run must collect all of them, in order, not just the last one.
func TestRunCollectsEverySavedFile(t *testing.T) {
	dir := t.TempDir()
	stderr := versionLine +
		`{"type":"saved","t":"x","path":"/out/movie.es.srt","watchable":false}` + "\n" +
		`{"type":"saved","t":"x","path":"/out/movie.mkv","covers":"container","watchable":false}` + "\n" +
		`{"type":"done","t":"x","path":"/out/movie.mkv","covers":"container"}` + "\n"
	cli := writeStubCLI(t, dir, "", stderr, 0)

	r, err := Start(context.Background(), cli, RunOptions{Input: "movie.mkv", From: "en", To: "es", Mode: "both", Track: -1}, func(Event) error { return nil })
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	outcome, err := r.Wait()
	if err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}
	if outcome == nil {
		t.Fatal("Wait() outcome = nil")
	}

	if len(outcome.Saved) != 2 {
		t.Fatalf("len(Saved) = %d, want 2: %+v", len(outcome.Saved), outcome.Saved)
	}
	if outcome.Saved[0].Path != "/out/movie.es.srt" {
		t.Errorf("Saved[0].Path = %q, want the sidecar path", outcome.Saved[0].Path)
	}
	if outcome.Saved[1].Path != "/out/movie.mkv" {
		t.Errorf("Saved[1].Path = %q, want the container path", outcome.Saved[1].Path)
	}
	if outcome.Saved[1].Covers != "container" {
		t.Errorf("Saved[1].Covers = %q, want %q", outcome.Saved[1].Covers, "container")
	}
}

// TestCancelSendsSIGINTAndWaitsFor130: against a stub that traps INT,
// Cancel must deliver the signal (the stub only reacts to INT, not TERM or
// KILL) and then block until the stub's own graceful shutdown — simulated
// here by a sleep before it touches a marker file — has actually finished.
func TestCancelSendsSIGINTAndWaitsFor130(t *testing.T) {
	dir := t.TempDir()
	cli, marker := writeTrapCLI(t, dir)

	ready := make(chan struct{})
	r, err := Start(context.Background(), cli, runOpts(), func(ev Event) error {
		if ev.Type == "version" {
			close(ready)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("stub never emitted version — trap may not be installed yet")
	}

	if err := r.Cancel(); err != nil {
		t.Fatalf("Cancel() error = %v, want nil", err)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker file missing right after Cancel() returned — Cancel returned before "+
			"the stub's graceful shutdown (which writes it right before exiting) finished: %v", err)
	}

	outcome, err := r.Wait()
	if err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}
	if outcome.Err == nil || outcome.Err.Kind != "cancelled" {
		t.Fatalf("outcome.Err = %+v, want kind=cancelled", outcome.Err)
	}
	if outcome.ExitCode != 130 {
		t.Errorf("outcome.ExitCode = %d, want 130", outcome.ExitCode)
	}
}

// TestCancelDoesNotOrphanTheChild: the CLI kills its own process group, so
// Cancel must wait for the process rather than returning while it dies. A
// GUI that quits as soon as Cancel returns must not leave an ffmpeg (or, in
// this stub, the shell itself) running — Cancel returning is only correct
// once cmd.Wait has actually reaped the child.
func TestCancelDoesNotOrphanTheChild(t *testing.T) {
	dir := t.TempDir()
	cli, marker := writeTrapCLI(t, dir)

	ready := make(chan struct{})
	r, err := Start(context.Background(), cli, runOpts(), func(ev Event) error {
		if ev.Type == "version" {
			close(ready)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("stub never emitted version — trap may not be installed yet")
	}

	if err := r.Cancel(); err != nil {
		t.Fatalf("Cancel() error = %v, want nil", err)
	}

	// r.cmd is unexported but this test lives in the same package: it can
	// check directly that the child was actually reaped, which is the one
	// fact a marker file alone cannot prove.
	if r.cmd.ProcessState == nil {
		t.Fatal("Cancel() returned before the child was reaped: cmd.ProcessState is nil")
	}
	if code := r.cmd.ProcessState.ExitCode(); code != 130 {
		t.Errorf("reaped child's exit code = %d, want 130", code)
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker file missing after Cancel() returned: the stub's own graceful shutdown "+
			"did not finish before Cancel returned: %v", err)
	}
}

// TestStartReportsASignalledExitWithAnEmptyStreamAsCancelled mirrors
// TestProbeReportsASignalledExitWithAnEmptyStream: a cancel landing in the
// few milliseconds before the CLI arms its signal handler kills it with
// nothing on the stream at all — no version, no error — which is exactly
// what the GUI's own Cancel button produces if it's clicked the instant a
// run starts. That must come back as ErrCancelled, the same sentinel
// ProbeFile already uses for its identical case, not as a "stream broke its
// contract" error: the run wasn't broken, it was cancelled.
func TestStartReportsASignalledExitWithAnEmptyStreamAsCancelled(t *testing.T) {
	dir := t.TempDir()
	cli := writeSelfSignallingStub(t, dir)

	r, err := Start(context.Background(), cli, runOpts(), func(Event) error { return nil })
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	outcome, err := r.Wait()
	if outcome != nil {
		t.Errorf("Wait() outcome = %+v, want nil", outcome)
	}
	if !errors.Is(err, ErrCancelled) {
		t.Fatalf("Wait() error = %v, want errors.Is(err, ErrCancelled)", err)
	}
}

// --- argv -----------------------------------------------------------------

// TestBuildRunArgs is the argv coverage the spec's Testing section asks for —
// "argv construction from the form's state, for every combination of source,
// mode and fast" — and which no test in this file had. Its absence is how
// -atrack went missing: every other test here uses a stub that ignores argv
// entirely, so a run could be composed with the audio index on the wrong flag
// and nothing would notice but a user watching the wrong dub.
func TestBuildRunArgs(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts RunOptions
		want []string
	}{
		{
			name: "transcribe an audio track to a sidecar",
			opts: RunOptions{Input: "movie.mkv", To: "es", Mode: "srt", Source: "audio", Track: -1, Atrack: 3},
			want: []string{
				"-events", "json", "-to", "es", "-mode", "srt", "-source", "audio",
				"-track", "-1", "-atrack", "3", "movie.mkv",
			},
		},
		{
			name: "translate a subtitle track, which needs -from",
			opts: RunOptions{Input: "movie.mkv", From: "en", To: "es", Mode: "srt", Source: "sub", Track: 2, Atrack: -1},
			want: []string{
				"-events", "json", "-from", "en", "-to", "es", "-mode", "srt", "-source", "sub",
				"-track", "2", "-atrack", "-1", "movie.mkv",
			},
		},
		{
			name: "mux, with a chosen output path",
			opts: RunOptions{Input: "movie.mkv", To: "es", Mode: "mux", Out: "/films/out.mkv", Source: "audio", Track: -1, Atrack: 0},
			want: []string{
				"-events", "json", "-to", "es", "-mode", "mux", "-out", "/films/out.mkv",
				"-source", "audio", "-track", "-1", "-atrack", "0", "movie.mkv",
			},
		},
		{
			name: "both, fast",
			opts: RunOptions{Input: "movie.mkv", To: "es", Mode: "both", Source: "audio", Track: -1, Atrack: 1, Fast: true},
			want: []string{
				"-events", "json", "-to", "es", "-mode", "both", "-source", "audio",
				"-track", "-1", "-atrack", "1", "-fast", "movie.mkv",
			},
		},
		{
			name: "nothing but the input: the CLI's own defaults stand",
			opts: RunOptions{Input: "movie.mkv", Track: -1, Atrack: -1},
			want: []string{"-events", "json", "-track", "-1", "-atrack", "-1", "movie.mkv"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := buildRunArgs(tc.opts)
			if !slices.Equal(got, tc.want) {
				t.Errorf("buildRunArgs()\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// TestTrackIndicesPutsTheChosenStreamOnTheFlagThatReadsIt is defect 1 pinned
// at its source: -track is the subtitle index and -atrack is the audio one,
// and the File screen offers both lists as one choice, so something has to
// decide which flag the chosen index belongs on.
func TestTrackIndicesPutsTheChosenStreamOnTheFlagThatReadsIt(t *testing.T) {
	for _, tc := range []struct {
		source        string
		chosen        int
		track, atrack int
		why           string
	}{
		{"audio", 3, -1, 3, "an audio index on -track is read by nothing: the CLI picks its own stream"},
		{"audio", 0, -1, 0, "index 0 is a real stream, not an unset value"},
		{"sub", 2, 2, -1, "a subtitle index belongs on -track"},
		{"auto", 5, -1, -1, "nothing was chosen from either list, so neither flag is set"},
		{"", 5, -1, -1, "same, for an unset source"},
	} {
		t.Run(tc.source+"/"+strconv.Itoa(tc.chosen), func(t *testing.T) {
			track, atrack := trackIndices(tc.source, tc.chosen)
			if track != tc.track || atrack != tc.atrack {
				t.Errorf("trackIndices(%q, %d) = (%d, %d), want (%d, %d) — %s",
					tc.source, tc.chosen, track, atrack, tc.track, tc.atrack, tc.why)
			}
		})
	}
}

// --- Cancel's error contract ---------------------------------------------

// TestCancelInTheStartupRaceIsNotAFailure pins the first of Cancel's two
// undocumented cases. A cancel landing before the CLI arms its handler kills
// it with nothing on the stream, which Wait reports as ErrCancelled — the
// cancel having worked exactly as asked. Cancel must not pass that on as its
// own error: the Cancel button's only use for a non-nil return is to put a
// dialog on screen, and a dialog for a successful cancel is a bug the user
// sees every time they cancel a run they just started.
func TestCancelInTheStartupRaceIsNotAFailure(t *testing.T) {
	dir := t.TempDir()
	cli := writeSilentBlockingStub(t, dir)

	r, err := Start(context.Background(), cli, runOpts(), func(Event) error { return nil })
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	if err := r.Cancel(); err != nil {
		t.Errorf("Cancel() in the startup race = %v, want nil: the cancel succeeded, and a "+
			"non-nil return here is what the Run screen shows an error dialog for", err)
	}

	// Wait still tells the whole truth: the sentinel stays where a caller asks
	// how the run ended, rather than being swallowed along with the error.
	outcome, waitErr := r.Wait()
	if outcome != nil {
		t.Errorf("Wait() outcome = %+v, want nil", outcome)
	}
	if !errors.Is(waitErr, ErrCancelled) {
		t.Errorf("Wait() error = %v, want errors.Is(err, ErrCancelled)", waitErr)
	}
}

// TestCancelAfterTheRunAlreadyEndedReportsTheRealOutcome pins the second
// case. The Cancel button is live until the page redraws, so it can be
// pressed on a run that has already finished. Signalling a reaped process
// returns os.ErrProcessDone, and reporting that told the user "os: process
// already finished" about a run that had in fact completed successfully —
// a signalling complaint standing in for the run's real ending.
func TestCancelAfterTheRunAlreadyEndedReportsTheRealOutcome(t *testing.T) {
	dir := t.TempDir()
	stderr := versionLine +
		`{"type":"saved","t":"2026-09-30T00:00:01Z","path":"/out/movie.es.srt","watchable":false}` + "\n" +
		`{"type":"done","t":"2026-09-30T00:00:02Z","path":"/out/movie.es.srt"}` + "\n"
	cli := writeStubCLI(t, dir, "", stderr, 0)

	r, err := Start(context.Background(), cli, runOpts(), func(Event) error { return nil })
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	// Let it finish naturally, and let the stream goroutine's cmd.Wait reap
	// it — which is what makes a later Signal return os.ErrProcessDone.
	outcome, err := r.Wait()
	if err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}
	if outcome.Done == nil {
		t.Fatalf("outcome = %+v, want a done event", outcome)
	}

	if err := r.Cancel(); err != nil {
		t.Errorf("Cancel() after the run already finished = %v, want nil: there was nothing "+
			"left to cancel, and the run's real ending was a success", err)
	}

	// And the outcome is unchanged by having been cancelled after the fact.
	again, err := r.Wait()
	if err != nil || again.Done == nil {
		t.Errorf("Wait() after Cancel() = (%+v, %v), want the same done outcome", again, err)
	}
}
