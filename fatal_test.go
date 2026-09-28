package main

import (
	"errors"
	"fmt"
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
