package media

import (
	"strconv"
	"strings"
	"time"
)

// Progress reports how far through a piece of media ffmpeg has got. total is
// zero when the caller does not know the length, which is why it is a duration
// pair rather than a percentage — the media package does not decide how an
// unknown total is presented.
type Progress func(done, total time.Duration)

// parseFFmpegTime reads the media position out of one of ffmpeg's progress
// lines. Those lines end in a carriage return rather than a newline, which is
// why the scanner splits on both; here they arrive already split.
func parseFFmpegTime(line string) (time.Duration, bool) {
	i := strings.Index(line, "time=")
	if i < 0 {
		return 0, false
	}
	field := strings.Fields(line[i+len("time="):])
	if len(field) == 0 {
		return 0, false
	}
	return parseClock(field[0])
}

// parseClock turns HH:MM:SS.ss into a duration. "N/A" — which ffmpeg prints
// before it has read anything — is not a time and reports false rather than
// zero, so a caller cannot mistake "not yet known" for "at the start".
func parseClock(s string) (time.Duration, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	sec, err3 := strconv.ParseFloat(parts[2], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	return time.Duration(h)*time.Hour + time.Duration(m)*time.Minute +
		time.Duration(sec*float64(time.Second)), true
}

// progressReporter turns a caller's Progress into the func(time.Duration)
// runFFmpeg wants, probing input for the total only when someone is actually
// listening. ContainerDuration is a second ffprobe subprocess, and a nil
// onProgress — every call site before this task, and several still after it —
// must not pay for a probe whose answer nothing will read. A failed probe
// still runs the command: it degrades to total=0, which the caller-supplied
// Progress is documented to treat as "unknown" rather than aborting the run
// over a stat it doesn't strictly need.
func progressReporter(input string, onProgress Progress) func(time.Duration) {
	if onProgress == nil {
		return nil
	}
	total, err := ContainerDuration(input)
	if err != nil {
		total = 0
	}
	return func(done time.Duration) {
		onProgress(done, total)
	}
}
