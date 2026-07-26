package main

import (
	"fmt"
	"strings"
)

// outputMode selects what the tool produces for a translated subtitle track.
type outputMode int

const (
	// modeSRT writes a sidecar .srt next to the input video. This is the
	// default: it never rewrites the video file and works on any container.
	modeSRT outputMode = iota
	// modeMux writes a new container with the translated track embedded.
	modeMux
	// modeBoth writes the muxed container and the sidecar .srt.
	modeBoth
)

func (m outputMode) String() string {
	switch m {
	case modeMux:
		return "mux"
	case modeBoth:
		return "both"
	default:
		return "srt"
	}
}

// writesSRT reports whether the mode produces a sidecar .srt file.
func (m outputMode) writesSRT() bool { return m == modeSRT || m == modeBoth }

// writesContainer reports whether the mode produces a muxed video file.
func (m outputMode) writesContainer() bool { return m == modeMux || m == modeBoth }

func parseMode(s string) (outputMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "srt":
		return modeSRT, nil
	case "mux":
		return modeMux, nil
	case "both":
		return modeBoth, nil
	default:
		return 0, fmt.Errorf("invalid -mode %q: want srt, mux or both", s)
	}
}

// resolveMode downgrades container-writing modes when the input container
// cannot hold subtitle tracks (AVI). It returns the effective mode and a
// human-readable note, empty when nothing was downgraded.
func resolveMode(m outputMode, canEmbed bool) (outputMode, string) {
	if canEmbed || !m.writesContainer() {
		return m, ""
	}
	return modeSRT, fmt.Sprintf(
		"Note: this container can't embed subtitle tracks; -mode %s downgraded to srt.", m)
}
