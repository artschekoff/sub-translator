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
