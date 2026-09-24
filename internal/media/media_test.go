package media

import (
	"slices"
	"strings"
	"testing"
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
