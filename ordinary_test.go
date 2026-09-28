package main

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/artschekoff/sub-translator/internal/events"
	"github.com/artschekoff/sub-translator/internal/media"
	"github.com/artschekoff/sub-translator/internal/translate"
)

// audioOnlyStubs plants an ffprobe that answers both calls the default path
// makes — -show_streams for the track list, -show_format for probeEvent's
// duration — with a file that has a video track and nothing else. A run told to
// transcribe it has no audio track to work with, which is the classified
// failure the test below is after.
//
// The whisper "binary" and "model" are empty files: resolveWhisper only stats
// them, and the run never gets as far as executing anything, because
// pickAudioStream refuses first.
func audioOnlyStubs(t *testing.T) (dir, whisperBin, whisperModel string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stubs are POSIX shell scripts")
	}
	dir = t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, filepath.Join(binDir, "ffprobe"), `#!/bin/sh
case "$*" in
  *-show_format*) cat <<JSON
{"format":{"duration":"120.5"}}
JSON
  ;;
  *) cat <<JSON
{"streams":[{"index":0,"codec_type":"video","codec_name":"h264"}]}
JSON
  ;;
esac
exit 0
`)
	whisperBin = filepath.Join(binDir, "stub-whisper")
	writeStub(t, whisperBin, "#!/bin/sh\nexit 0\n")
	whisperModel = filepath.Join(dir, "ggml-test.bin")
	if err := os.WriteFile(whisperModel, []byte("not a model"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	// No config file, so resolveWhisper is settled entirely by the options
	// passed in and this machine's own settings cannot change the answer.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "cfg"))
	return dir, whisperBin, whisperModel
}

// TestRunOrdinaryReturnsAClassifiedError is what the extraction bought: the
// default path's failures can now be asserted without spawning a process.
// Before it, every one of these 24 sites ended in os.Exit, so the only way to
// see which kind a failure carried was to read it back off a subprocess's
// stderr.
//
// The message is asserted as well as the kind, and deliberately in full:
// classifyTranscribeErr recognises this failure by the prefix "no audio tracks
// in", so a reworded message would silently demote it to "other" — the exact
// regression the kind exists to prevent, and one no assertion on the kind alone
// would catch, because the kind would still be whatever the classifier said.
func TestRunOrdinaryReturnsAClassifiedError(t *testing.T) {
	dir, whisperBin, whisperModel := audioOnlyStubs(t)

	input := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(input, []byte("not really a film"), 0o644); err != nil {
		t.Fatal(err)
	}
	tmpDir := filepath.Join(dir, "scratch")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		t.Fatal(err)
	}

	saved := ev
	t.Cleanup(func() { ev = saved })
	ev = events.New(&bytes.Buffer{})

	var err error
	out := captureStdout(t, func() {
		err = runOrdinary(ordinaryOptions{
			input:        input,
			tmpDir:       tmpDir,
			from:         "en",
			to:           "es",
			source:       sourceAudio,
			track:        -1,
			atrack:       -1,
			mode:         modeSRT,
			whisperBin:   whisperBin,
			whisperModel: whisperModel,
			translateOpts: translate.Options{
				APIKey:  "sk-test",
				Model:   translate.DefaultModel,
				Workers: 4,
			},
		})
	})

	if err == nil {
		t.Fatalf("runOrdinary() = nil, want an error — the file has no audio track\n%s", out)
	}
	if got, want := kindOf(err), errNoAudioTrack; got != want {
		t.Errorf("kindOf(err) = %q, want %q — a caller routes on this\nerror: %v", got, want, err)
	}
	if got, want := err.Error(), "no audio tracks in movie.mkv — nothing to transcribe"; got != want {
		t.Errorf("err.Error() = %q, want %q — the message classifyTranscribeErr matches on", got, want)
	}
}

// TestRunOrdinaryEndsInOneDoneEvent asserts in-process what Task 2 asserts
// through a process, so a later change that breaks it fails fast and locally:
// a successful default run ends in exactly one done event, naming the file it
// wrote, and emits no error event at all.
func TestRunOrdinaryEndsInOneDoneEvent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stubs are POSIX shell scripts")
	}
	dir := t.TempDir()
	binDir := subtitleSourceStubs(t, dir, subtitleTrackSRT)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	stubTranslator(t, nil)

	input := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(input, []byte("not really a film"), 0o644); err != nil {
		t.Fatal(err)
	}
	tmpDir := filepath.Join(dir, "scratch")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var stream bytes.Buffer
	saved := ev
	t.Cleanup(func() { ev = saved })
	ev = events.New(&stream)

	var err error
	out := captureStdout(t, func() {
		err = runOrdinary(ordinaryOptions{
			input:  input,
			tmpDir: tmpDir,
			from:   "en",
			to:     "es",
			source: sourceSub,
			track:  0,
			atrack: -1,
			mode:   modeSRT,
			translateOpts: translate.Options{
				APIKey:  "sk-test",
				Model:   translate.DefaultModel,
				Workers: 4,
			},
		})
	})
	if err != nil {
		t.Fatalf("runOrdinary() = %v, want success\nstdout:\n%s\nevents:\n%s", err, out, stream.String())
	}

	done := eventsOfKind(stream.String(), "done")
	if len(done) != 1 {
		t.Fatalf("%d done events, want exactly one:\n%s", len(done), stream.String())
	}
	if got, want := done[0]["path"], media.DefaultSRTPath(input, "es"); got != want {
		t.Errorf("done path = %v, want %q — a consumer opens the file this names", got, want)
	}
	if n := len(eventsOfKind(stream.String(), "error")); n != 0 {
		t.Errorf("%d error events on a successful run, want none:\n%s", n, stream.String())
	}
}
