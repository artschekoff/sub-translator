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
	errMissingKey errKind = "missing_key"
	// errInvalidKey sits next to errMissingKey rather than next to
	// errRateLimited, which is the other endpoint refusal, because the ordering
	// here is what the spec's "the first N kinds are the ones a caller routes to
	// its settings screen" reads off: a key that is wrong is fixed in the same
	// place as a key that is absent.
	errInvalidKey      errKind = "invalid_key"
	errWhisperNotFound errKind = "whisper_not_found"
	errNoModel         errKind = "no_model"
	errAmbiguousModel  errKind = "ambiguous_model"
	errNoAudioTrack    errKind = "no_audio_track"
	errNoSubtitleTrack errKind = "no_subtitle_track"
	errProbeFailed     errKind = "probe_failed"
	errRateLimited     errKind = "rate_limited"
	errTranscriptEmpty errKind = "transcript_empty"
	errCancelled       errKind = "cancelled"

	// errOther is what an unclassified failure reports. fatalf emits it
	// automatically, so a consumer always receives exactly one error event on
	// any fatal path, classified or not, never an absent field and never
	// silence.
	errOther errKind = "other"
)

// allErrKinds lists every kind, fallback last, for the test that keeps this file
// and the documentation in step.
func allErrKinds() []errKind {
	return []errKind{
		errMissingKey,
		errInvalidKey,
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
// reports no kind because there is nothing useful to say about it — but still
// emits, with kind "other".
//
// This goes straight to fatalTail rather than through fatalf: fatalf emits its
// own error event (kind "other"), and calling it here would double-emit —
// once with this kind, once with "other" — for every classified failure.
func fatalKindf(kind errKind, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	ev.Emit(&events.Error{Kind: string(kind), Message: msg})
	fatalTail("%s", msg)
}
