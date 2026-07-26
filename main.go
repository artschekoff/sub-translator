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

Supported formats:
  MKV, MP4/M4V/MOV  — translated track embedded into output file
  AVI               — embedded subs not supported; SRT saved externally

Flags:
  -from   source language code (default: en)
  -to     target language code (required; prompted for if omitted)
  -track  subtitle stream index, -1 = auto-detect by -from lang (default: -1)
  -srt    also save translated .srt alongside the output file
  -no-mux skip muxing, only save translated .srt
  -out    output file path (default: input.<TO><ext>)
  -version print version and exit

Examples:
  sub-translator movie.mkv
  sub-translator -from en -to fr movie.mp4
  sub-translator -srt movie.mkv
  sub-translator -no-mux movie.avi
`

func main() {
	from := flag.String("from", "en", "source language")
	to := flag.String("to", "", "target language (required)")
	track := flag.Int("track", -1, "subtitle stream index (-1 = auto)")
	saveSRT := flag.Bool("srt", false, "save translated .srt alongside MKV")
	noMux := flag.Bool("no-mux", false, "skip muxing back into MKV")
	out := flag.String("out", "", "output MKV path")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	flag.Parse()

	if *showVersion {
		fmt.Printf("sub-translator %s\n", version)
		return
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

	// AVI can't embed subtitle tracks — force SRT-only output
	aviMode := !media.ContainerSupportsEmbeddedSubs(input)
	if aviMode {
		*saveSRT = true
		*noMux = true
	}

	// Determine SRT output path
	srtOut := media.DefaultSRTPath(input, *to)
	tmpTranslated := filepath.Join(os.TempDir(), "sub_translator_dst.srt")
	defer os.Remove(tmpTranslated)

	writePath := tmpTranslated
	if *saveSRT || *noMux {
		writePath = srtOut
	}
	if err := srt.Write(writePath, outBlocks); err != nil {
		fatalf("write SRT: %v", err)
	}
	if *saveSRT || *noMux {
		fmt.Printf("Saved SRT: %s\n", srtOut)
	}

	if aviMode {
		fmt.Println("Note: AVI container doesn't support embedded subtitles. SRT saved externally.")
	}

	if *noMux {
		fmt.Println("Done (mux skipped).")
		return
	}

	// Also write SRT to temp if we wrote to srtOut above
	if writePath == srtOut {
		if err := srt.Write(tmpTranslated, outBlocks); err != nil {
			fatalf("write temp SRT: %v", err)
		}
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
