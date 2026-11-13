// runner.go runs the CLI. ProbeFile runs its -probe subcommand and turns
// its event stream into the track list (or the reason it couldn't get one)
// that the window needs before it can show anything. Start runs an actual
// translation, streaming its event stream live to the window for the
// minutes the job takes, and Run's Cancel ends it early.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
)

// ErrCancelled is returned by ProbeFile, and by Start/Wait, when the run
// ended because it was cancelled rather than because the CLI failed.
//
// It was called ErrProbeCancelled until the Run screen was built, and the
// name named the wrong operation from the moment Start began returning it:
// what a user cancels is a twenty-minute translation far more often than a
// probe. The Run screen's Cancel button is the first consumer to write
// errors.Is on it while handling a cancelled *translation*, which is the
// moment the old name stopped being merely imprecise and started reading as
// a check on something else entirely.
//
// Two distinct CLI endings both mean this, per the README, and both
// subcommands can hit either one:
//
//   - an error event with kind "cancelled" (exit 130) — a cancel that
//     reached the CLI's signal handler once it was armed; Start's own
//     buildOutcome checks for this one directly, since it also has to
//     validate the exit code against it.
//   - a signalled exit with an empty stream — a cancel landing in the few
//     milliseconds at startup before the handler is armed, documented as
//     "cancelled before the tool could answer" rather than a crash. This is
//     the one narrow case ProbeFile and buildOutcome share verbatim (see
//     signalled below): neither treats "nothing on the stream at all" as
//     the CLI having broken its own protocol.
//
// The window must route both away from ProbeFailure or a "broken stream"
// error: cancelling is not the file being unreadable, not a crash, and not
// a contract violation.
var ErrCancelled = errors.New("cancelled")

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
			return nil, ErrCancelled
		}
		return nil, &ProbeFailure{Kind: probeErr.Kind, Message: probeErr.Message}
	}

	// A signalled exit (the child killed by an uncaught SIGINT/SIGTERM, not
	// an os.Exit call) with nothing at all on the stream — not even
	// version — is the startup race documented in the README: a cancel
	// landing before the CLI's own handler is armed.
	if seen == 0 && signalled(runErr) {
		return nil, ErrCancelled
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

// RunOptions configures a translation run. It is turned into the CLI's argv
// by buildRunArgs; -events json is always added on top of what's here so
// the run has a stream to decode in the first place.
//
// Track and Atrack are two different flags on two different streams, and
// that distinction is why Atrack exists at all. The CLI's -track is the
// *subtitle* stream index (`flag.Int("track", -1, "subtitle stream index")`)
// and -atrack is the audio one. Until Atrack was added, the window sent the
// audio index the user picked from the track list as -track — a flag the
// audio path never reads — so the CLI fell back to pickAudioStream and
// transcribed whatever it chose. Picking "#3 eng" and getting the Russian
// dub is a defect with no error message attached to it.
//
// Neither field's zero value means "unset": both of the CLI's own defaults
// are -1 (auto), so RunOptions follows the same convention — a caller that
// wants auto-selection sets -1, exactly as it would type -track -1 on the
// CLI's own command line. Use trackIndices to turn the one track the File
// screen chose into the pair. Mode, Out and Source are only added to the
// argv when non-empty, so leaving them blank keeps the CLI's own defaults
// ("srt", unset, "auto") rather than sending it an empty string it would
// have to reject.
type RunOptions struct {
	Input, To, From, Mode, Out string
	Track                      int
	Atrack                     int
	Source                     string
	Fast                       bool
}

// trackIndices maps the single track the File screen chose onto the two
// flags the CLI has for it: -track for a subtitle stream, -atrack for an
// audio one. The other flag is left at the CLI's own auto sentinel, because
// the run does not use it — an audio-source run never reads -track, and a
// subtitle-source run never reads -atrack.
//
// Source "" or "auto" leaves both on auto: there is no chosen stream to
// place, and guessing which list an index came from is exactly the mistake
// this function exists to stop being possible.
func trackIndices(source string, chosen int) (track, atrack int) {
	switch source {
	case "audio":
		return -1, chosen
	case "sub":
		return chosen, -1
	}
	return -1, -1
}

// Outcome summarizes how a completed run ended. The CLI guarantees exactly
// one of Done or Err is set, never both and never neither, and that
// ExitCode agrees with which one it is (0 for Done, 1 or 2 for Err, 130 for
// an Err with Kind "cancelled") — Start refuses a stream that breaks either
// half of that guarantee rather than picking a winner (see buildOutcome).
//
// Saved collects every "saved" event the run emitted, in order: -mode both
// emits one per file (a sidecar and a container), and the window needs both
// to offer "Show in Finder" for each.
type Outcome struct {
	Done     *Done
	Err      *Error
	ExitCode int
	Saved    []Saved
	Warning  *Warning
}

// Run is a translation subprocess in flight, returned by Start.
//
// Start cannot itself be named Run: Go forbids a package-level function and
// type from sharing one identifier, and a type here needs the name so that
// Cancel — which must be callable *while* the run is still in progress,
// minutes before it ends — has something live to call it on. Wait blocks
// until the run ends and hands back the same (*Outcome, error) a merged,
// blocking "func Run(...) (*Outcome, error)" would have returned directly;
// Start plus Wait is that same contract split across two calls so Cancel
// has a window to act in between.
type Run struct {
	cmd  *exec.Cmd
	done chan struct{}
	// outcome and err are written exactly once, by stream's goroutine,
	// strictly before done is closed. Every reader (Wait, Cancel) only
	// reads them after receiving from done, so the channel close is the
	// only synchronization these two fields need.
	outcome *Outcome
	err     error
}

// buildRunArgs turns o into the CLI's argv, always requesting the JSON
// event stream this package decodes.
func buildRunArgs(o RunOptions) []string {
	args := []string{"-events", "json"}
	if o.From != "" {
		args = append(args, "-from", o.From)
	}
	if o.To != "" {
		args = append(args, "-to", o.To)
	}
	if o.Mode != "" {
		args = append(args, "-mode", o.Mode)
	}
	if o.Out != "" {
		args = append(args, "-out", o.Out)
	}
	if o.Source != "" {
		args = append(args, "-source", o.Source)
	}
	// Both are always sent, both always explicit. -1 is the CLI's own default
	// for each, so sending it changes nothing, and sending both unconditionally
	// means there is no argv shape where one of the two silently goes missing
	// — which is precisely how -atrack came to be absent in the first place.
	args = append(args, "-track", strconv.Itoa(o.Track))
	args = append(args, "-atrack", strconv.Itoa(o.Atrack))
	if o.Fast {
		args = append(args, "-fast")
	}
	args = append(args, o.Input)
	return args
}

// Start runs `cli <argv built from o>` and returns immediately once the
// process has started, streaming its stderr event stream to on from a
// background goroutine as lines arrive — not after the process exits. A
// translation runs for minutes and emits stage and progress events the
// whole time; buffering the stream the way ProbeFile does for a short probe
// would leave the window blank for the whole job and then dump everything
// at once.
//
// Stdout is discarded exactly as ProbeFile discards it: os/exec runs its
// own copy goroutine for a non-*os.File Stdout, so the child can never
// block on a full pipe here even though nothing reads it.
func Start(ctx context.Context, cli string, o RunOptions, on func(Event) error) (*Run, error) {
	cmd := exec.CommandContext(ctx, cli, buildRunArgs(o)...)
	cmd.Stdout = io.Discard
	// Only ever interrupt, never kill outright, even along the ctx-cancel
	// path: the CLI kills its own process group once its handler sees the
	// signal, and killing it ourselves here would race that handler. The
	// primary way a caller ends a run early is Run.Cancel below, which
	// signals the same way.
	cmd.Cancel = func() error {
		return cmd.Process.Signal(os.Interrupt)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("run: creating %s's stderr pipe: %w", cli, err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("run: starting %s: %w", cli, err)
	}

	r := &Run{cmd: cmd, done: make(chan struct{})}
	go r.stream(stderr, on)
	return r, nil
}

// stream decodes stderr while the child runs, forwarding every event to on
// as it arrives, then waits for the process to exit and assembles the
// Outcome (or the error explaining why the stream didn't earn one). It runs
// on its own goroutine, started by Start; Wait and Cancel both just block
// on r.done.
func (r *Run) stream(stderr io.Reader, on func(Event) error) {
	var (
		doneEv *Done
		errEv  *Error
		saved  []Saved
		warn   *Warning
		seen   int
	)

	decodeErr := Decode(stderr, func(ev Event) error {
		seen++
		switch ev.Type {
		case "done":
			d, err := ev.AsDone()
			if err != nil {
				return err
			}
			doneEv = d
		case "error":
			e, err := ev.AsError()
			if err != nil {
				return err
			}
			errEv = e
		case "saved":
			s, err := ev.AsSaved()
			if err != nil {
				return err
			}
			saved = append(saved, *s)
		case "warning":
			w, err := ev.AsWarning()
			if err != nil {
				return err
			}
			warn = w
		}
		if on == nil {
			return nil
		}
		return on(ev)
	})

	// Always wait, whatever decode did: the process must be reaped whether
	// the stream ended cleanly, failed to parse, or on returned an error
	// and cut the decode short. Leaving this unreached is exactly the
	// defect TestCancelDoesNotOrphanTheChild exists to catch.
	waitErr := r.cmd.Wait()

	r.outcome, r.err = buildOutcome(r.cmd.Path, decodeErr, waitErr, r.cmd.ProcessState, seen, doneEv, errEv, saved, warn)
	close(r.done)
}

// buildOutcome applies the CLI's guarantees to what stream collected:
// exactly one of doneEv/errEv, and an exit code that agrees with which one
// it is. It refuses — returns a nil *Outcome and a descriptive error —
// rather than picking a winner when the stream breaks either half of that
// contract, per the task brief.
func buildOutcome(cliPath string, decodeErr, waitErr error, state *os.ProcessState, seen int, doneEv *Done, errEv *Error, saved []Saved, warn *Warning) (*Outcome, error) {
	if decodeErr != nil {
		return nil, fmt.Errorf("run: decoding %s's event stream: %w", cliPath, decodeErr)
	}

	// Mirrors ProbeFile's own check (see signalled below): a cancel landing
	// in the few milliseconds before the CLI arms its signal handler kills
	// it with nothing on the stream at all — not even version. That is the
	// GUI's Cancel button clicked the instant a run starts, not the stream
	// breaking its own protocol, so it must route to ErrCancelled
	// exactly as ProbeFile's identical case does, before the generic
	// exit-code bookkeeping below ever sees the signalled exit's -1.
	if doneEv == nil && errEv == nil && seen == 0 && signalled(waitErr) {
		return nil, ErrCancelled
	}

	var exitCode int
	if waitErr == nil {
		exitCode = 0
	} else {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			return nil, fmt.Errorf("run: waiting for %s: %w", cliPath, waitErr)
		}
		exitCode = state.ExitCode()
	}

	switch {
	case doneEv != nil && errEv != nil:
		return nil, fmt.Errorf("run: %s emitted both a done event and an error event (kind=%q): "+
			"the stream broke its one-terminal-event contract", cliPath, errEv.Kind)
	case doneEv == nil && errEv == nil:
		return nil, fmt.Errorf("run: %s exited %d without a done or an error event: "+
			"the stream broke its one-terminal-event contract", cliPath, exitCode)
	}

	switch {
	case doneEv != nil:
		if exitCode != 0 {
			return nil, fmt.Errorf("run: %s reported done but exited %d, not 0: "+
				"the stream broke its exit-code contract", cliPath, exitCode)
		}
	case errEv.Kind == "cancelled":
		if exitCode != 130 {
			return nil, fmt.Errorf("run: %s reported a cancelled error but exited %d, not 130: "+
				"the stream broke its exit-code contract", cliPath, exitCode)
		}
	default:
		if exitCode != 1 && exitCode != 2 {
			return nil, fmt.Errorf("run: %s reported error (kind=%q) but exited %d, not 1 or 2: "+
				"the stream broke its exit-code contract", cliPath, errEv.Kind, exitCode)
		}
	}

	return &Outcome{
		Done:     doneEv,
		Err:      errEv,
		ExitCode: exitCode,
		Saved:    saved,
		Warning:  warn,
	}, nil
}

// Wait blocks until the run ends and returns the Outcome the CLI reported,
// or the error explaining why it couldn't (a stream that broke the CLI's
// own guarantees, or a process that couldn't be waited on at all). It is
// safe to call more than once, and safe to call after Cancel.
func (r *Run) Wait() (*Outcome, error) {
	<-r.done
	return r.outcome, r.err
}

// Cancel ends the run early: it sends SIGINT and then waits for the process
// to exit, exactly as the task brief specifies. It never kills the
// process group itself — the CLI does that once its own handler sees the
// signal, and killing it here would race that handler, possibly cutting
// off the partial file the CLI is in the middle of finalizing.
//
// The wait half is not optional. The CLI's own cleanup (killing the group,
// closing the partial file) happens between the signal landing and the
// process actually exiting, so returning as soon as the signal is sent
// would hand control back to the caller — free to quit the app — while
// that cleanup is still in flight, orphaning whatever the CLI's own
// children were doing.
//
// # The error contract
//
// A non-nil return means the cancel itself could not be carried out, or the
// run's stream could not be turned into an outcome. It never means "the run
// ended because you cancelled it" — which is what the two cases below used
// to make it mean, and the Cancel button is the consumer that could not live
// with either.
//
//   - A cancel landing in the startup race, before the CLI arms its handler,
//     makes Wait return ErrCancelled. That is the cancel working exactly as
//     asked, so Cancel reports nil and leaves the sentinel for Wait, whose
//     job is to say how the run ended. Surfacing it here put an error dialog
//     on screen for a successful cancel.
//   - Cancel called on a run that already finished naturally — the user
//     pressing the button in the moment between the last event and the
//     screen redrawing — used to return "os: process already finished",
//     which is os.ErrProcessDone from signalling a reaped process. That is
//     not a failure of anything: there was simply nothing left to cancel, and
//     reporting it hid the run's real ending behind a signalling complaint.
//     Cancel now waits and reports the run's own outcome error instead, which
//     for a successful run is nil.
//
// So: nil covers every ending the run can reach, cancelled or not. What is
// left over — a signal that could not be delivered for any other reason, a
// stream that broke the CLI's own guarantees, a process that could not be
// waited on — is what a caller should actually show, and Wait returns the
// same thing for a caller that only wants the ending.
func (r *Run) Cancel() error {
	if err := r.cmd.Process.Signal(os.Interrupt); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("run: signalling %s: %w", r.cmd.Path, err)
	}
	<-r.done
	if errors.Is(r.err, ErrCancelled) {
		return nil
	}
	return r.err
}
