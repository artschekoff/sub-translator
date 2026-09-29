package main

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

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
//
// This is the only accessor: fatalError deliberately has no Kind() method, so
// there is no way to ask a *fatalError for its kind that stops at the outermost
// error and quietly answers "other" for a wrapped one.
func kindOf(err error) errKind {
	var fe *fatalError
	if errors.As(err, &fe) {
		return fe.kind
	}
	return errOther
}

// ending holds the one fact every ending of a run has to agree on: whether the
// single terminal event is already on the stream, and what exit status that
// event earned. Three endings meet here from two goroutines — exitWith and
// emitDone on the main one, the signal handler on its own — and all three ask
// the same question through claimEnding.
//
// It is a mutex rather than a second atomic flag because the question and the
// answer have to be one step. Whoever checks a flag and then emits leaves the
// other goroutine a window to emit in, and that window is what put a done line
// and an error{cancelled} line on the same stream: the check that existed was
// one-directional (the main goroutine deferred to the handler, never the
// reverse), and even the direction it covered was only as wide as the few
// instructions between the load and the emit.
//
// Nothing long is ever held: one json.Marshal and one write. The kill and the
// RemoveAll a cancel also performs happen after the lock is released, so a
// handler can never park a run's ending behind an ffmpeg pass.
var ending struct {
	mu      sync.Mutex
	written bool
	code    int
}

// claimEnding writes the one terminal event a run owes its consumer and returns
// the exit status that event earns. When another ending got there first it
// writes nothing and returns that one's status with won false.
//
// That is what keeps the stream self-describing: the exit code is always the one
// the single event on the wire pairs with, whichever ending won. A cancel that
// arrives after a successful done therefore exits 0 — the work did complete, the
// file named by done is on disk, and there was nothing left to cancel — and a
// cancel that arrives after a failure exits 1. Only a cancel that is genuinely
// the run's ending exits 130.
func claimEnding(e events.Event, code int) (earned int, won bool) {
	ending.mu.Lock()
	defer ending.mu.Unlock()
	if ending.written {
		return ending.code, false
	}
	ev.Emit(e)
	ending.written, ending.code = true, code
	return code, true
}

// exitWith is the one place a returned error becomes an exit: it emits the
// single error event a fatal run owes its consumer, prints the human-readable
// line, removes the scratch directory and exits 1. Everything fatal goes
// through here — fatalf and fatalKindf are thin wrappers that build the error
// and hand it over, and runOrdinary's 24 returned errors arrive through main —
// so "exactly one error event per fatal run" is a property of one function
// rather than of every call site that can fail. The one deliberate exception is
// the missing-input path in main, which prints the usage text instead of an
// error: line and so emits and exits on its own; its comment says why.
//
// The message is printed with a literal %s rather than the caller's own format
// string, so a message that happens to contain a percent sign (an ffmpeg
// progress line, a model name) cannot be re-interpreted as a verb.
func exitWith(err error) {
	yieldIfCancelling()
	code, won := claimEnding(&events.Error{Kind: string(kindOf(err)), Message: err.Error()}, 1)
	if !won {
		// A cancel landed in the few instructions between yieldIfCancelling's
		// read and the claim. The cancelled event is this run's terminal event
		// now and this failure is the killed child surfacing, so the ending goes
		// to the handler exactly as it would have an instant earlier.
		handOverToCancelHandler(code)
	}
	fmt.Fprintf(proseErr, "error: %s\n", err.Error())
	// os.Exit skips deferred calls, so the scratch directory has to be removed
	// here or every error path leaks it.
	cleanupRunTmpDir()
	os.Exit(code)
}

// exitAfterEmitting is the ending for the two fatal paths that cannot go through
// exitWith, because the prose they owe a person is not exitWith's "error: …"
// line: a usage error, whose message the flag package has already printed, and a
// missing input path, which needs the usage text instead of a one-line error.
//
// They owe their consumer exactly one terminal event and an exit status that
// agrees with it all the same, so they claim the ending the way every other
// ending does. Both used to emit with a bare ev.Emit, which was safe only for as
// long as the signal handler was armed below them. Moving the arming up to the
// top of the run — so that a cancel during the config load or -probe's ffprobe
// stops being death by signal — put both of them inside the armed window, and a
// cancel landing between the emit and the exit then appended a second terminal
// event. That window is not narrow on the missing-input path: 56 lines of usage
// text are written inside it.
//
// prose is what the path prints after its event and may be nil; it is called only
// when this ending won, because a cancel that got there first has already ended
// the run and 56 lines of flag documentation are not that run's epilogue.
func exitAfterEmitting(e *events.Error, code int, prose func()) {
	earned, won := claimEnding(e, code)
	if !won {
		handOverToCancelHandler(earned)
	}
	if prose != nil {
		prose()
	}
	os.Exit(code)
}

// yieldIfCancelling hands the ending to the signal handler when a cancel is
// already in flight, and returns immediately when there is none.
//
// Whatever reached it is not a run of its own any more. Either it is the child
// the handler has just killed, surfacing as "signal: killed", or it is a run
// that happened to finish in the same instant; either way the cancelled event is
// already on the stream — the handler raises the flag only after emitting it,
// and only when its own emit was the one that counted — so a second terminal
// event here would break the one-terminal-event promise, and exiting 1 or 0
// would tell a wrapper the run failed or succeeded rather than that the user
// cancelled it.
//
// So it waits for the handler rather than racing it. It is the cheap check, not
// the load-bearing one: claimEnding is what actually guarantees a single
// terminal event, in both directions. This exists so the common case — a cancel
// that arrived first and has already been reported — hands over cleanly instead
// of surfacing the killed child as a failure of its own.
func yieldIfCancelling() {
	if !cancelling.Load() {
		return
	}
	// cancelling is raised only by a handler whose own claim succeeded, so the
	// terminal event on the stream is the cancelled one and 130 is its status.
	handOverToCancelHandler(130)
}

// handOverToCancelHandler waits for the signal handler to finish the cancel and
// then ends the run the way the handler would have, in the same order and with
// the exit status the terminal event already on the stream earned.
//
// Waiting keeps the handler the only place the scratch directory is removed, so
// it cannot go while the children are still being killed. The wait is bounded,
// because a handler wedged in os.RemoveAll must not hang the process forever: if
// it ever elapses, this ends the run the way the handler would have.
func handOverToCancelHandler(code int) {
	time.Sleep(cancelHandoffTimeout)
	shutdownAfterCancel()
	os.Exit(code)
}

// emitDone is the success twin of exitWith: the one place a run's terminal done
// event is written, so the check that has to come before it is written once
// instead of at all four success returns. Without it, a cancel arriving in a
// run's last milliseconds put a cancelled event and a done event on the same
// stream and exited 0 — two terminal events, and the one a Cancel button
// believes is whichever it read last.
func emitDone(d *events.Done) {
	yieldIfCancelling()
	if code, won := claimEnding(d, 0); !won {
		// The mirror of exitWith's case: a cancel claimed the ending between the
		// read above and the claim. Returning would let the pipeline carry on
		// and main exit 0 after an ending that was not its own.
		handOverToCancelHandler(code)
	}
}
