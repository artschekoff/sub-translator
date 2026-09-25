package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/artschekoff/sub-translator/internal/media"
	"github.com/artschekoff/sub-translator/internal/srt"
)

// audioStream builds a minimal audio Stream for pickAudioStream tests. Tags is
// an inline anonymous struct on media.Stream, so it can't be set in a literal.
func audioStream(index int, lang string) media.Stream {
	s := media.Stream{Index: index, CodecType: "audio"}
	s.Tags.Language = lang
	return s
}

// pickAudioStream used to just choose a track; it now also settles the
// language whisper is told to use, because forcing a -from that no track
// actually carries makes whisper emit confident nonsense in the wrong
// language instead of failing (that nonsense then survives translation into a
// finished, entirely wrong subtitle file). This proves the three cases that
// matter: an explicit -atrack wins outright, a matching -from passes through
// unchanged, and a -from that matches nothing falls back to the first track
// with the language cleared rather than forced.
func TestPickAudioStreamLanguageSelection(t *testing.T) {
	streams := []media.Stream{audioStream(1, "eng"), audioStream(2, "rus")}

	tests := []struct {
		name      string
		atrack    int
		from      string
		wantIndex int
		wantLang  string
	}{
		{
			name:      "explicit atrack wins even over a mismatched -from",
			atrack:    2,
			from:      "eng",
			wantIndex: 2,
			wantLang:  "eng",
		},
		{
			name:      "a -from that matches a track passes through unchanged",
			atrack:    -1,
			from:      "rus",
			wantIndex: 2,
			wantLang:  "rus",
		},
		{
			name:      "a -from matching no track falls back to the first stream with language cleared",
			atrack:    -1,
			from:      "fra",
			wantIndex: 1,
			wantLang:  "",
		},
		{
			name:      "no -from at all picks the first stream with no language forced",
			atrack:    -1,
			from:      "",
			wantIndex: 1,
			wantLang:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src, lang, err := pickAudioStream("movie.mkv", streams, tt.atrack, tt.from)
			if err != nil {
				t.Fatalf("pickAudioStream: %v", err)
			}
			if src.Index != tt.wantIndex {
				t.Errorf("stream index = %d, want %d", src.Index, tt.wantIndex)
			}
			if lang != tt.wantLang {
				t.Errorf("language = %q, want %q", lang, tt.wantLang)
			}
		})
	}
}

// An -atrack that names no audio stream at all must still error, exactly as
// before the extraction.
func TestPickAudioStreamRejectsUnknownExplicitTrack(t *testing.T) {
	streams := []media.Stream{audioStream(1, "eng")}
	if _, _, err := pickAudioStream("movie.mkv", streams, 9, ""); err == nil {
		t.Error("want an error for an -atrack that matches nothing")
	}
}

// A file with no audio streams at all has nothing to transcribe.
func TestPickAudioStreamRejectsNoAudioTracks(t *testing.T) {
	if _, _, err := pickAudioStream("movie.mkv", nil, -1, ""); err == nil {
		t.Error("want an error when there are no audio tracks")
	}
}

func TestPlanChunksEvenDivision(t *testing.T) {
	got := planChunks(30*time.Minute, 10*time.Minute)
	if len(got) != 3 {
		t.Fatalf("got %d chunks, want 3: %+v", len(got), got)
	}
	for i, c := range got {
		if c.Index != i {
			t.Errorf("chunk %d has Index %d", i, c.Index)
		}
		if c.Start != time.Duration(i)*10*time.Minute {
			t.Errorf("chunk %d starts at %v", i, c.Start)
		}
		if c.Dur != 10*time.Minute {
			t.Errorf("chunk %d runs %v, want 10m", i, c.Dur)
		}
	}
}

// A film is never an exact multiple of the chunk length; the tail must be short
// rather than reading past the end of the audio.
func TestPlanChunksShortTail(t *testing.T) {
	got := planChunks(25*time.Minute, 10*time.Minute)
	if len(got) != 3 {
		t.Fatalf("got %d chunks, want 3", len(got))
	}
	last := got[2]
	if last.Start != 20*time.Minute || last.Dur != 5*time.Minute {
		t.Errorf("tail = start %v dur %v, want 20m/5m", last.Start, last.Dur)
	}
	var sum time.Duration
	for _, c := range got {
		sum += c.Dur
	}
	if sum != 25*time.Minute {
		t.Errorf("chunks cover %v, want exactly 25m", sum)
	}
}

func TestPlanChunksShorterThanOneChunk(t *testing.T) {
	got := planChunks(3*time.Minute, 10*time.Minute)
	if len(got) != 1 {
		t.Fatalf("got %d chunks, want 1", len(got))
	}
	if got[0].Start != 0 || got[0].Dur != 3*time.Minute {
		t.Errorf("chunk = start %v dur %v, want 0/3m", got[0].Start, got[0].Dur)
	}
}

func TestPlanChunksDegenerateInputs(t *testing.T) {
	if got := planChunks(0, 10*time.Minute); len(got) != 0 {
		t.Errorf("zero total = %v, want none", got)
	}
	if got := planChunks(10*time.Minute, 0); len(got) != 0 {
		t.Errorf("zero chunk length = %v, want none", got)
	}
	if got := planChunks(-time.Minute, time.Minute); len(got) != 0 {
		t.Errorf("negative total = %v, want none", got)
	}
}

// -fast only makes sense when there is a slow step to hide. A subtitle track is
// already on disk.
func TestValidateFastRejectsSubtitleSource(t *testing.T) {
	err := validateFast(true, sourceSub, modeSRT, 10)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "-source audio") {
		t.Errorf("error %q does not say what to do instead", err)
	}
}

// A container cannot be rewritten incrementally, so the progressive promise
// cannot be kept in mux modes.
func TestValidateFastRejectsContainerModes(t *testing.T) {
	for _, m := range []outputMode{modeMux, modeBoth} {
		err := validateFast(true, sourceAudio, m, 10)
		if err == nil {
			t.Errorf("mode %v: want an error", m)
			continue
		}
		if !strings.Contains(err.Error(), "-mode srt") {
			t.Errorf("mode %v: error %q does not name the working mode", m, err)
		}
	}
}

func TestValidateFastRejectsBadChunkLength(t *testing.T) {
	for _, n := range []int{0, -5} {
		if err := validateFast(true, sourceAudio, modeSRT, n); err == nil {
			t.Errorf("chunk %d: want an error", n)
		}
	}
}

func TestValidateFastAcceptsTheWorkingCombination(t *testing.T) {
	if err := validateFast(true, sourceAudio, modeSRT, 10); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// Without -fast nothing is validated, because nothing changes.
func TestValidateFastIgnoresEverythingWhenOff(t *testing.T) {
	for _, m := range []outputMode{modeSRT, modeMux, modeBoth} {
		for _, s := range []sourceMode{sourceSub, sourceAudio} {
			if err := validateFast(false, s, m, 0); err != nil {
				t.Errorf("fast=false must never error, got %v", err)
			}
		}
	}
}

// resolveChunkMinutes is what stands between the -chunk flag and validateFast. A
// plain 0 default there would make "-chunk 0" indistinguishable from omitting the
// flag, silently handing the user a 10-minute chunk instead of the error
// validateFast promises — so only the -1 sentinel may be treated as "not given";
// 0 and every other value must pass through untouched for validateFast to see.
func TestResolveChunkMinutes(t *testing.T) {
	tests := []struct {
		name        string
		flagValue   int
		configValue int
		want        int
	}{
		{"unset, no config, falls back to 10", -1, 0, 10},
		{"unset, config set, uses config", -1, 5, 5},
		{"explicit zero survives for validateFast to reject", 0, 5, 0},
		{"explicit negative survives for validateFast to reject", -2, 5, -2},
		{"explicit value wins over config", 7, 5, 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveChunkMinutes(tt.flagValue, tt.configValue); got != tt.want {
				t.Errorf("resolveChunkMinutes(%d, %d) = %d, want %d",
					tt.flagValue, tt.configValue, got, tt.want)
			}
		})
	}
}

// Merging chunks is where progressive output goes wrong: the second chunk's
// timings are relative to its own start, and its numbering restarts at 1. This
// reproduces the merge the loop performs and pins both.
func TestChunkMergeProducesOneContinuousFile(t *testing.T) {
	chunk0 := []srt.Block{
		{Index: "1", Timing: "00:00:01,000 --> 00:00:03,000", Text: "opening line"},
		{Index: "2", Timing: "00:09:50,000 --> 00:09:55,000", Text: "end of first chunk"},
	}
	chunk1 := []srt.Block{
		{Index: "1", Timing: "00:00:02,000 --> 00:00:04,500", Text: "second chunk opens"},
	}

	var merged []srt.Block
	for i, c := range [][]srt.Block{chunk0, chunk1} {
		shifted, err := srt.Shift(c, time.Duration(i)*10*time.Minute)
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		merged = append(merged, shifted...)
	}
	merged = srt.Renumber(merged)

	if len(merged) != 3 {
		t.Fatalf("merged %d blocks, want 3", len(merged))
	}
	for i, want := range []string{"1", "2", "3"} {
		if merged[i].Index != want {
			t.Errorf("block %d index = %q, want %q", i, merged[i].Index, want)
		}
	}
	// The second chunk's first line is 2 s into a chunk that starts at 10:00.
	if got := merged[2].Timing; got != "00:10:02,000 --> 00:10:04,500" {
		t.Errorf("second chunk timing = %q, want 00:10:02,000 --> 00:10:04,500", got)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "out.srt")
	if err := srt.Write(path, merged); err != nil {
		t.Fatal(err)
	}
	parsed, err := srt.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 3 {
		t.Errorf("wrote a file that parses back as %d blocks", len(parsed))
	}
}

// The plan and the merge must agree: every chunk's blocks land inside that
// chunk's window, so no line is attributed to the wrong part of the film.
func TestPlanAndShiftAgreeOnChunkWindows(t *testing.T) {
	total := 25 * time.Minute
	chunkLen := 10 * time.Minute
	for _, c := range planChunks(total, chunkLen) {
		block := []srt.Block{{Index: "1", Timing: "00:00:00,500 --> 00:00:01,500", Text: "x"}}
		shifted, err := srt.Shift(block, c.Start)
		if err != nil {
			t.Fatal(err)
		}
		start, err := firstStart(shifted[0].Timing)
		if err != nil {
			t.Fatal(err)
		}
		if start < c.Start || start >= c.Start+c.Dur {
			t.Errorf("chunk %d: a block at %v falls outside [%v, %v)",
				c.Index, start, c.Start, c.Start+c.Dur)
		}
	}
}

// firstStart reads the left-hand timestamp out of an SRT timing line.
func firstStart(timing string) (time.Duration, error) {
	left, _, ok := strings.Cut(timing, "-->")
	if !ok {
		return 0, fmt.Errorf("malformed timing %q", timing)
	}
	var h, m, s, ms int
	if _, err := fmt.Sscanf(strings.TrimSpace(left), "%d:%d:%d,%d", &h, &m, &s, &ms); err != nil {
		return 0, err
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute +
		time.Duration(s)*time.Second + time.Duration(ms)*time.Millisecond, nil
}
