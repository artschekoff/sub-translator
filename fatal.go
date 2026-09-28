package main

import (
	"errors"
	"fmt"
	"os"

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

// exitWith is the one place a returned error becomes an exit: it emits the
// single error event a fatal run owes its consumer, prints the human-readable
// line, removes the scratch directory and exits 1. Everything fatal goes
// through here — fatalf and fatalKindf are now thin wrappers that build the
// error and hand it over — so "exactly one error event per fatal run" is a
// property of one function rather than of 34 call sites.
//
// The message is printed with a literal %s rather than the caller's own format
// string, so a message that happens to contain a percent sign (an ffmpeg
// progress line, a model name) cannot be re-interpreted as a verb.
func exitWith(err error) {
	ev.Emit(&events.Error{Kind: string(kindOf(err)), Message: err.Error()})
	fmt.Fprintf(proseErr, "error: %s\n", err.Error())
	// os.Exit skips deferred calls, so the scratch directory has to be removed
	// here or every error path leaks it.
	cleanupRunTmpDir()
	os.Exit(1)
}
