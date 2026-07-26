package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/artschekoff/sub-translator/internal/media"
	"github.com/artschekoff/sub-translator/internal/srt"
	"github.com/artschekoff/sub-translator/internal/translate"
)

// version is injected at build time via -ldflags "-X main.version=..."
var version = "dev"

const usage = `sub-translator — subtitle translator for MKV, MP4, AVI and more

Usage:
  sub-translator [flags] <input>

Output modes (-mode):
  srt   write a translated .srt next to the input video (default)
  mux   write a new video file with the translated track embedded
  both  write the muxed video file and the .srt

Supported formats:
  MKV, MP4/M4V/MOV  — translated track embedded into output file
  AVI               — embedded subs not supported; -mode mux/both fall back to srt

Flags:
  -from    source language code (default: en)
  -to      target language code (required; prompted for if omitted)
  -track   subtitle stream index, -1 = auto-detect by -from lang (default: -1)
  -mode    output mode: srt, mux or both (default: srt)
  -out     output path (.srt in srt mode, container otherwise)
  -version print version and exit

Examples:
  sub-translator -to es movie.mkv
  sub-translator -to fr -mode mux movie.mp4
  sub-translator -to de -mode both movie.mkv
  sub-translator -to ru -track 3 movie.mkv
`

func main() {
	from := flag.String("from", "en", "source language")
	to := flag.String("to", "", "target language (required)")
	track := flag.Int("track", -1, "subtitle stream index (-1 = auto)")
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

	if flag.NArg() < 1 {
		flag.Usage()
		os.Exit(1)
	}
	input := flag.Arg(0)
	if _, err := os.Stat(input); err != nil {
		fatalf("input file not found: %s", input)
	}

	if *to = strings.TrimSpace(*to); *to == "" {
		*to = promptTargetLang()
	}

	// Detect subtitle track
	fmt.Printf("Probing %s...\n", filepath.Base(input))
	streams, err := media.Probe(input)
	if err != nil {
		fatalf("probe failed: %v", err)
	}

	subs := media.SubtitleStreams(streams)
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
			fmt.Fprintf(os.Stderr, "available subtitle tracks:\n")
			for _, s := range subs {
				fmt.Fprintf(os.Stderr, "  #%d  lang=%-6s  %s\n", s.Index, s.Tags.Language, s.Tags.Title)
			}
			fatalf("no subtitle track found for lang=%s (use -track to specify)", *from)
		}
	}
	fmt.Printf("Source: #%d  lang=%s  %q\n", srcStream.Index, srcStream.Tags.Language, srcStream.Tags.Title)

	// Extract
	tmpSRT := filepath.Join(os.TempDir(), "sub_translator_src.srt")
	defer os.Remove(tmpSRT)
	fmt.Printf("Extracting subtitle track #%d...\n", srcStream.Index)
	if err := media.ExtractSubtitle(input, srcStream.Index, tmpSRT); err != nil {
		fatalf("extract: %v", err)
	}

	// Parse
	blocks, err := srt.Parse(tmpSRT)
	if err != nil {
		fatalf("parse SRT: %v", err)
	}
	if len(blocks) == 0 {
		fatalf("subtitle track #%d is empty — nothing to translate", srcStream.Index)
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

// promptTargetLang asks for the target language on stdin. -to has no default:
// silently translating to some arbitrary language is worse than asking.
func promptTargetLang() string {
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("Target language code (e.g. es, fr, de, ja): ")
		line, err := reader.ReadString('\n')
		if lang := strings.TrimSpace(line); lang != "" {
			return lang
		}
		if err != nil {
			// Non-interactive stdin (pipe, CI) — nothing left to read.
			fmt.Println()
			fatalf("-to is required: no target language given")
		}
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
