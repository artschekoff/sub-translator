package main

import (
	"fmt"

	"github.com/artschekoff/sub-translator/internal/events"
)

// errKind classifies a fatal error so a program driving this tool can act on the
// reason instead of matching on the wording. The strings are a contract: a
// wrapper switches on them, so they are pinned by a test and must not be
// renamed casually.
type errKind string

const (
	errMissingKey      errKind = "missing_key"
	errWhisperNotFound errKind = "whisper_not_found"
	errNoModel         errKind = "no_model"
	errAmbiguousModel  errKind = "ambiguous_model"
	errNoAudioTrack    errKind = "no_audio_track"
	errNoSubtitleTrack errKind = "no_subtitle_track"
	errProbeFailed     errKind = "probe_failed"
	errRateLimited     errKind = "rate_limited"
	errTranscriptEmpty errKind = "transcript_empty"
	errCancelled       errKind = "cancelled"

	// errOther is what an unclassified failure reports. It exists so a consumer
	// always receives a kind, never an absent field.
	errOther errKind = "other"
)

// allErrKinds lists every kind, fallback last, for the test that keeps this file
// and the documentation in step.
func allErrKinds() []errKind {
	return []errKind{
		errMissingKey,
		errWhisperNotFound,
		errNoModel,
		errAmbiguousModel,
		errNoAudioTrack,
		errNoSubtitleTrack,
		errProbeFailed,
		errRateLimited,
		errTranscriptEmpty,
		errCancelled,
		errOther,
	}
}

// fatalKindf reports a fatal error with a kind attached, for the failures a
// caller can do something about. Everything else keeps using fatalf, which
// reports no kind because there is nothing useful to say about it.
func fatalKindf(kind errKind, format string, args ...any) {
	ev.Emit(&events.Error{Kind: string(kind), Message: fmt.Sprintf(format, args...)})
	fatalf("%s", fmt.Sprintf(format, args...))
}
