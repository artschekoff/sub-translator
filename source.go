package main

import (
	"fmt"
	"strings"
)

// sourceMode selects where the subtitle text comes from.
type sourceMode int

const (
	// sourceAuto reads an existing subtitle track when the file has one and
	// otherwise offers to transcribe the audio.
	sourceAuto sourceMode = iota
	// sourceSub reads an existing subtitle track and fails if there is none.
	sourceSub
	// sourceAudio transcribes the audio with whisper, ignoring any subtitle
	// tracks the file may already carry.
	sourceAudio
)

func (s sourceMode) String() string {
	switch s {
	case sourceSub:
		return "sub"
	case sourceAudio:
		return "audio"
	default:
		return "auto"
	}
}

func parseSource(s string) (sourceMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "auto":
		return sourceAuto, nil
	case "sub":
		return sourceSub, nil
	case "audio":
		return sourceAudio, nil
	default:
		return 0, fmt.Errorf("invalid -source %q: want sub, audio or auto", s)
	}
}

// resolveSource turns auto into a concrete choice. It returns the mode to use
// and whether the user still has to approve it: transcription is expensive
// enough that auto must never start one silently.
func resolveSource(s sourceMode, hasSubs, interactive bool) (sourceMode, bool, error) {
	if s != sourceAuto {
		return s, false, nil
	}
	if hasSubs {
		return sourceSub, false, nil
	}
	if !interactive {
		return 0, false, fmt.Errorf(
			"no subtitle tracks, and -source auto cannot ask for confirmation here; " +
				"pass -source audio to transcribe the audio track")
	}
	return sourceAudio, true, nil
}
