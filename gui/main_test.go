package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	if starting {
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
	if starting || run != nil {
		t.Fatalf("after a failed start: starting=%v run=%v, want both clear", starting, run != nil)
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
