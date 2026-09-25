package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/artschekoff/sub-translator/internal/config"
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
// fatalf's os.Exit and a SIGINT — both have to be able to remove it, and a
// feature film's 16 kHz WAV is well over a gigabyte to leave behind.
var runTmpDir string

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

Flags:
  -from          source language code (prompted for if omitted; detected in audio mode)
  -to            target language code (required; prompted for if omitted)
  -source        subtitle source: sub, audio or auto (default: auto)
  -track         subtitle stream index, -1 = auto-detect by -from lang (default: -1)
  -atrack        audio stream index, -1 = auto (default: -1)
  -mode          output mode: srt, mux or both (default: srt)
  -out           output path (.srt in srt mode, container otherwise)
  -whisper-model path to a whisper ggml model
  -whisper-bin   path to the whisper-cli binary
  -vad-model     path to a Silero VAD model
  -version       print version and exit

Examples:
  sub-translator -to es movie.mkv
  sub-translator -to es -source audio movie.mkv
  sub-translator -to fr -mode mux movie.mp4
  sub-translator -to ru -track 3 movie.mkv
  sub-translator config set whisper.model ~/models/ggml-large-v3-turbo.bin
  sub-translator model pull large-v3-turbo
`

func main() {
	if handled, err := dispatchSubcommand(os.Args[1:], os.Stdout); handled {
		if err != nil {
			fatalf("%v", err)
		}
		return
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
	fast := flag.Bool("fast", false, "translate progressively so the opening is watchable within a minute")
	chunkMin := flag.Int("chunk", 0, "chunk length in minutes for -fast (default 10, or whisper.chunk-minutes)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

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

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}
	input := flag.Arg(0)
	if _, err := os.Stat(input); err != nil {
		fatalf("input file not found: %s", input)
	}

	setupRunTmpDir()
	defer cleanupRunTmpDir()

	// Detect subtitle track
	fmt.Printf("Probing %s...\n", filepath.Base(input))
	streams, err := media.Probe(input)
	if err != nil {
		fatalf("probe failed: %v", err)
	}

	subs := media.SubtitleStreams(streams)
	if len(subs) > 0 {
		// Many releases ship untagged tracks, and the listing is what tells you
		// which -track to pass.
		fmt.Print(formatTracks(subs))
	}

	source, needsConfirm, err := resolveSource(source, len(subs) > 0, stdinIsTerminal())
	if err != nil {
		fatalf("%v", err)
	}

	// Whisper is resolved before anything is asked of the user: a missing
	// binary or model is a prerequisite failure, and making someone answer a
	// scary confirmation and type a language code first only to be told
	// whisper-cli is not installed wastes their time.
	var whisperOpts whisper.Options
	if source == sourceAudio {
		cfg, err := config.Load()
		if err != nil {
			fatalf("%v", err)
		}
		whisperOpts, err = resolveWhisper(cfg, *whisperModel, *whisperBin, *vadModel)
		if err != nil {
			fatalf("%v", err)
		}
	}

	if needsConfirm {
		fmt.Printf("No subtitle tracks in %s.\n", filepath.Base(input))
		// stdinIsTerminal cannot tell a real terminal from /dev/null, because
		// /dev/null is itself a character device, so the terminal check can
		// report interactive when nobody is there to answer. The actionable
		// message here must not depend on that guess being right.
		ok, err := readConfirm(stdin, os.Stderr, fmt.Sprintf(
			"Transcribe the audio track with %s? This can take a while",
			filepath.Base(whisperOpts.Model)))
		if err != nil {
			fatalf("no subtitle tracks, and the transcription prompt got no answer; " +
				"pass -source audio to transcribe the audio track")
		}
		if !ok {
			fatalf("nothing to do")
		}
	}

	if source == sourceSub && len(subs) == 0 {
		fatalf("no subtitle tracks in %s — nothing to translate", filepath.Base(input))
	}

	chunkMinutes := *chunkMin
	if chunkMinutes == 0 {
		if cfg, err := config.Load(); err == nil && cfg.Whisper.ChunkMinutes > 0 {
			chunkMinutes = cfg.Whisper.ChunkMinutes
		} else {
			chunkMinutes = 10
		}
	}
	if err := validateFast(*fast, source, mode, chunkMinutes); err != nil {
		fatalf("%v", err)
	}

	*from = strings.TrimSpace(*from)
	*to = strings.TrimSpace(*to)

	// In audio mode whisper detects the source language, so only the target is
	// genuinely required.
	if *from == "" && source == sourceSub {
		*from = promptLang("Source")
	}
	if *to == "" {
		*to = promptLang("Target")
	}

	var blocks []srt.Block

	if source == sourceAudio {
		if *fast {
			if err := runProgressive(input, runTmpDir, streams, *atrack, *from, *to,
				time.Duration(chunkMinutes)*time.Minute, whisperOpts); err != nil {
				fatalf("%v", err)
			}
			return
		}

		var lang string
		blocks, lang, err = transcribe(input, runTmpDir, streams, *atrack, *from, whisperOpts)
		if err != nil {
			fatalf("%v", err)
		}
		// The language whisper actually worked in wins over the one that was
		// asked for: transcribe drops -from when no track carries that tag, and
		// translating from a language the transcript is not in would be worse
		// than useless.
		if lang != "" && lang != *from {
			fmt.Printf("Detected language: %s\n", lang)
			*from = lang
		}

		// Transcription is by far the most expensive step here; keeping its
		// output costs one small file. A media library on a NAS share or a
		// read-only mount makes this write fail routinely, and throwing away an
		// hour of CPU over a sidecar file would be indefensible — so it warns
		// and carries on to the translation the user actually asked for.
		transcriptPath := media.DefaultSRTPath(input, *from)
		if err := srt.Write(transcriptPath, blocks); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not save transcript to %s: %v\n", transcriptPath, err)
		} else {
			fmt.Printf("Saved transcript: %s\n", transcriptPath)
		}
	} else {
		var srcStream media.Stream
		if *track >= 0 {
			found := false
			for _, s := range subs {
				if s.Index == *track {
					srcStream = s
					found = true
					break
				}
			}
			if !found {
				fatalf("no subtitle stream at index %d", *track)
			}
		} else {
			var ok bool
			srcStream, ok = media.FindSubtitleByLang(subs, *from)
			if !ok {
				fatalf("no subtitle track tagged lang=%s — pick one from the list above with -track", *from)
			}
		}
		fmt.Printf("Source: #%d  lang=%s  %q\n", srcStream.Index, srcStream.Tags.Language, srcStream.Tags.Title)

		tmpSRT := filepath.Join(runTmpDir, "source.srt")
		fmt.Printf("Extracting subtitle track #%d...\n", srcStream.Index)
		if err := media.ExtractSubtitle(input, srcStream.Index, tmpSRT); err != nil {
			fatalf("extract: %v", err)
		}

		blocks, err = srt.Parse(tmpSRT)
		if err != nil {
			fatalf("parse SRT: %v", err)
		}
		if len(blocks) == 0 {
			fatalf("subtitle track #%d is empty — nothing to translate", srcStream.Index)
		}
	}
	fmt.Printf("Parsed %d subtitle blocks\n", len(blocks))

	// Translate
	fmt.Printf("Translating %s → %s...\n", *from, *to)
	client := translate.New(*from, *to)
	texts := srt.Texts(blocks)
	translated, failedBlocks, err := client.TranslateAll(texts, func(done, total int) {
		pct := float64(done) / float64(total) * 100
		fmt.Printf("\r  progress: %d/%d (%.0f%%)   ", done, total, pct)
	})
	fmt.Println()
	if err != nil {
		fatalf("translate: %v", err)
	}
	if len(failedBlocks) > 0 {
		fmt.Fprintf(os.Stderr,
			"warning: %d of %d blocks could not be translated and kept their original text (first: %v)\n",
			len(failedBlocks), len(texts), firstFew(failedBlocks, 3))
	}

	// Rebuild blocks
	outBlocks, err := srt.WithTexts(blocks, translated)
	if err != nil {
		fatalf("rebuild: %v", err)
	}

	// AVI and friends can't embed subtitle tracks — fall back to a sidecar.
	mode, note := resolveMode(mode, media.ContainerSupportsEmbeddedSubs(input))
	if note != "" {
		fmt.Println(note)
	}

	// Inside the run-scoped directory, not a fixed name in $TMPDIR: two runs on
	// two different films would otherwise share the file, and a mux could pick
	// up the other run's subtitles.
	tmpTranslated := filepath.Join(runTmpDir, "translated.srt")

	if mode.writesSRT() {
		srtOut := media.DefaultSRTPath(input, *to)
		if mode == modeSRT && *out != "" {
			srtOut = *out
		}
		if err := srt.Write(srtOut, outBlocks); err != nil {
			fatalf("write SRT: %v", err)
		}
		fmt.Printf("Saved SRT: %s\n", srtOut)
	}

	if !mode.writesContainer() {
		fmt.Println("Done.")
		return
	}

	if err := srt.Write(tmpTranslated, outBlocks); err != nil {
		fatalf("write temp SRT: %v", err)
	}

	// Mux
	outMKV := *out
	if outMKV == "" {
		outMKV = media.DefaultOutputPath(input, *to)
	}
	langTitle := map[string]string{
		"es": "Spanish", "fr": "French", "de": "German",
		"it": "Italian", "pt": "Portuguese", "ru": "Russian",
		"zh": "Chinese", "ja": "Japanese", "ko": "Korean",
		"ar": "Arabic", "pl": "Polish", "nl": "Dutch",
	}
	title := langTitle[*to]
	if title == "" {
		title = *to
	}

	fmt.Printf("Muxing → %s...\n", filepath.Base(outMKV))
	if err := media.MuxSubtitle(input, tmpTranslated, *to, title, outMKV); err != nil {
		fatalf("mux: %v", err)
	}
	fmt.Printf("Done: %s\n", outMKV)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	// os.Exit skips deferred calls, so the scratch directory has to be removed
	// here or every error path leaks it.
	cleanupRunTmpDir()
	os.Exit(1)
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

// transcribe extracts one audio stream, runs whisper over it, and returns the
// parsed transcript together with the language whisper used. Its working files
// go in tmpDir, the caller's run-scoped scratch directory, so that a single
// owner removes them however the run ends: a feature film's 16 kHz WAV is over
// a gigabyte and must not be left behind.
func transcribe(input, tmpDir string, streams []media.Stream, atrack int, from string, opts whisper.Options) ([]srt.Block, string, error) {
	audio := media.AudioStreams(streams)
	if len(audio) == 0 {
		return nil, "", fmt.Errorf("no audio tracks in %s — nothing to transcribe", filepath.Base(input))
	}

	// detect records that a requested language had to be abandoned, so the
	// choice to let whisper detect one survives down to the options below.
	detect := false
	if atrack < 0 && from != "" {
		if _, ok := media.FindAudioByLang(audio, from); !ok {
			// Falling back to the first track is right — untagged audio is
			// common — but forcing -l <from> onto it is not: whisper given the
			// wrong language emits fluent nonsense in that language, which then
			// translates into a finished, confidently wrong subtitle file.
			fmt.Fprintf(os.Stderr, "warning: no audio track tagged lang=%s; using #%d and letting whisper detect the language\n", from, audio[0].Index)
			from = ""
			detect = true
		}
	}

	src, err := pickAudioStream(input, streams, atrack, from)
	if err != nil {
		return nil, "", err
	}
	fmt.Printf("Audio:  #%d  lang=%s  %q\n", src.Index, src.Tags.Language, src.Tags.Title)

	wav := filepath.Join(tmpDir, "audio.wav")
	fmt.Printf("Extracting audio track #%d...\n", src.Index)
	if err := media.ExtractAudio(input, src.Index, wav); err != nil {
		return nil, "", fmt.Errorf("extract audio: %w", err)
	}

	opts.Audio = wav
	opts.OutBase = filepath.Join(tmpDir, "transcript")
	switch {
	case from != "":
		opts.Language = from
	case detect:
		// The requested language was dropped above; a language left over from
		// config would be just as wrong for this track, so whisper decides.
		opts.Language = ""
	}

	fmt.Printf("Transcribing with %s...\n", filepath.Base(opts.Model))
	if opts.VADModel != "" {
		fmt.Printf("  VAD: %s\n", filepath.Base(opts.VADModel))
	}
	result, err := whisper.Run(opts, func(pct int) {
		fmt.Printf("\r  progress: %d%%   ", pct)
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
		return nil, "", fmt.Errorf("transcript is empty — the audio track may be silent")
	}
	return blocks, result.Language, nil
}

// firstFew renders the first n values of a list for an error message, with a
// trailing ellipsis when there are more.
func firstFew(v []int, n int) string {
	if len(v) <= n {
		return fmt.Sprint(v)
	}
	return fmt.Sprint(v[:n]) + "…"
}
