package main

import (
	"slices"
	"testing"
)

// These strings are a contract with anything driving the tool. Renaming one
// silently breaks a consumer that switches on it, so they are pinned here
// rather than left to whatever the constant happens to say.
func TestErrKindsAreTheDocumentedStrings(t *testing.T) {
	want := map[errKind]string{
		errMissingKey:      "missing_key",
		errWhisperNotFound: "whisper_not_found",
		errNoModel:         "no_model",
		errAmbiguousModel:  "ambiguous_model",
		errNoAudioTrack:    "no_audio_track",
		errNoSubtitleTrack: "no_subtitle_track",
		errProbeFailed:     "probe_failed",
		errRateLimited:     "rate_limited",
		errTranscriptEmpty: "transcript_empty",
		errCancelled:       "cancelled",
		errOther:           "other",
	}
	for k, s := range want {
		if string(k) != s {
			t.Errorf("kind %q, want %q", string(k), s)
		}
	}
	if len(allErrKinds()) != len(want) {
		t.Errorf("allErrKinds() has %d entries, want %d — add the new one to both",
			len(allErrKinds()), len(want))
	}
}

func TestAllErrKindsHasNoDuplicates(t *testing.T) {
	seen := map[errKind]bool{}
	for _, k := range allErrKinds() {
		if seen[k] {
			t.Errorf("kind %q appears twice", k)
		}
		seen[k] = true
	}
}

// Every kind must be lowercase with underscores: a consumer matching on these
// should not have to guess at casing.
func TestErrKindsAreSnakeCase(t *testing.T) {
	for _, k := range allErrKinds() {
		for _, r := range string(k) {
			if (r < 'a' || r > 'z') && r != '_' {
				t.Errorf("kind %q contains %q; want lowercase and underscores only", k, r)
			}
		}
	}
}

func TestErrOtherIsNotInTheClassifiedSet(t *testing.T) {
	classified := allErrKinds()
	i := slices.Index(classified, errOther)
	if i < 0 {
		t.Fatal("errOther must be in allErrKinds so the list is exhaustive")
	}
	if classified[len(classified)-1] != errOther {
		t.Error("errOther should be listed last: it is the fallback, not a peer")
	}
}
