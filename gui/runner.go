// runner.go runs the CLI's -probe subcommand and turns its event stream
// into the track list (or the reason it couldn't get one) that the window
// needs before it can show anything.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
)

// ErrProbeCancelled is returned by Probe when the probe ended because it was
// cancelled, not because the file was unreadable. Two distinct CLI endings
// both mean this, per the README:
//
//   - an error event with kind "cancelled" (exit 130) — a cancel that
//     reached the CLI's signal handler once it was armed;
//   - a signalled exit with an empty stream — a cancel landing in the few
//     milliseconds at startup before the handler is armed, documented as
//     "cancelled before the tool could answer" rather than a crash.
//
// The window must route both away from ProbeFailure: cancelling is not the
// file being unreadable, and it is not a crash.
var ErrProbeCancelled = errors.New("probe cancelled")

// ProbeFailure is returned by Probe when the CLI ran and reported, through
// its own error event, that the probe itself failed — most commonly kind
// "probe_failed" for a file ffprobe cannot read. It carries the CLI's own
// kind and message verbatim so the window can show the CLI's own reason
// rather than inventing "no tracks".
type ProbeFailure struct {
	Kind    string
	Message string
}

func (e *ProbeFailure) Error() string {
	return e.Message
}

// Probe runs `cli -probe -events json path`, decodes its stderr event
// stream, and returns the track list the CLI reports.
//
// Stdout carries the CLI's human-readable track listing (the same listing
// -probe prints without -events json), which nothing here reads; it is
// discarded. This needs no manual draining: os/exec already runs its own
// copy goroutine for any non-*os.File Writer assigned to Stdout (and even
// for a nil Stdout, which it connects to /dev/null), and Wait blocks until
// that copy is done, so the child can never be left blocked on a full pipe
// here.
func ProbeFile(ctx context.Context, cli, path string) (*Probe, error) {
	cmd := exec.CommandContext(ctx, cli, "-probe", "-events", "json", path)
	cmd.Stdout = io.Discard

	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	runErr := cmd.Run()

	var probe *Probe
	var probeErr *Error
	seen := 0

	decodeErr := Decode(bytes.NewReader(stderrBuf.Bytes()), func(ev Event) error {
		seen++
		switch ev.Type {
		case "probe":
			p, err := ev.AsProbe()
			if err != nil {
				return err
			}
			probe = p
		case "error":
			e, err := ev.AsError()
			if err != nil {
				return err
			}
			probeErr = e
		}
		return nil
	})
	if decodeErr != nil {
		return nil, fmt.Errorf("probe: decoding %s's event stream: %w", cli, decodeErr)
	}

	if probeErr != nil {
		if probeErr.Kind == "cancelled" {
			return nil, ErrProbeCancelled
		}
		return nil, &ProbeFailure{Kind: probeErr.Kind, Message: probeErr.Message}
	}

	// A signalled exit (the child killed by an uncaught SIGINT/SIGTERM, not
	// an os.Exit call) with nothing at all on the stream — not even
	// version — is the startup race documented in the README: a cancel
	// landing before the CLI's own handler is armed.
	if seen == 0 && signalled(runErr) {
		return nil, ErrProbeCancelled
	}

	if runErr != nil {
		return nil, fmt.Errorf("probe: running %s: %w (stderr: %s)", cli, runErr, bytes.TrimSpace(stderrBuf.Bytes()))
	}

	if probe == nil {
		return nil, fmt.Errorf("probe: %s exited 0 without reporting a probe event", cli)
	}

	return probe, nil
}

// signalled reports whether err is the *exec.ExitError of a process that
// was terminated by a signal rather than by exiting with a code. Go's
// os/exec reports that case with ExitCode() == -1 (see os.ProcessState).
func signalled(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	return exitErr.ExitCode() == -1
}
