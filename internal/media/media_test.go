package media

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// A malformed metadata specifier (an extra "s:" prefix) makes ffmpeg abort with
// "Stream type specified multiple times" before it opens the output file.
func TestMuxArgsMetadataSpecifier(t *testing.T) {
	args := muxArgs("in.mkv", "dst.srt", "spa", "Spanish", "out.mkv", "copy", 2)

	want := "-metadata:s:s:2"
	if !slices.Contains(args, want) {
		t.Errorf("muxArgs did not emit %q\ngot: %v", want, args)
	}

	for _, a := range args {
		if strings.HasPrefix(a, "-metadata") && a != want {
			t.Errorf("unexpected metadata specifier %q, want %q", a, want)
		}
	}
}

func TestMuxArgsSubtitleIndexFollowsSubCount(t *testing.T) {
	for _, subCount := range []int{0, 1, 5} {
		args := muxArgs("in.mkv", "dst.srt", "spa", "Spanish", "out.mkv", "copy", subCount)
		want := "-metadata:s:s:" + string(rune('0'+subCount))
		if !slices.Contains(args, want) {
			t.Errorf("subCount=%d: want %q\ngot: %v", subCount, want, args)
		}
	}
}

// Matroska's Language element is ISO 639-2 (3 letters). A 2-letter code is
// invalid there: ffmpeg's own -map 0:m:language:spa selector cannot find such a
// track, and players show it as an unidentified language.
func TestMuxArgsNormalizesLanguageToISO6392(t *testing.T) {
	tests := []struct{ in, want string }{
		{"es", "language=spa"},
		{"fr", "language=fra"},
		{"ja", "language=jpn"},
		{"spa", "language=spa"}, // already 3-letter, left alone
		{"ES", "language=spa"},  // case-insensitive
	}

	for _, tt := range tests {
		args := muxArgs("in.mkv", "dst.srt", tt.in, "Spanish", "out.mkv", "copy", 2)
		if !slices.Contains(args, tt.want) {
			t.Errorf("muxArgs(lang=%q): want %q\ngot: %v", tt.in, tt.want, args)
		}
	}
}

func TestMuxArgsCarriesSubCodecAndOutput(t *testing.T) {
	args := muxArgs("in.mp4", "dst.srt", "fra", "French", "out.mp4", "mov_text", 0)

	i := slices.Index(args, "-c:s")
	if i < 0 || i+1 >= len(args) || args[i+1] != "mov_text" {
		t.Errorf("-c:s mov_text missing\ngot: %v", args)
	}
	if args[len(args)-1] != "out.mp4" {
		t.Errorf("output must be the last arg, got %q", args[len(args)-1])
	}
}

func audioFixture() []Stream {
	mk := func(idx int, codecType, lang string) Stream {
		s := Stream{Index: idx, CodecType: codecType}
		s.Tags.Language = lang
		return s
	}
	return []Stream{
		mk(0, "video", ""),
		mk(1, "audio", "eng"),
		mk(2, "audio", "rus"),
		mk(3, "subtitle", "eng"),
	}
}

func TestAudioStreamsSelectsOnlyAudio(t *testing.T) {
	got := AudioStreams(audioFixture())
	if len(got) != 2 {
		t.Fatalf("got %d audio streams, want 2: %v", len(got), got)
	}
	for _, s := range got {
		if s.CodecType != "audio" {
			t.Errorf("stream #%d is %q, not audio", s.Index, s.CodecType)
		}
	}
}

// Audio tracks are tagged with ISO 639-2 in containers just as subtitles are,
// so a 2-letter -from code has to match a 3-letter tag.
func TestFindAudioByLangNormalizesCodes(t *testing.T) {
	tests := []struct {
		lang      string
		wantIndex int
		wantOK    bool
	}{
		{"en", 1, true},
		{"eng", 1, true},
		{"ru", 2, true},
		{"RU", 2, true},
		{"fr", 0, false},
	}
	for _, tt := range tests {
		got, ok := FindAudioByLang(audioFixture(), tt.lang)
		if ok != tt.wantOK {
			t.Errorf("FindAudioByLang(%q) ok = %v, want %v", tt.lang, ok, tt.wantOK)
			continue
		}
		if ok && got.Index != tt.wantIndex {
			t.Errorf("FindAudioByLang(%q) = #%d, want #%d", tt.lang, got.Index, tt.wantIndex)
		}
	}
}

// A subtitle track tagged eng must never be returned as an audio track.
func TestFindAudioByLangIgnoresNonAudioStreams(t *testing.T) {
	got, ok := FindAudioByLang(audioFixture(), "en")
	if !ok {
		t.Fatal("want a match")
	}
	if got.CodecType != "audio" {
		t.Errorf("returned a %q stream", got.CodecType)
	}
}

// whisper.cpp's bundled decoder reads 16 kHz mono signed-16-bit PCM WAV and
// nothing else. Any deviation here produces "read_wav: unsupported format".
func TestExtractAudioArgsProduceWhisperReadyWAV(t *testing.T) {
	args := extractAudioArgs("in.mkv", 2, "out.wav")

	for _, pair := range [][2]string{
		{"-map", "0:2"},
		{"-ac", "1"},
		{"-ar", "16000"},
		{"-c:a", "pcm_s16le"},
	} {
		i := slices.Index(args, pair[0])
		if i < 0 || i+1 >= len(args) || args[i+1] != pair[1] {
			t.Errorf("want %s %s\ngot: %v", pair[0], pair[1], args)
		}
	}
	if !slices.Contains(args, "-vn") {
		t.Errorf("want -vn to drop the video stream\ngot: %v", args)
	}
	if args[len(args)-1] != "out.wav" {
		t.Errorf("output must be the last arg, got %q", args[len(args)-1])
	}
}

// The extracted WAV is 16 kHz mono s16 PCM — constant bitrate, no keyframes — so
// an input-side -ss is an exact byte seek and -c copy avoids a second decode of a
// multi-gigabyte source. Re-encoding here would cost more than the transcription
// it feeds.
func TestSliceWAVArgsSeekOnInputAndStreamCopy(t *testing.T) {
	args := sliceWAVArgs("full.wav", 10*time.Minute, 5*time.Minute, "chunk.wav")

	iInput := slices.Index(args, "-i")
	iSS := slices.Index(args, "-ss")
	iT := slices.Index(args, "-t")
	if iSS < 0 || iT < 0 || iInput < 0 {
		t.Fatalf("missing -ss/-t/-i: %v", args)
	}
	if iSS > iInput || iT > iInput {
		t.Errorf("-ss and -t must precede -i to seek the input, got: %v", args)
	}
	if args[iSS+1] != "600.000" {
		t.Errorf("-ss = %q, want 600.000", args[iSS+1])
	}
	if args[iT+1] != "300.000" {
		t.Errorf("-t = %q, want 300.000", args[iT+1])
	}

	iC := slices.Index(args, "-c")
	if iC < 0 || args[iC+1] != "copy" {
		t.Errorf("want -c copy, got: %v", args)
	}
	if args[len(args)-1] != "chunk.wav" {
		t.Errorf("output must be last, got %q", args[len(args)-1])
	}
}

func TestSliceWAVArgsSubSecondPrecision(t *testing.T) {
	args := sliceWAVArgs("in.wav", 1500*time.Millisecond, 250*time.Millisecond, "out.wav")
	iSS := slices.Index(args, "-ss")
	iT := slices.Index(args, "-t")
	if args[iSS+1] != "1.500" {
		t.Errorf("-ss = %q, want 1.500", args[iSS+1])
	}
	if args[iT+1] != "0.250" {
		t.Errorf("-t = %q, want 0.250", args[iT+1])
	}
}

// stubFfmpeg puts a shell script named ffmpeg first on PATH for the rest of the
// test. exec.Command resolves the name when the command is built, so the
// environment has to be in place before the call under test runs.
func stubFfmpeg(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the ffmpeg stub is a POSIX shell script")
	}
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ffmpeg"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// captureProcessStderr swaps os.Stderr for a pipe and returns everything written
// to the real file descriptor while fn runs — which is what an ffmpeg child
// inheriting our stderr would land in.
func captureProcessStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = w
	collected := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		collected <- string(b)
	}()
	fn()
	os.Stderr = saved
	w.Close()
	out := <-collected
	r.Close()
	return out
}

// ffmpegNoise is what a real ffmpeg writes to stderr: a banner, then progress
// lines rewritten in place with a carriage return and no newline at all. The
// event stream shares that file descriptor, so a JSON event emitted while one of
// these is in flight is unparseable — which is how a cancelled run lost its only
// terminal event.
const ffmpegNoise = `printf 'ffmpeg version 8.1.1 Copyright (c) 2000-2026 the FFmpeg developers\n' >&2
printf '  built with Apple clang version 21.0.0\n' >&2
printf 'size=       0KiB time=00:00:00.00 bitrate=N/A speed=N/A    \r' >&2
printf 'size=    7168KiB time=00:00:57.32 bitrate=1024.0kbits/s speed=  57x    \r' >&2
`

// A successful ffmpeg step must write nothing to the stream the events go to.
func TestASuccessfulFfmpegWritesNothingToOurStderr(t *testing.T) {
	stubFfmpeg(t, "#!/bin/sh\n"+ffmpegNoise+"out=\"\"\nfor a in \"$@\"; do out=\"$a\"; done\n: > \"$out\"\nexit 0\n")

	out := filepath.Join(t.TempDir(), "audio.wav")
	var err error
	leaked := captureProcessStderr(t, func() {
		err = ExtractAudio("movie.mkv", 1, out)
	})
	if err != nil {
		t.Fatalf("ExtractAudio() = %v, want success", err)
	}
	if leaked != "" {
		t.Errorf("ffmpeg wrote %d bytes to our stderr, want none:\n%q", len(leaked), leaked)
	}
}

// Capturing ffmpeg's stderr must not cost the user the diagnostics: before this,
// a failing ffmpeg explained itself only because its output was passed straight
// through. Now the explanation travels in the error — which also puts it in front
// of an -events json consumer, who never saw the inherited stream at all.
func TestAFailingFfmpegsDiagnosticsTravelInTheError(t *testing.T) {
	const diagnostic = "movie.mkv: Invalid data found when processing input"
	stubFfmpeg(t, "#!/bin/sh\n"+ffmpegNoise+"printf '"+diagnostic+"\\n' >&2\nexit 1\n")

	out := filepath.Join(t.TempDir(), "audio.wav")
	var err error
	leaked := captureProcessStderr(t, func() {
		err = ExtractAudio("movie.mkv", 1, out)
	})
	if err == nil {
		t.Fatal("ExtractAudio() = nil, want the stub's failure")
	}
	if leaked != "" {
		t.Errorf("ffmpeg wrote %d bytes to our stderr, want none:\n%q", len(leaked), leaked)
	}
	for _, want := range []string{diagnostic, "ffmpeg", "exit status 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}
	// The progress chatter is the one thing worth dropping: a feature film's
	// extraction writes thousands of those lines and they say nothing about the
	// failure.
	if strings.Contains(err.Error(), "size=") {
		t.Errorf("error carries ffmpeg's progress chatter:\n%s", err)
	}
}

// The diagnostic that matters is the last thing ffmpeg says, and on a long run it
// arrives after thousands of carriage-return-terminated progress fragments. A
// scan that split on newlines alone would see all of them as one line and lose
// the diagnostic with it.
func TestFfmpegDiagnosticsKeepsTheDiagnosticBehindTheProgressChatter(t *testing.T) {
	var raw strings.Builder
	raw.WriteString("ffmpeg version 8.1.1\n")
	for i := range 5000 {
		fmt.Fprintf(&raw, "size=%8dKiB time=00:00:00.00 speed=57x    \r", i)
	}
	raw.WriteString("Conversion failed!\n")

	got := ffmpegDiagnostics(strings.NewReader(raw.String()))
	if !strings.Contains(got, "Conversion failed!") {
		t.Errorf("diagnostics lost the failure line:\n%q", got)
	}
	if strings.Contains(got, "size=") {
		t.Errorf("diagnostics kept progress chatter:\n%q", got)
	}
	if lines := strings.Count(got, "\n") + 1; lines > ffmpegTailLines {
		t.Errorf("diagnostics kept %d lines, want at most %d", lines, ffmpegTailLines)
	}
}
