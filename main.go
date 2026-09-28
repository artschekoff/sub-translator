package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/artschekoff/sub-translator/internal/config"
	"github.com/artschekoff/sub-translator/internal/events"
	"github.com/artschekoff/sub-translator/internal/media"
	"github.com/artschekoff/sub-translator/internal/models"
	"github.com/artschekoff/sub-translator/internal/srt"
	"github.com/artschekoff/sub-translator/internal/translate"
	"github.com/artschekoff/sub-translator/internal/whisper"
)

// version is injected at build time via -ldflags "-X main.version=..."
var version = "dev"

// runTmpDir is the one scratch directory a run owns: the extracted WAV, the
// whisper output and the intermediate SRTs all live inside it. It is a package
// variable because the two ways this program ends without unwinding the stack —
// exitWith's os.Exit and a SIGINT — both have to be able to remove it, and a
// feature film's 16 kHz WAV is well over a gigabyte to leave behind.
var runTmpDir string

// ev carries the machine-readable event stream. It is disabled unless
// -events json is passed, and Emit is a no-op when disabled, so the call sites
// below need no guards.
var ev *events.Emitter

// proseErr is stderr for everything this process writes there in prose: the
// warning lines, the fatal error line, the usage dump, the interactive prompt.
// It is the same serialised writer the event emitter is given, so a JSON event
// — including the one the SIGINT handler emits from its own goroutine — can
// never land inside one of those lines, and none of them can land inside an
// event. Both kinds of line share stderr by design; what the lock buys is that
// each one is whole.
var proseErr = events.NewSyncWriter(os.Stderr)

// setupRunTmpDir creates the run-scoped scratch directory and arms the signal
// handler that removes it. Go's default SIGINT disposition terminates the
// process outright, so a deferred cleanup never runs when a user aborts a
// twenty-minute transcription with Ctrl-C.
func setupRunTmpDir() {
	dir, err := os.MkdirTemp("", "sub-translator-*")
	if err != nil {
		fatalf("create temp dir: %v", err)
	}
	runTmpDir = dir

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		// Not fatalKindf: that exits 1 and this is a signal, not a failure — the
		// shell convention below is what has to survive, unchanged, whether or
		// not a caller is reading the event stream.
		ev.Emit(&events.Error{Kind: string(errCancelled), Message: "interrupted"})
		cleanupRunTmpDir()
		// 128 + SIGINT: the shell convention for "terminated by a signal".
		os.Exit(130)
	}()
}

func cleanupRunTmpDir() {
	if runTmpDir != "" {
		os.RemoveAll(runTmpDir)
	}
}

const usage = `sub-translator — subtitle translator for MKV, MP4, AVI and more

Usage:
  sub-translator [flags] <input>
  sub-translator config <list|get|set|path> [key] [value]
  sub-translator model <list|pull> [name]

Subtitle sources (-source):
  sub    read a subtitle track already in the file
  audio  transcribe the audio track with whisper.cpp
  auto   use a subtitle track when there is one, otherwise offer to transcribe (default)

Output modes (-mode):
  srt   write a translated .srt next to the input video (default)
  mux   write a new video file with the translated track embedded
  both  write the muxed video file and the .srt

Supported formats:
  MKV, MP4/M4V/MOV  — translated track embedded into output file
  AVI               — embedded subs not supported; -mode mux/both fall back to srt

Transcription needs whisper.cpp (brew install whisper.cpp) and a ggml model.
Models already downloaded by other whisper.cpp front-ends are found
automatically; otherwise run: sub-translator model pull large-v3-turbo

Translation goes through the OpenAI API and needs a key. Supply it either way:
  export OPENAI_API_KEY=sk-...
  sub-translator config set translate.api-key sk-...

Flags:
  -from          source language code (prompted for if omitted; detected in audio mode)
  -to            target language code (required; prompted for if omitted)
  -source        subtitle source: sub, audio or auto (default: auto)
  -track         subtitle stream index, -1 = auto-detect by -from lang (default: -1)
  -atrack        audio stream index, -1 = auto (default: -1)
  -mode          output mode: srt, mux or both (default: srt)
  -out           output path (.srt in srt mode, container otherwise)
  -fast          translate progressively so the opening is watchable within a minute
  -chunk         chunk length in minutes for -fast (default 10, or whisper.chunk-minutes)
  -workers       batches translated at once (default 4, or translate.workers)
  -whisper-model path to a whisper ggml model
  -whisper-bin   path to the whisper-cli binary
  -vad-model     path to a Silero VAD model
  -probe         list the file's tracks and exit
  -events        emit machine-readable events on stderr: json
  -version       print version and exit

Examples:
  sub-translator -to es movie.mkv
  sub-translator -to es -source audio movie.mkv
  sub-translator -to fr -mode mux movie.mp4
  sub-translator -to ru -track 3 movie.mkv
  sub-translator -to es -source audio -fast movie.mkv
  sub-translator config set whisper.model ~/models/ggml-large-v3-turbo.bin
  sub-translator model pull large-v3-turbo
`

// eventsRequested reports whether argv asks for the JSON event stream, reading
// os.Args the way flag.Parse is about to read it.
//
// It exists because flag.Parse can end the process before the emitter is built —
// an unknown flag prints a usage error and exits 2 — and "the first line of any
// run is always a version event" has to hold in precisely that case, which is
// the one version negotiation exists for: a wrapper newer than the CLI it found
// on PATH.
//
// It is not a second parser. It reads no values, validates nothing and keeps no
// state; it asks fs a single question — does this flag take a value, so is the
// next word its value or the first positional argument — because that is the one
// piece of flag.Parse's knowledge it cannot do without. Asking the real flag set
// rather than keeping a list here is what stops the two ever disagreeing about a
// command line that runs: this stops where flag.Parse stops, at the first
// positional argument or "--", so `movie.mkv -events json` is ignored by both,
// exactly like every other flag written after the input path.
//
// One asymmetry is deliberate. At a flag this CLI does not define, flag.Parse is
// about to fail rather than produce a run, so this keeps looking instead of
// stopping — which is what lets `-bogus -events json` be answered on the stream
// instead of in silence, the whole reason for scanning early. It cannot make the
// two disagree about any argv that actually runs.
func eventsRequested(args []string, fs *flag.FlagSet) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-" || a == "--" || !strings.HasPrefix(a, "-") {
			// Where flag.Parse stops: everything from here is positional.
			return false
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if name == "events" {
			if hasValue {
				return value == "json"
			}
			return i+1 < len(args) && args[i+1] == "json"
		}
		if hasValue {
			continue
		}
		if f := fs.Lookup(name); f != nil && !isBoolFlag(f) {
			// The next word is this flag's value, not the input path.
			i++
		}
	}
	return false
}

// isBoolFlag reports whether f is a boolean flag, the same way the flag package
// itself decides whether the following argument belongs to it.
func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// splitEventsFlag takes a -events flag out of a subcommand's arguments and
// returns the rest of them, the value it carried, and whether it was there.
//
// A subcommand needs its own scan, and a different one: `config get <key>` is
// three positional words, so eventsRequested's rule — stop at the first word
// that is not a flag — would stop immediately and never see the flag. And the
// flag has to be removed rather than merely noticed, because the subcommands
// count their arguments: passing it through is what made `config get k -events
// json` fail with "config get: want exactly one key". Nothing else in argv is
// touched, so every verb sees exactly what it saw before the flag existed.
func splitEventsFlag(args []string) (rest []string, value string, found bool) {
	without := func(from, to int) []string {
		out := append([]string{}, args[:from]...)
		return append(out, args[min(to, len(args)):]...)
	}
	for i, a := range args {
		switch {
		case a == "-events", a == "--events":
			if i+1 < len(args) {
				return without(i, i+2), args[i+1], true
			}
			return without(i, i+1), "", true
		case strings.HasPrefix(a, "-events="), strings.HasPrefix(a, "--events="):
			_, v, _ := strings.Cut(a, "=")
			return without(i, i+1), v, true
		}
	}
	return args, "", false
}

func main() {
	if verb := subcommandVerb(os.Args[1:]); verb != "" {
		// A subcommand is not a run: it prints plain text on stdout and has no
		// stage, no progress and nothing that could honestly be a done or an
		// error. But -events json still has to compose with it — the flag worked
		// on `config path` and `config list` before this branch existed, and a
		// wrapper that appends it to every invocation must not get an
		// inexplicable failure on a settings read. So the flag is accepted,
		// taken out of what the subcommand sees, and answered with a version
		// event and nothing else: the shape -probe and -version already have.
		args, value, found := splitEventsFlag(os.Args[1:])
		if found {
			switch value {
			case "json":
				ev = events.New(proseErr)
				ev.Emit(&events.Version{Events: events.ProtocolVersion, Tool: version})
			default:
				// The same refusal the run path gives, and for the same reason:
				// an unsupported format cannot be answered in that format.
				fatalf("invalid -events %q: the only supported value is json", value)
			}
		}
		if handled, err := dispatchSubcommand(args, os.Stdout); handled {
			if err != nil {
				fatalf("%v", err)
			}
			// No terminal event, as for -probe and -version.
			return
		}
	}

	from := flag.String("from", "", "source language (required)")
	to := flag.String("to", "", "target language (required)")
	track := flag.Int("track", -1, "subtitle stream index (-1 = auto)")
	sourceFlag := flag.String("source", "auto", "subtitle source: sub, audio or auto")
	atrack := flag.Int("atrack", -1, "audio stream index (-1 = auto)")
	whisperModel := flag.String("whisper-model", "", "path to a whisper ggml model")
	whisperBin := flag.String("whisper-bin", "", "path to the whisper-cli binary")
	vadModel := flag.String("vad-model", "", "path to a Silero VAD model")
	modeFlag := flag.String("mode", "srt", "output mode: srt, mux or both")
	out := flag.String("out", "", "output path (.srt in srt mode, container otherwise)")
	workers := flag.Int("workers", -1, "batches translated at once (default 4, or translate.workers)")
	fast := flag.Bool("fast", false, "translate progressively so the opening is watchable within a minute")
	chunkMin := flag.Int("chunk", -1, "chunk length in minutes for -fast (default 10, or whisper.chunk-minutes)")
	showVersion := flag.Bool("version", false, "print version and exit")
	probeOnly := flag.Bool("probe", false, "list the file's tracks and exit")
	eventsFlag := flag.String("events", "", "emit machine-readable events on stderr: json")
	flag.Usage = func() { fmt.Fprint(proseErr, usage) }

	// After the definitions, so the scan can ask which flags take a value;
	// before Parse, which is the thing that can exit without the emitter.
	if eventsRequested(os.Args[1:], flag.CommandLine) {
		ev = events.New(proseErr)
		ev.Emit(&events.Version{Events: events.ProtocolVersion, Tool: version})
	}

	// ContinueOnError, so a usage error comes back here rather than exiting
	// inside the flag package: flag has already printed its message and the
	// usage text by then, which is the prose this keeps unchanged, and the exit
	// code stays the 2 ExitOnError used.
	flag.CommandLine.Init(os.Args[0], flag.ContinueOnError)
	// flag's own "flag provided but not defined" line is prose on stderr like
	// ours, so it goes through the same serialised writer.
	flag.CommandLine.SetOutput(proseErr)
	if err := flag.CommandLine.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// -h and -help: flag has already printed the usage text, and asking
			// for help is not a failure. ExitOnError exits 0 here, so this must
			// too — and like -version it ends with no terminal event.
			os.Exit(0)
		}
		ev.Emit(&events.Error{Kind: string(errOther), Message: err.Error()})
		os.Exit(2)
	}

	switch *eventsFlag {
	case "":
		// Disabled: Emit is a no-op, so nothing below needs a guard.
	case "json":
		if ev == nil {
			// Unreachable as long as the scan above reads argv the way Parse
			// does, which is the property its comment is about. Kept as the one
			// place that would notice if that ever stopped being true: an
			// honoured -events json without a stream would be worse than a
			// duplicate version line, and nothing has been emitted yet, so
			// version is still the first line.
			ev = events.New(proseErr)
			ev.Emit(&events.Version{Events: events.ProtocolVersion, Tool: version})
		}
	default:
		fatalf("invalid -events %q: the only supported value is json", *eventsFlag)
	}

	if *showVersion {
		fmt.Printf("sub-translator %s\n", version)
		return
	}

	mode, err := parseMode(*modeFlag)
	if err != nil {
		fatalf("%v", err)
	}

	source, err := parseSource(*sourceFlag)
	if err != nil {
		fatalf("%v", err)
	}

	// A config that cannot be read is not fatal here: every setting it holds has
	// a flag or a default. The audio path below loads it again and does fail, so
	// a broken file is still reported where it matters.
	settings := &config.Config{}
	if c, err := config.Load(); err == nil {
		settings = c
	}

	// The mode and chunk checks depend on nothing but the flags, so they belong
	// here rather than behind a container probe, a whisper lookup and an
	// interactive "this can take a while" prompt the user answers only to be
	// told the combination was never going to work.
	chunkMinutes := resolveChunkMinutes(*chunkMin, settings.Whisper.ChunkMinutes)
	if err := validateFastFlags(*fast, mode, chunkMinutes); err != nil {
		fatalf("%v", err)
	}

	if flag.NArg() < 1 {
		// The kind is "other": there is nothing a caller can fix in its
		// configuration, only in the argv it built — an empty path, or a file
		// that vanished between the drop and the launch. The prose and the exit
		// code are exactly what they were; what is new is that a consumer gets
		// a terminal event instead of 56 lines of flag documentation and
		// silence on the wire.
		//
		// This is the one fatal path that does not go through exitWith, and the
		// reason is the line it prints: a person with no input path needs the
		// usage text, not "error: no input file". exitWith would print the
		// latter, so the emit and the exit are spelled out here instead — the
		// same single error event, the same exit 1, arrived at by hand.
		ev.Emit(&events.Error{
			Kind:    string(errOther),
			Message: "no input file: sub-translator needs the path to a video file",
		})
		flag.Usage()
		os.Exit(1)
	}
	input := flag.Arg(0)
	if _, err := os.Stat(input); err != nil {
		fatalKindf(errProbeFailed, "input file not found: %s", input)
	}

	// -probe is what a wrapper calls to build a track list before the user has
	// chosen anything, so it returns here — before a key is required, before any
	// scratch directory is created, and before anything is written.
	if *probeOnly {
		streams, err := media.Probe(input)
		if err != nil {
			fatalKindf(errProbeFailed, "probe failed: %v", err)
		}
		emitProbe(input, streams)
		return
	}

	// The translation settings are settled for the same reason, and before any
	// work happens: a run with no API key has to be refused now, not discovered
	// after an hour of transcription. This covers the progressive path too — both
	// -fast and the ordinary run reach translation only through here.
	if *workers != -1 && *workers < 1 {
		fatalf("-workers must be at least 1, got %d", *workers)
	}
	translateOpts := resolveTranslateOptions(settings, *workers)
	if err := translateOpts.Validate(); err != nil {
		fatalKindf(errMissingKey, "%v", err)
	}

	setupRunTmpDir()
	defer cleanupRunTmpDir()

	if err := runOrdinary(ordinaryOptions{
		input:         input,
		tmpDir:        runTmpDir,
		from:          *from,
		to:            *to,
		source:        source,
		track:         *track,
		atrack:        *atrack,
		mode:          mode,
		out:           *out,
		fast:          *fast,
		chunkMinutes:  chunkMinutes,
		whisperModel:  *whisperModel,
		whisperBin:    *whisperBin,
		vadModel:      *vadModel,
		translateOpts: translateOpts,
	}); err != nil {
		exitWith(err)
	}
}

// fatalf reports a fatal error with no kind a caller can act on. It still
// emits exactly one error event — kind "other" — so a consumer never gets
// silence and a bare exit code; fatalKindf is the one other path to the same
// ending, and it emits its own kind instead, never this one, so a fatal run
// always emits exactly one error event, never two.
//
// It is errf plus exitWith, and nothing else: the emit, the prose line, the
// cleanup and the exit all live in exitWith, so the shape of a fatal run is
// described in one place — with one deliberate exception. The missing-input
// path above hand-rolls its own emit and exit because it must print the usage
// text where exitWith would print the "error: ..." line; it is the only other
// code that describes that shape, and it is annotated there.
func fatalf(format string, args ...any) {
	exitWith(errf(format, args...))
}

// classifyWhisperErr says which kind a resolveWhisper failure is. Its errors
// come back as plain strings — two of them formatted by internal/whisper,
// which this plan puts out of scope and so cannot be given exported sentinels
// — so this matches on the message prefix instead of errors.Is. Every error
// resolveWhisper can return is one of these four; the false return exists so a
// future addition there fails open to a plain, unclassified fatalf rather than
// silently mislabelling something new.
func classifyWhisperErr(err error) (errKind, bool) {
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "whisper binary not found at"),
		strings.HasPrefix(msg, "whisper-cli not found"):
		return errWhisperNotFound, true
	case strings.HasPrefix(msg, "whisper model not found:"),
		strings.HasPrefix(msg, "no whisper model found"):
		return errNoModel, true
	case strings.HasPrefix(msg, "several whisper models found"):
		return errAmbiguousModel, true
	default:
		return errOther, false
	}
}

// resolveWhisper assembles the whisper settings from flags, config and
// discovery, in that order of precedence, and fails with an actionable message
// when a required piece is missing.
func resolveWhisper(c *config.Config, modelFlag, binFlag, vadFlag string) (whisper.Options, error) {
	bin, err := whisper.FindBinary(firstNonEmpty(binFlag, c.Whisper.Bin))
	if err != nil {
		return whisper.Options{}, err
	}

	dirs := models.SearchDirs(c.Models.Dir)
	found, _ := models.Discover(dirs)

	model := firstNonEmpty(modelFlag, c.Whisper.Model)
	switch {
	case model != "":
		if _, err := os.Stat(model); err != nil {
			return whisper.Options{}, fmt.Errorf("whisper model not found: %s", model)
		}
	case len(found) == 1:
		model = found[0]
	case len(found) > 1:
		var b strings.Builder
		fmt.Fprintf(&b, "several whisper models found; pick one with -whisper-model "+
			"or `sub-translator config set whisper.model <path>`:\n")
		for _, m := range found {
			fmt.Fprintf(&b, "  %s\n", m)
		}
		return whisper.Options{}, errors.New(b.String())
	default:
		return whisper.Options{}, fmt.Errorf(
			"no whisper model found — download one (sub-translator model pull large-v3-turbo) " +
				"or set it: sub-translator config set whisper.model <path>")
	}

	// VAD is off unless explicitly asked for: measured against this same
	// model, it merges speech into long (10-20s) unpunctuated chunks and
	// measurably worsens transcription accuracy, which makes worse
	// subtitles, not better ones. It stays available via -vad-model or
	// config for material with long silent stretches where whisper's
	// decoder can otherwise loop.
	vad := firstNonEmpty(vadFlag, c.Whisper.VADModel)

	return whisper.Options{
		Bin:          bin,
		Model:        model,
		VADModel:     vad,
		VADThreshold: c.Whisper.VADThreshold,
		Threads:      c.Whisper.Threads,
		Language:     c.Whisper.Language,
	}, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// srtOutputPath is where the translated .srt will land: an explicit -out wins
// in srt mode, otherwise it is the default <video>.<lang>.srt next to the input.
// The transcript sidecar is compared against this, so the two sites must not
// drift.
func srtOutputPath(input, to, out string, mode outputMode) string {
	if mode == modeSRT && out != "" {
		return out
	}
	return media.DefaultSRTPath(input, to)
}

// newTranslateClient builds the client the run translates through. It is a
// variable so the end-to-end test can point a whole progressive run at an
// httptest server instead of the real endpoint; production never replaces it.
var newTranslateClient = func(o translate.Options) (translate.Translator, error) {
	return translate.NewTranslator(o)
}

// resolveTranslateOptions assembles the translation settings from flags, config
// and defaults, in that order of precedence — the same order -whisper-model and
// friends follow. The key is read from the environment first: an exported
// OPENAI_API_KEY is how a shared machine or a CI job supplies one without
// writing it to disk.
func resolveTranslateOptions(c *config.Config, workersFlag int) translate.Options {
	return translate.Options{
		APIKey: firstNonEmpty(os.Getenv("OPENAI_API_KEY"), c.Translate.APIKey),
		// Environment only, deliberately not config: this redirects every request
		// the run makes, so it belongs to one invocation rather than to a file a
		// user forgets they wrote. A persisted wrong endpoint fails in a way that
		// looks like the network rather than like a setting.
		BaseURL: os.Getenv("OPENAI_BASE_URL"),
		Model:   firstNonEmpty(c.Translate.Model, translate.DefaultModel),
		Workers: resolveWorkers(workersFlag, c.Translate.Workers),
	}
}

// resolveWorkers turns the -workers flag into a worker count. Only the sentinel
// -1 means "not given", so a 0 the user typed survives to be rejected instead of
// quietly becoming the default.
func resolveWorkers(flagValue, configValue int) int {
	if flagValue != -1 {
		return flagValue
	}
	if configValue > 0 {
		return configValue
	}
	return translate.DefaultWorkers
}

// withLangs is o with the languages this run settled on. In audio mode the source
// language is not known until whisper has reported it, so the options travel
// without it and the client is built once it is.
func withLangs(o translate.Options, from, to string) translate.Options {
	o.From, o.To = from, to
	return o
}

// describeProvider names what is about to be spent: which model, and how many
// batches go out at once. The defaults are filled in here as well as in the
// package, because the line is printed before the client is built and a run that
// says "openai , 0 workers" is a run nobody can check.
func describeProvider(o translate.Options) string {
	model := o.Model
	if model == "" {
		model = translate.DefaultModel
	}
	workers := o.Workers
	if workers < 1 {
		workers = translate.DefaultWorkers
	}
	if workers == 1 {
		return "openai " + model
	}
	return fmt.Sprintf("openai %s, %d workers", model, workers)
}

// errEmptyTranscript is what whisper producing nothing at all comes back as.
// The whole-file path treats it as fatal outright; the chunked path has to tell
// a silent stretch mid-film (ordinary — credits, a long establishing shot) from
// a silent film, so it needs to recognise this rather than match on a string.
var errEmptyTranscript = errors.New("transcript is empty — the audio track may be silent")

// classifyTranscribeErr says which kind a failure out of transcribe or
// runProgressive is, covering the two ways picking an audio track fails
// (pickAudioStream, in progressive.go, is unexported and returns plain
// errors, so this matches its message prefix rather than errors.Is), the
// one way transcription itself comes up empty, which does have a sentinel,
// and the two endpoint refusals internal/translate names for itself.
//
// A -fast run translates inside its chunk loop, so a translation failure
// reaches main through here rather than through classifyTranslateErr. It used
// to arrive as "other": the kind was known at the point of failure and thrown
// away in a fmt.Errorf, where no prefix matcher could recover it — which is
// why the wrapped sentinels are the thing being matched now.
//
// Everything else that can reach these paths — an extract, parse or whisper
// failure — is not one of the kinds a caller can act on, hence the false
// return: those stay a plain, unclassified fatalf.
func classifyTranscribeErr(err error) (errKind, bool) {
	switch {
	case errors.Is(err, errEmptyTranscript):
		return errTranscriptEmpty, true
	case errors.Is(err, translate.ErrUnauthorized):
		return errInvalidKey, true
	case errors.Is(err, translate.ErrRateLimited):
		return errRateLimited, true
	case strings.HasPrefix(err.Error(), "no audio tracks in"),
		strings.HasPrefix(err.Error(), "no audio stream at index"):
		return errNoAudioTrack, true
	default:
		return errOther, false
	}
}

// classifyTranslateErr names a translation failure the endpoint refused
// outright. The two refusals a caller can act on are different problems with
// different fixes — a wrong key is corrected in the settings, a rate limit is
// waited out — and both used to arrive as rate_limited, which sent a user who
// mistyped one character to a Retry button that could never succeed. Anything
// else the endpoint or the model does is not something a caller can route on,
// hence errOther.
func classifyTranslateErr(err error) errKind {
	switch {
	case errors.Is(err, translate.ErrUnauthorized):
		return errInvalidKey
	case errors.Is(err, translate.ErrRateLimited):
		return errRateLimited
	default:
		return errOther
	}
}

// transcribeInto runs whisper over one audio file and returns its blocks and the
// language actually used. Both the whole-file path and the chunked path go
// through here, so the rule about what whisper is told to hear lives in exactly
// one place.
func transcribeInto(opts whisper.Options, audio, outBase, lang string) ([]srt.Block, string, error) {
	opts.Audio = audio
	opts.OutBase = outBase
	opts.Language = lang

	result, err := whisper.Run(opts, func(pct int) {
		fmt.Printf("\r  progress: %d%%   ", pct)
		ev.Emit(&events.Progress{Pct: pct})
	})
	fmt.Println()
	if err != nil {
		return nil, "", err
	}

	blocks, err := srt.Parse(result.SRTPath)
	if err != nil {
		return nil, "", fmt.Errorf("parse transcript: %w", err)
	}
	if len(blocks) == 0 {
		return nil, result.Language, errEmptyTranscript
	}
	return blocks, result.Language, nil
}

// reportDetectedLanguage is main()'s reaction to what transcribe (or the first
// chunk of a -fast run) found: whisper's own language, when it differs from
// what was asked for, is worth telling both a person (the printed line) and a
// machine (the language event) about, and it is what everything downstream —
// the transcript sidecar's filename, the translation client — treats as the
// source language from here on. Factored out of main() as its own small
// function, not as part of the larger extraction of main()'s pipeline that is
// out of scope for this round, so a test can call it directly rather than
// re-deriving its two lines by hand.
func reportDetectedLanguage(from, lang string) string {
	if lang == "" || lang == from {
		return from
	}
	fmt.Printf("Detected language: %s\n", lang)
	ev.Emit(&events.Language{Detected: lang})
	return lang
}

// transcribe extracts one audio stream, runs whisper over it, and returns the
// parsed transcript together with the language whisper used. Its working files
// go in tmpDir, the caller's run-scoped scratch directory, so that a single
// owner removes them however the run ends: a feature film's 16 kHz WAV is over
// a gigabyte and must not be left behind.
func transcribe(input, tmpDir string, streams []media.Stream, atrack int, from string, opts whisper.Options) ([]srt.Block, string, error) {
	src, picked, err := pickAudioStream(input, streams, atrack, from)
	if err != nil {
		return nil, "", err
	}
	fmt.Printf("Audio:  #%d  lang=%s  %q\n", src.Index, src.Tags.Language, src.Tags.Title)

	wav := filepath.Join(tmpDir, "audio.wav")
	fmt.Printf("Extracting audio track #%d...\n", src.Index)
	ev.Emit(&events.Stage{Name: "extract"})
	if err := media.ExtractAudio(input, src.Index, wav); err != nil {
		return nil, "", fmt.Errorf("extract audio: %w", err)
	}

	fmt.Printf("Transcribing with %s...\n", filepath.Base(opts.Model))
	ev.Emit(&events.Stage{Name: "transcribe"})
	if opts.VADModel != "" {
		fmt.Printf("  VAD: %s\n", filepath.Base(opts.VADModel))
	}
	return transcribeInto(opts, wav, filepath.Join(tmpDir, "transcript"),
		resolveChunkLanguage(from, picked, opts.Language))
}

// progressLine is the ordinary run's printed translation progress, exactly as
// it goes to stdout. It is a function only so a test can read the number a
// person sees and hold it against the number the event carries: the two are
// computed from the same division but formatted in two places, and they diverged
// once — %.0f rounds halves to even while int() truncates, so 3 of 8 printed 37
// and emitted 38 — with the whole suite green. The format string is unchanged,
// so stdout is byte-for-byte what it was.
func progressLine(done, total int) string {
	return fmt.Sprintf("\r  progress: %d/%d (%.0f%%)   ",
		done, total, float64(done)/float64(total)*100)
}

// pctOf is what the printed progress line shows, as an integer. It goes
// through fmt's own formatting rather than math.Round because the event and
// the terminal have to agree on every value, and %.0f rounds halves to even
// where math.Round rounds them away from zero: 12.5% prints as 12 and would
// otherwise be emitted as 13. Both translate-progress callbacks (the
// whole-file path and every chunk of a -fast run) share this rather than each
// computing its own.
func pctOf(done, total int) int {
	p, _ := strconv.Atoi(fmt.Sprintf("%.0f", float64(done)/float64(total)*100))
	return p
}

// firstN is the first n values of v, unformatted: the actual indices, for a
// Warning event's First field. It is the one place this slicing happens, so
// the prose line a caller renders alongside a Warning event (with its own
// ellipsis, since only the caller knows whether it truncated) and the event's
// First field cannot drift apart the way two near-identical helpers could.
func firstN(v []int, n int) []int {
	if len(v) <= n {
		return v
	}
	return v[:n]
}

// probeEvent builds the complete Probe event for input's streams, duration
// included. Both the -probe path (emitProbe, below) and the ordinary run call
// this one function rather than each building the event its own way, which is
// what let the ordinary run's event drift out of sync with -probe's in the
// first place. subs is the caller's own media.SubtitleStreams(streams), taken
// as a parameter rather than recomputed here, since both callers already need
// that filtered list for their own purposes right next to this call.
func probeEvent(input string, streams, subs []media.Stream) *events.Probe {
	toEvent := func(list []media.Stream) []events.Stream {
		out := make([]events.Stream, 0, len(list))
		for _, s := range list {
			out = append(out, events.Stream{
				Index: s.Index,
				Lang:  s.Tags.Language,
				Title: s.Tags.Title,
				Codec: s.CodecName,
			})
		}
		return out
	}
	p := &events.Probe{
		Audio:     toEvent(media.AudioStreams(streams)),
		Subtitles: toEvent(subs),
	}
	// A duration we cannot read — missing, or ffprobe's own "N/A" — is not
	// worth failing over: the file is still translatable, and the field is
	// omitted rather than reported as zero.
	if d, err := media.ContainerDuration(input); err == nil {
		p.Duration = d.Seconds()
	}
	return p
}

// emitProbe reports what the file contains and nothing else. It is what a
// wrapper calls to build a track list before the user has chosen anything, so
// it must not need a key, a target language, or do any work.
func emitProbe(input string, streams []media.Stream) {
	subs := media.SubtitleStreams(streams)
	// Guarded for the reason the ordinary run's call site states: Go evaluates
	// Emit's argument before Emit's nil check runs, so an unguarded call here
	// spawns probeEvent's second ffprobe — for a duration the human output never
	// prints — on every -probe with the flag off. An off feature costs nothing,
	// and this was the last hole in that rule.
	if ev != nil {
		ev.Emit(probeEvent(input, streams, subs))
	}

	if len(subs) > 0 {
		fmt.Print(formatTracks(subs))
	} else {
		fmt.Printf("No subtitle tracks in %s.\n", filepath.Base(input))
	}
}
