package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

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

// freshEnding gives one test its own answer to "has this run already written its
// terminal event".
//
// That answer is per-process, which is exactly right for a CLI that performs one
// run and exits, and exactly wrong for a test binary that drives a dozen runs in
// one process: without a reset the second run's emitDone finds the first run's
// ending already claimed, concludes a cancel got there first, and hands the
// process over to a signal handler that does not exist — surfacing as
// "unexpected call to os.Exit(0) during test". Every in-process run helper calls
// this, so a new one inherits it rather than having to remember it.
func freshEnding(t *testing.T) {
	t.Helper()
	reset := func() {
		ending.mu.Lock()
		defer ending.mu.Unlock()
		ending.written, ending.code = false, 0
	}
	reset()
	t.Cleanup(reset)
}

func TestAFatalErrorKeepsItsMessageExactly(t *testing.T) {
	err := kindErrf(errRateLimited, "translate: %v", errors.New("429"))
	if got, want := err.Error(), "translate: 429"; got != want {
		t.Errorf("Error() = %q, want %q — the message is part of the contract", got, want)
	}
}

// "Exactly one terminal event per run" is a property of claimEnding, and only of
// claimEnding: it is the one place a done or an error reaches the stream, and the
// one place that can see whether something got there first. A site that builds
// one and hands it straight to ev.Emit opts out of the whole mechanism, and the
// suite cannot notice unless a cancel happens to land in its window.
//
// That is not hypothetical. Two such sites — the usage error and the missing
// input path — sat harmlessly below the point where the signal handler used to be
// armed; moving the arming to the top of the run, so that a cancel during the
// config load or -probe's ffprobe stopped being death by signal, put both of them
// inside the armed window and re-opened the defect claimEnding had just closed,
// reproducibly, on the missing-input path with 56 lines of usage text written
// between the emit and the exit.
//
// So the rule is structural, and checked as one: no non-test file in this package
// may pass an events.Error or an events.Done to Emit. Build it, name it, and give
// it to claimEnding — through exitWith, emitDone or exitAfterEmitting — and the
// exit status comes back with it.
func TestNoTerminalEventIsEmittedOutsideClaimEnding(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	// Emit takes an events.Event, so claimEnding's own call reads ev.Emit(e) and
	// cannot match these: what is being forbidden is handing Emit a freshly built
	// terminal event, which is the only way to bypass the claim.
	forbidden := []string{"Emit(&events.Error{", "Emit(&events.Done{"}

	scanned := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for i, line := range strings.Split(string(body), "\n") {
			lineNo := i + 1
			for _, bad := range forbidden {
				if strings.Contains(line, bad) {
					t.Errorf("%s:%d emits a terminal event without claiming the ending:\n\t%s\n"+
						"Route it through claimEnding — exitWith, emitDone or exitAfterEmitting — "+
						"or a cancel landing between this emit and the exit puts a second "+
						"terminal event on a stream that promises one.",
						name, lineNo, strings.TrimSpace(line))
				}
			}
		}
	}
	// A scan that found no files would pass silently, which is the failure mode
	// this whole test exists to rule out elsewhere.
	if scanned < 8 {
		t.Fatalf("scanned %d non-test .go files in package main, want the whole package", scanned)
	}
}
