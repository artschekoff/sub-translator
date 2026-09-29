package media

import (
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseFFmpegTime(t *testing.T) {
	for _, tc := range []struct {
		line string
		want time.Duration
		ok   bool
	}{
		{"size=    1024kB time=00:01:23.45 bitrate= 100.0kbits/s speed=1.5x", 83*time.Second + 450*time.Millisecond, true},
		{"frame=  100 fps=25 q=-1.0 size=N/A time=00:00:04.00 bitrate=N/A speed=4x", 4 * time.Second, true},
		{"size=N/A time=N/A bitrate=N/A speed=N/A", 0, false},
		{"[matroska @ 0x1] Invalid data found when processing input", 0, false},
		{"time=01:02:03.99", 3723*time.Second + 990*time.Millisecond, true},
		{"", 0, false},
	} {
		got, ok := parseFFmpegTime(tc.line)
		if ok != tc.ok || got != tc.want {
			t.Errorf("parseFFmpegTime(%q) = %v, %v; want %v, %v", tc.line, got, ok, tc.want, tc.ok)
		}
	}
}

// TestRunFFmpegReportsProgress drives a stub ffmpeg that prints CR-terminated
// progress lines and asserts the callback saw them in order.
func TestRunFFmpegReportsProgress(t *testing.T) {
	stubFfmpeg(t, "#!/bin/sh\n"+
		"printf 'size=    1024kB time=00:00:01.00 bitrate=100.0kbits/s speed=1x    \\r' >&2\n"+
		"printf 'size=    2048kB time=00:00:02.50 bitrate=100.0kbits/s speed=1x    \\r' >&2\n"+
		"printf 'size=    3072kB time=00:00:04.00 bitrate=100.0kbits/s speed=1x    \\r' >&2\n"+
		"out=\"\"\nfor a in \"$@\"; do out=\"$a\"; done\n: > \"$out\"\nexit 0\n")

	out := filepath.Join(t.TempDir(), "audio.wav")
	var got []time.Duration
	report := func(d time.Duration) { got = append(got, d) }

	cmd := exec.Command("ffmpeg", "-y", "-i", "in.mkv", out)
	if err := runFFmpeg(cmd, report); err != nil {
		t.Fatalf("runFFmpeg() = %v, want success", err)
	}

	want := []time.Duration{time.Second, 2500 * time.Millisecond, 4 * time.Second}
	if !slices.Equal(got, want) {
		t.Errorf("report saw %v, want %v", got, want)
	}
}

// TestProgressLinesStillDoNotReachTheDiagnostics guards the thing this task
// could break: a progress line must still be dropped from the error tail.
func TestProgressLinesStillDoNotReachTheDiagnostics(t *testing.T) {
	const diagnostic = "movie.mkv: Invalid data found when processing input"
	stubFfmpeg(t, "#!/bin/sh\n"+
		"printf 'size=    1024kB time=00:00:01.00 bitrate=100.0kbits/s speed=1x    \\r' >&2\n"+
		"printf 'size=    2048kB time=00:00:02.00 bitrate=100.0kbits/s speed=1x    \\r' >&2\n"+
		"printf '"+diagnostic+"\\n' >&2\n"+
		"exit 1\n")

	var reported []time.Duration
	cmd := exec.Command("ffmpeg", "-y", "-i", "in.mkv", "out.wav")
	err := runFFmpeg(cmd, func(d time.Duration) { reported = append(reported, d) })
	if err == nil {
		t.Fatal("runFFmpeg() = nil, want the stub's failure")
	}
	if !strings.Contains(err.Error(), diagnostic) {
		t.Errorf("error %q does not carry the diagnostic %q", err, diagnostic)
	}
	if strings.Contains(err.Error(), "size=") {
		t.Errorf("error carries ffmpeg's progress chatter:\n%s", err)
	}
	want := []time.Duration{time.Second, 2 * time.Second}
	if !slices.Equal(reported, want) {
		t.Errorf("report saw %v, want %v — the progress lines must still be parsed even though they are dropped from the tail", reported, want)
	}
}
