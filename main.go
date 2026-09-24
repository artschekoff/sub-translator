package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/artschekoff/sub-translator/internal/config"
	"github.com/artschekoff/sub-translator/internal/media"
	"github.com/artschekoff/sub-translator/internal/models"
	"github.com/artschekoff/sub-translator/internal/srt"
	"github.com/artschekoff/sub-translator/internal/translate"
	"github.com/artschekoff/sub-translator/internal/whisper"
)

// version is injected at build time via -ldflags "-X main.version=..."
var version = "dev"

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
	if needsConfirm {
		fmt.Printf("No subtitle tracks in %s.\n", filepath.Base(input))
		if !confirm("Transcribe the audio track with whisper? This can take a while") {
			fatalf("nothing to do")
		}
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
		cfg, err := config.Load()
		if err != nil {
			fatalf("%v", err)
		}
		opts, err := resolveWhisper(cfg, *whisperModel, *whisperBin, *vadModel)
		if err != nil {
			fatalf("%v", err)
		}

		var lang string
		blocks, lang, err = transcribe(input, streams, *atrack, *from, opts)
		if err != nil {
			fatalf("%v", err)
		}
		if *from == "" {
			*from = lang
			fmt.Printf("Detected language: %s\n", lang)
		}

		// Transcription is by far the most expensive step here; keeping its
		// output costs one small file and saves repeating it.
		transcriptPath := media.DefaultSRTPath(input, *from)
		if err := srt.Write(transcriptPath, blocks); err != nil {
			fatalf("write transcript: %v", err)
		}
		fmt.Printf("Saved transcript: %s\n", transcriptPath)
	} else {
		if len(subs) == 0 {
			fatalf("no subtitle tracks in %s — nothing to translate", filepath.Base(input))
		}

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

		tmpSRT := filepath.Join(os.TempDir(), "sub_translator_src.srt")
		defer os.Remove(tmpSRT)
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
	translated, err := client.TranslateAll(texts, func(done, total int) {
		pct := float64(done) / float64(total) * 100
		fmt.Printf("\r  progress: %d/%d (%.0f%%)   ", done, total, pct)
	})
	fmt.Println()
	if err != nil {
		fatalf("translate: %v", err)
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

	tmpTranslated := filepath.Join(os.TempDir(), "sub_translator_dst.srt")
	defer os.Remove(tmpTranslated)

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
	found, vads := models.Discover(dirs)

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

	// VAD is optional: whisper works without it, but on a feature film it keeps
	// the decoder from looping over long silences.
	vad := firstNonEmpty(vadFlag, c.Whisper.VADModel)
	if vad == "" && len(vads) > 0 {
		vad = vads[0]
	}

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
// parsed transcript together with the language whisper used. Temporary files
// live in a directory of their own: a feature film's 16 kHz WAV is over a
// gigabyte and must not be left behind.
func transcribe(input string, streams []media.Stream, atrack int, from string, opts whisper.Options) ([]srt.Block, string, error) {
	audio := media.AudioStreams(streams)
	if len(audio) == 0 {
		return nil, "", fmt.Errorf("no audio tracks in %s — nothing to transcribe", filepath.Base(input))
	}

	var src media.Stream
	switch {
	case atrack >= 0:
		found := false
		for _, s := range audio {
			if s.Index == atrack {
				src, found = s, true
				break
			}
		}
		if !found {
			return nil, "", fmt.Errorf("no audio stream at index %d", atrack)
		}
	case from != "":
		if s, ok := media.FindAudioByLang(audio, from); ok {
			src = s
		} else {
			src = audio[0]
		}
	default:
		src = audio[0]
	}
	fmt.Printf("Audio:  #%d  lang=%s  %q\n", src.Index, src.Tags.Language, src.Tags.Title)

	tmpDir, err := os.MkdirTemp("", "sub-translator-*")
	if err != nil {
		return nil, "", fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	wav := filepath.Join(tmpDir, "audio.wav")
	fmt.Printf("Extracting audio track #%d...\n", src.Index)
	if err := media.ExtractAudio(input, src.Index, wav); err != nil {
		return nil, "", fmt.Errorf("extract audio: %w", err)
	}

	opts.Audio = wav
	opts.OutBase = filepath.Join(tmpDir, "transcript")
	if from != "" {
		opts.Language = from
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
