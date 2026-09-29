package main

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

	// errOutputUnwritable is a path the user chose that cannot be written: a
	// read-only directory, a full disk, a name the filesystem refuses. Separate
	// from errOther because the GUI picks the save path, so this is the one
	// write failure a user can fix by choosing somewhere else.
	errOutputUnwritable errKind = "output_unwritable"

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
		errOutputUnwritable,
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
// This builds its own classified error and hands it to exitWith rather than
// going through fatalf: fatalf's error is kind "other", so routing through it
// would relabel every classified failure. exitWith reads the kind back off the
// error it is given, which is how one emit serves both helpers without either
// of them emitting a second time.
func fatalKindf(kind errKind, format string, args ...any) {
	exitWith(kindErrf(kind, format, args...))
}
