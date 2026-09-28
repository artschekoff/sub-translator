package main

import (
	"errors"
	"fmt"
	"os"
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
	ev.Emit(&events.Error{Kind: string(kindOf(err)), Message: err.Error()})
	fmt.Fprintf(proseErr, "error: %s\n", err.Error())
	// os.Exit skips deferred calls, so the scratch directory has to be removed
	// here or every error path leaks it.
	cleanupRunTmpDir()
	os.Exit(1)
}

// yieldIfCancelling hands the ending to the signal handler when a cancel is
// already in flight, and returns immediately when there is none.
//
// Whatever reached it is not a run of its own any more. Either it is the child
// the handler has just killed, surfacing as "signal: killed", or it is a run
// that happened to finish in the same instant; either way the cancelled event is
// already on the stream — the handler raises the flag only after emitting it —
// so a second terminal event here would break the one-terminal-event promise,
// and exiting 1 or 0 would tell a wrapper the run failed or succeeded rather
// than that the user cancelled it.
//
// So it waits for the handler rather than racing it. That also keeps the handler
// the only place the scratch directory is removed, so it cannot go while the
// children are still being killed. The wait is bounded, because a handler wedged
// in os.RemoveAll must not hang the process forever: if it ever elapses, this
// ends the run the way the handler would have, in the same order.
func yieldIfCancelling() {
	if !cancelling.Load() {
		return
	}
	time.Sleep(cancelHandoffTimeout)
	shutdownAfterCancel()
	os.Exit(130)
}

// emitDone is the success twin of exitWith: the one place a run's terminal done
// event is written, so the check that has to come before it is written once
// instead of at all four success returns. Without it, a cancel arriving in a
// run's last milliseconds put a cancelled event and a done event on the same
// stream and exited 0 — two terminal events, and the one a Cancel button
// believes is whichever it read last.
func emitDone(d *events.Done) {
	yieldIfCancelling()
	ev.Emit(d)
}
