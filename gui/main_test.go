package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

// writeCountingStub creates a stub CLI that records one line per invocation in
// dir/runs.log, emits version, then blocks until interrupted — long enough for
// every goroutine in TestStartRunAcceptsExactlyOneRunUnderARace to have raced,
// and interruptible so the test can reap it.
func writeCountingStub(t *testing.T, dir string) (cli, log string) {
	t.Helper()

	log = filepath.Join(dir, "runs.log")
	script := "#!/bin/sh\n" +
		"echo started >> '" + log + "'\n" +
		"echo '" + strings.TrimRight(versionLine, "\n") + "' 1>&2\n" +
		"trapped=\n" +
		"trap 'trapped=1' INT\n" +
		"i=0\n" +
		"while [ -z \"$trapped\" ] && [ $i -lt 500 ]; do\n" +
		"  sleep 0.01\n" +
		"  i=$((i+1))\n" +
		"done\n" +
		"echo '{\"type\":\"error\",\"t\":\"2026-09-30T00:00:01Z\",\"kind\":\"cancelled\",\"message\":\"cancelled\"}' 1>&2\n" +
		"exit 130\n"

	cli = filepath.Join(dir, "sub-translator")
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatalf("writing counting stub: %v", err)
	}
	return cli, log
}

// testApp is an App wired to a window that does not exist: its emit seam
// collects what it would have told the page instead of calling into Wails,
// whose EventsEmit needs a context only Wails can build.
func testApp(t *testing.T, cli string) (*App, func() []string) {
	t.Helper()

	var mu sync.Mutex
	var sent []string
	app := &App{
		cli:  cli,
		film: "movie.mkv",
		emit: func(_ context.Context, name string, _ ...interface{}) {
			mu.Lock()
			sent = append(sent, name)
			mu.Unlock()
		},
	}
	return app, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), sent...)
	}
}

// TestStartRunAcceptsExactlyOneRunUnderARace is finding I-3 pinned.
//
// Wails dispatches each binding message on a goroutine of its own, so two
// Start clicks are two concurrent StartRun calls. When the check on a.run and
// the claim of it are not one critical section, both see nothing in flight and
// both spawn a CLI — and the second assignment to a.run orphans the first
// child from CancelRun and from beforeClose, so the window quits with a
// transcription still running. The page's own `running` flag hides this, which
// is exactly the problem: it puts a Go invariant in the one layer of this
// project with no test harness.
//
// The stub counts its own invocations, so the assertion is on the thing that
// actually matters — how many children exist — not on how many calls returned
// an error.
func TestStartRunAcceptsExactlyOneRunUnderARace(t *testing.T) {
	dir := t.TempDir()
	cli, log := writeCountingStub(t, dir)
	app, _ := testApp(t, cli)

	const racers = 16
	var wg sync.WaitGroup
	errs := make([]error, racers)
	gun := make(chan struct{})

	for i := range racers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gun
			errs[i] = app.StartRun(RunForm{Source: "audio", Track: 0, To: "es", Mode: "srt"})
		}(i)
	}
	close(gun)
	wg.Wait()

	accepted := 0
	for _, err := range errs {
		if err == nil {
			accepted++
		}
	}
	if accepted != 1 {
		t.Errorf("%d of %d concurrent StartRun calls were accepted, want exactly 1", accepted, racers)
	}

	// The claim is released once the child exists, so a run is now in flight
	// and reachable.
	app.mu.Lock()
	run, starting := app.run, app.starting
	app.mu.Unlock()
	if run == nil {
		t.Fatal("no run in flight after an accepted StartRun")
	}
	if starting != nil {
		t.Error("app.starting is still set after StartRun returned: the claim was never released")
	}

	// Count before cancelling, and only once a child has actually recorded
	// itself: SIGINT sent in the microseconds before the shell runs its first
	// line kills it with nothing written, which would make this assertion pass
	// for the wrong reason. The settle afterwards gives any *second* child —
	// the thing being ruled out — the same chance to be counted, since two
	// racing spawns land within microseconds of each other.
	waitForAChild(t, log)
	time.Sleep(300 * time.Millisecond)
	started := countLines(t, log)
	if started != 1 {
		t.Errorf("the stub CLI ran %d times, want 1 — every child beyond the first is one "+
			"that CancelRun and beforeClose cannot reach, left transcribing after the window quits",
			started)
	}

	if err := app.CancelRun(); err != nil {
		t.Errorf("CancelRun() = %v, want nil", err)
	}
}

// waitForAChild blocks until the counting stub has recorded at least one
// invocation, so a test that cancels does not race the shell's first line.
func waitForAChild(t *testing.T, log string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for countLines(t, log) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the stub CLI never recorded an invocation within 10s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStartRunReleasesTheClaimWhenTheChildCannotStart: the claim must not
// outlive a failed spawn, or the Start button is dead for the rest of the
// window's life with no run to show for it.
func TestStartRunReleasesTheClaimWhenTheChildCannotStart(t *testing.T) {
	dir := t.TempDir()
	app, _ := testApp(t, filepath.Join(dir, "there-is-no-binary-here"))

	if err := app.StartRun(RunForm{Source: "audio", Track: 0, To: "es", Mode: "srt"}); err == nil {
		t.Fatal("StartRun() = nil for a CLI that does not exist, want an error")
	}

	app.mu.Lock()
	run, starting := app.run, app.starting
	app.mu.Unlock()
	if starting != nil || run != nil {
		t.Fatalf("after a failed start: starting=%v run=%v, want both clear", starting != nil, run != nil)
	}

	// And a second attempt is not refused as "a run is already going".
	cli, log := writeCountingStub(t, dir)
	app.mu.Lock()
	app.cli = cli
	app.mu.Unlock()

	if err := app.StartRun(RunForm{Source: "audio", Track: 0, To: "es", Mode: "srt"}); err != nil {
		t.Fatalf("StartRun() after a failed start = %v, want nil — the claim wedged the button", err)
	}
	waitForAChild(t, log)
	if n := countLines(t, log); n != 1 {
		t.Errorf("the stub ran %d times, want 1", n)
	}
	if err := app.CancelRun(); err != nil {
		t.Errorf("CancelRun() = %v, want nil", err)
	}
}

// TestCancelRunWithNothingRunningIsNotAnError: the Cancel button stays live
// until the page redraws, so it can be pressed on a run that has already
// ended, and on a window that has never run anything.
func TestCancelRunWithNothingRunningIsNotAnError(t *testing.T) {
	app, _ := testApp(t, "")
	if err := app.CancelRun(); err != nil {
		t.Errorf("CancelRun() with no run = %v, want nil", err)
	}
}

// TestStartRunReportsTheEndingToThePage: a run that ends must reach the page
// through run:ended, or the Run screen sits on a progress bar forever.
func TestStartRunReportsTheEndingToThePage(t *testing.T) {
	dir := t.TempDir()
	stderr := versionLine +
		`{"type":"done","t":"2026-09-30T00:00:02Z","path":"/out/movie.es.srt"}` + "\n"
	cli := writeStubCLI(t, dir, "", stderr, 0)

	app, sent := testApp(t, cli)
	if err := app.StartRun(RunForm{Source: "audio", Track: 0, To: "es", Mode: "srt"}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		if slicesContains(sent(), "run:ended") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no run:ended within 10s; the page was told %v", sent())
		}
		time.Sleep(10 * time.Millisecond)
	}

	// And the run slot is free again, so the next Start is accepted.
	app.mu.Lock()
	run := app.run
	app.mu.Unlock()
	if run != nil {
		t.Error("app.run is still set after the run ended: the next StartRun would be refused")
	}
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("reading %s: %v", path, err)
	}
	return len(strings.Fields(string(body)))
}

// writeLongLivedStub creates a stub CLI that emits version, then runs for half
// a minute — and, crucially, ends differently depending on whether it was
// interrupted: error{cancelled} with exit 130 if it was, done with exit 0 if it
// simply ran out.
//
// Both halves are load-bearing. The half-minute means a run that ends promptly
// ended because a signal reached it rather than because it happened to finish.
// The two different endings mean the assertion can tell those apart at all:
// writeCountingStub emits the cancelled error either way, so against it "the
// run ended as a cancel" would have been true of a child nobody ever signalled.
func writeLongLivedStub(t *testing.T, dir string) (cli, log string) {
	t.Helper()

	log = filepath.Join(dir, "runs.log")
	script := "#!/bin/sh\n" +
		"echo started >> '" + log + "'\n" +
		"echo '" + strings.TrimRight(versionLine, "\n") + "' 1>&2\n" +
		"trapped=\n" +
		"trap 'trapped=1' INT\n" +
		"i=0\n" +
		"while [ -z \"$trapped\" ] && [ $i -lt 3000 ]; do\n" +
		"  sleep 0.01\n" +
		"  i=$((i+1))\n" +
		"done\n" +
		"if [ -n \"$trapped\" ]; then\n" +
		"  echo '{\"type\":\"error\",\"t\":\"2026-09-30T00:00:01Z\",\"kind\":\"cancelled\",\"message\":\"cancelled\"}' 1>&2\n" +
		"  exit 130\n" +
		"fi\n" +
		"echo '{\"type\":\"done\",\"t\":\"2026-09-30T00:00:02Z\",\"path\":\"/out/movie.es.srt\"}' 1>&2\n" +
		"exit 0\n"

	cli = filepath.Join(dir, "sub-translator")
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatalf("writing long-lived stub: %v", err)
	}
	return cli, log
}

// enterTheStartupWindow puts app into the state StartRun holds it in between
// claiming the run slot and its child existing — starting non-nil, run still
// nil — and returns the release StartRun performs on the way out.
//
// It is the same two writes under the same lock, in the same order, because
// nothing else can hold that window open: it is the length of one exec syscall
// and a test cannot make exec slow. What makes the simulation honest is that
// StartRun's own tests drive the real claim and the real release; these two
// only need the window to stand still while another reader of the slot walks
// into it.
func enterTheStartupWindow(t *testing.T, app *App) (release func(*Run)) {
	t.Helper()

	claimed := make(chan struct{})
	app.mu.Lock()
	if app.run != nil || app.starting != nil {
		app.mu.Unlock()
		t.Fatal("the run slot was not free before the startup window was entered")
	}
	app.starting = claimed
	app.stage, app.pct = "", 0
	app.mu.Unlock()

	return func(run *Run) {
		app.mu.Lock()
		app.starting = nil
		app.run = run
		app.mu.Unlock()
		close(claimed)
	}
}

// startTheChild starts the stub the way StartRun's own Start call would, and
// waits until it has recorded itself so a cancel cannot land before the shell
// has armed its trap.
func startTheChild(t *testing.T, cli, log string) *Run {
	t.Helper()

	run, err := Start(context.Background(), cli, RunOptions{
		Input:  "movie.mkv",
		To:     "es",
		Mode:   "srt",
		Source: "audio",
		Track:  -1,
		Atrack: 0,
	}, nil)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = run.Cancel() })
	waitForAChild(t, log)
	return run
}

// assertCancelled fails unless the run has already ended, and ended as a
// cancel — which is the only evidence that matters, because it is evidence the
// signal reached the child rather than that some function reported success.
//
// The deadline is shorter than the stub's own half-minute life, so a child that
// was never signalled fails here on the clock rather than on the ending it
// eventually reaches. Wait cannot be interrupted, so it is waited on from a
// goroutine that the test's own Cleanup unblocks.
func assertCancelled(t *testing.T, run *Run) {
	t.Helper()

	type ending struct {
		outcome *Outcome
		err     error
	}
	ended := make(chan ending, 1)
	go func() {
		outcome, err := run.Wait()
		ended <- ending{outcome, err}
	}()

	select {
	case e := <-ended:
		switch {
		case errors.Is(e.err, ErrCancelled):
			return
		case e.err != nil:
			t.Fatalf("the run ended with %v, want a cancelled ending", e.err)
		case e.outcome.Err == nil || e.outcome.Err.Kind != "cancelled":
			t.Fatalf("the run ended with outcome %+v, want error{cancelled} — the child finished "+
				"on its own, so no signal ever reached it", e.outcome)
		case e.outcome.ExitCode != 130:
			t.Errorf("the cancelled run exited %d, want 130", e.outcome.ExitCode)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run had not ended 10s after the cancel returned: the child was never signalled")
	}
}

// TestCancelInTheStartupWindowCancelsTheRunItWaitedFor is finding I1's first
// half pinned.
//
// The Run screen reveals a live Cancel button before it awaits StartRun, so the
// first click can land while the slot is claimed and the child does not exist
// yet. CancelRun read a.run alone: it saw nil and returned nil, and the page
// treats a nil return as "the cancel was carried out" — it leaves the button
// disabled with "Cancelling — waiting for sub-translator to clean up…" on
// screen and re-enables it only from its catch branch, which is not taken. The
// run then ran to completion with nothing left that could stop it.
func TestCancelInTheStartupWindowCancelsTheRunItWaitedFor(t *testing.T) {
	dir := t.TempDir()
	cli, log := writeLongLivedStub(t, dir)
	app, _ := testApp(t, cli)

	release := enterTheStartupWindow(t, app)

	returned := make(chan error, 1)
	go func() { returned <- app.CancelRun() }()

	select {
	case err := <-returned:
		t.Fatalf("CancelRun() returned %v inside the startup window — the page reads that as a "+
			"cancel that was carried out, and the run it did not cancel goes on with a dead button "+
			"over it", err)
	case <-time.After(200 * time.Millisecond):
	}

	run := startTheChild(t, cli, log)
	release(run)

	select {
	case err := <-returned:
		if err != nil {
			t.Errorf("CancelRun() = %v, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("CancelRun() never returned after the claim resolved")
	}

	assertCancelled(t, run)
}

// TestCloseInTheStartupWindowAsksAndTakesTheChildWithIt is finding I1's second
// half pinned.
//
// beforeClose read a.run alone too, so a Cmd-Q inside the same window asked
// nothing and returned false: the window went, and the child that was a
// syscall away from transcribing stayed — the background orphan spec:255
// forbids outright.
func TestCloseInTheStartupWindowAsksAndTakesTheChildWithIt(t *testing.T) {
	dir := t.TempDir()
	cli, log := writeLongLivedStub(t, dir)
	app, _ := testApp(t, cli)

	const quit = "Cancel the run and quit"
	asked := make(chan string, 1)
	app.ask = func(_ context.Context, opts wailsruntime.MessageDialogOptions) (string, error) {
		asked <- opts.Message
		return quit, nil
	}

	release := enterTheStartupWindow(t, app)

	prevented := make(chan bool, 1)
	go func() { prevented <- app.beforeClose(context.Background()) }()

	select {
	case msg := <-asked:
		if !strings.Contains(msg, "still running") {
			t.Errorf("the close question said %q; a starting run has reported no stage yet, so it "+
				"should say the run is still running rather than claim a percentage of nothing", msg)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("beforeClose asked nothing inside the startup window: the window quits on a run " +
			"that is about to exist")
	}

	select {
	case got := <-prevented:
		t.Fatalf("beforeClose() = %v inside the startup window, before the child existed — the "+
			"window goes and the child stays", got)
	case <-time.After(200 * time.Millisecond):
	}

	run := startTheChild(t, cli, log)
	release(run)

	select {
	case got := <-prevented:
		if got {
			t.Error("beforeClose() = true after the user chose to quit, want false")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("beforeClose never returned after the claim resolved")
	}

	assertCancelled(t, run)
}

// TestCloseWithTheSlotFreeAsksNothing: teaching beforeClose about the claim
// must not make every quit put a question on screen.
func TestCloseWithTheSlotFreeAsksNothing(t *testing.T) {
	app, _ := testApp(t, "")
	app.ask = func(_ context.Context, _ wailsruntime.MessageDialogOptions) (string, error) {
		t.Error("beforeClose asked a question with no run in flight and no claim on the slot")
		return "Keep working", nil
	}

	if app.beforeClose(context.Background()) {
		t.Error("beforeClose() = true with nothing running, want false — the window cannot be closed")
	}
}

// TestKeepWorkingInTheStartupWindowPreventsTheClose: the other answer to the
// same question, so a claim on the slot cannot become a quit the user refused.
func TestKeepWorkingInTheStartupWindowPreventsTheClose(t *testing.T) {
	dir := t.TempDir()
	cli, log := writeLongLivedStub(t, dir)
	app, _ := testApp(t, cli)

	app.ask = func(_ context.Context, _ wailsruntime.MessageDialogOptions) (string, error) {
		return "Keep working", nil
	}

	release := enterTheStartupWindow(t, app)
	run := startTheChild(t, cli, log)
	release(run)

	if !app.beforeClose(context.Background()) {
		t.Error("beforeClose() = false after the user chose to keep working, want true")
	}
	if err := app.CancelRun(); err != nil {
		t.Errorf("CancelRun() = %v, want nil", err)
	}
}
