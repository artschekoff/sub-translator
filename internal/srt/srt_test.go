package srt

import (
	"testing"
	"time"
)

func TestParseAndFormatTimestampRoundTrip(t *testing.T) {
	tests := []struct {
		text string
		want time.Duration
	}{
		{"00:00:00,000", 0},
		{"00:00:07,450", 7*time.Second + 450*time.Millisecond},
		{"01:23:45,678", time.Hour + 23*time.Minute + 45*time.Second + 678*time.Millisecond},
		{"02:19:08,000", 2*time.Hour + 19*time.Minute + 8*time.Second},
	}
	for _, tt := range tests {
		got, err := parseTimestamp(tt.text)
		if err != nil {
			t.Errorf("parseTimestamp(%q): %v", tt.text, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseTimestamp(%q) = %v, want %v", tt.text, got, tt.want)
		}
		if back := formatTimestamp(got); back != tt.text {
			t.Errorf("formatTimestamp round trip = %q, want %q", back, tt.text)
		}
	}
}

func TestParseTimestampRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "nonsense", "00:00", "0:0:0"} {
		if _, err := parseTimestamp(bad); err == nil {
			t.Errorf("parseTimestamp(%q): want an error", bad)
		}
	}
}

// Chunk N is transcribed as if it started at zero, so merging it into the film
// requires adding the chunk's start offset to every timestamp. Getting this wrong
// puts the back half of a film's subtitles minutes out of sync.
func TestShiftOffsetsBothEnds(t *testing.T) {
	in := []Block{
		{Index: "1", Timing: "00:00:00,000 --> 00:00:02,500", Text: "first"},
		{Index: "2", Timing: "00:00:03,000 --> 00:00:05,250", Text: "second"},
	}

	got, err := Shift(in, 10*time.Minute)
	if err != nil {
		t.Fatalf("Shift: %v", err)
	}
	want := []string{
		"00:10:00,000 --> 00:10:02,500",
		"00:10:03,000 --> 00:10:05,250",
	}
	for i := range want {
		if got[i].Timing != want[i] {
			t.Errorf("block %d timing = %q, want %q", i, got[i].Timing, want[i])
		}
		if got[i].Text != in[i].Text {
			t.Errorf("block %d text changed to %q", i, got[i].Text)
		}
	}
}

func TestShiftByZeroIsIdentity(t *testing.T) {
	in := []Block{{Index: "1", Timing: "00:01:02,003 --> 00:01:04,005", Text: "x"}}
	got, err := Shift(in, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Timing != in[0].Timing {
		t.Errorf("timing = %q, want it unchanged", got[0].Timing)
	}
}

// A malformed timing must not be silently rewritten into a plausible-looking
// wrong one — that would desync a file with no visible cause.
func TestShiftRejectsMalformedTiming(t *testing.T) {
	for _, bad := range []string{"garbage", "00:00:01,000", "00:00:01,000 --> nope"} {
		_, err := Shift([]Block{{Index: "1", Timing: bad, Text: "x"}}, time.Second)
		if err == nil {
			t.Errorf("Shift with timing %q: want an error", bad)
		}
	}
}

// Concatenated chunks each restart their numbering at 1, which players reject.
func TestRenumberMakesOneAscendingSequence(t *testing.T) {
	in := []Block{
		{Index: "1", Timing: "00:00:00,000 --> 00:00:01,000", Text: "a"},
		{Index: "2", Timing: "00:00:01,000 --> 00:00:02,000", Text: "b"},
		{Index: "1", Timing: "00:10:00,000 --> 00:10:01,000", Text: "c"},
	}
	got := Renumber(in)
	for i, b := range got {
		want := []string{"1", "2", "3"}[i]
		if b.Index != want {
			t.Errorf("block %d index = %q, want %q", i, b.Index, want)
		}
		if b.Timing != in[i].Timing || b.Text != in[i].Text {
			t.Errorf("block %d: Renumber altered timing or text", i)
		}
	}
}

func TestRenumberEmpty(t *testing.T) {
	if got := Renumber(nil); len(got) != 0 {
		t.Errorf("Renumber(nil) = %v, want empty", got)
	}
}

// Renumber then Write must produce a file that starts at 1 and counts up.
func TestRenumberedFileIsSequential(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/out.srt"
	blocks := Renumber([]Block{
		{Index: "7", Timing: "00:00:00,000 --> 00:00:01,000", Text: "a"},
		{Index: "9", Timing: "00:00:01,000 --> 00:00:02,000", Text: "b"},
	})
	if err := Write(path, blocks); err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 2 || parsed[0].Index != "1" || parsed[1].Index != "2" {
		t.Errorf("round trip produced %v", parsed)
	}
}
