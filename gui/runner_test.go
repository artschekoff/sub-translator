package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	if errors.Is(err, ErrProbeCancelled) {
		t.Fatalf("Probe() error = %v, want it NOT to be ErrProbeCancelled", err)
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
// probe failure (a *ProbeFailure), only as ErrProbeCancelled.
func TestProbeSurvivesACancelledProbe(t *testing.T) {
	dir := t.TempDir()
	errLine := `{"type":"error","t":"2026-09-30T00:00:01Z","kind":"cancelled","message":"cancelled"}` + "\n"
	cli := writeStubCLI(t, dir, "", versionLine+errLine, 130)

	got, err := ProbeFile(context.Background(), cli, "movie.mkv")
	if got != nil {
		t.Errorf("Probe() = %+v, want nil *Probe on cancel", got)
	}
	if !errors.Is(err, ErrProbeCancelled) {
		t.Fatalf("Probe() error = %v, want errors.Is(err, ErrProbeCancelled)", err)
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
	if !errors.Is(err, ErrProbeCancelled) {
		t.Fatalf("Probe() error = %v, want errors.Is(err, ErrProbeCancelled)", err)
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
