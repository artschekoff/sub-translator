package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/artschekoff/sub-translator/internal/config"
	"github.com/artschekoff/sub-translator/internal/events"
	"github.com/artschekoff/sub-translator/internal/media"
	"github.com/artschekoff/sub-translator/internal/srt"
	"github.com/artschekoff/sub-translator/internal/translate"
	"github.com/artschekoff/sub-translator/internal/whisper"
)

// ordinaryOptions is one field for every value the default pipeline used to
// read out of a flag pointer. It mirrors what runProgressive takes, so the two
// paths read alike and neither reaches for a flag variable behind the other's
// back.
//
// It is not the whole of the run's input, and a caller should not read it as
// such: on the -source audio path runOrdinary still calls config.Load() itself
// and hands the result to resolveWhisper, so the whisper binary, model and VAD
// settings come off disk ($XDG_CONFIG_HOME, else $HOME) rather than through
// this struct. That is what main did before the extraction and it is left
// alone here, because the load is deliberately late — it is the one that fails
// loudly on a broken config file, where main's own earlier load ignores the
// error. The visible cost is that a test driving this path has to set
// XDG_CONFIG_HOME to stop the developer's own settings deciding the answer;
// ordinary_test.go does. Threading the config through as a field would make
// the struct honest and is the obvious next move, but it changes which load
// reports a broken file, so it is a change of behaviour rather than a comment.
type ordinaryOptions struct {
	// input is the video file, and tmpDir the run-scoped scratch directory its
	// working files go in. tmpDir is passed rather than read off runTmpDir for
	// the reason runProgressive takes it as a parameter: the caller owns it and
	// removes it however the run ends.
	input  string
	tmpDir string

	// from is empty until it is known: in audio mode whisper reports it, and in
	// subtitle mode a person is asked for it.
	from string
	to   string

	// source is the parsed -source flag, before resolveSource has decided what
	// "auto" means for this particular file.
	source sourceMode
	track  int
	atrack int

	// mode is the parsed -mode flag, before resolveMode has had a chance to fall
	// back to a sidecar for a container that cannot embed subtitles.
	mode outputMode
	out  string

	// fast and chunkMinutes belong to the progressive path, which is reached
	// from inside this one: only resolveSource knows whether "auto" found a
	// subtitle track, so the fork cannot be made before the probe.
	fast         bool
	chunkMinutes int

	whisperModel string
	whisperBin   string
	vadModel     string

	translateOpts translate.Options
}

// runOrdinary is the whole run after the flags are settled: probe, extract or
// read subtitles, transcribe, translate, write, and optionally mux. All 24 of
// the failures it used to exit on are returned instead, each carrying a kind —
// exitWith, in main, is the one place one of them becomes an exit.
//
// One path out of here still ends the process rather than returning, and a
// test has to know about it: promptLang, called below for a missing -from or
// -to, exits when the prompt gets no answer. So runOrdinary is drivable by a
// test that supplies both languages, and a test that leaves one empty with a
// closed stdin will lose its process instead of seeing an error. That is not a
// gap in this extraction so much as one left outside it: promptLang lives in
// ui.go, was never one of main's 34 exits, and converting it means changing
// its signature and every other caller's. It is the honest next step, and it
// is a task of its own. Nothing about the error-event contract is affected
// meanwhile — that path emits exactly once, through fatalf into exitWith.
//
// The -fast fork lives in here rather than in main because it cannot be decided
// any earlier: whether "auto" means transcription is a question only
// resolveSource can answer, and resolveSource needs the probe.
func runOrdinary(o ordinaryOptions) error {
	input, from, to := o.input, o.from, o.to
	mode := o.mode

	// Detect subtitle track
	fmt.Printf("Probing %s...\n", filepath.Base(input))
	streams, err := media.Probe(input)
	if err != nil {
		return kindErrf(errProbeFailed, "probe failed: %v", err)
	}
	subs := media.SubtitleStreams(streams)
	// probeEvent's own work — a second ffprobe call for the duration — must
	// not happen at all when nothing is listening: Go evaluates the argument
	// before Emit's nil check runs, so calling this unconditionally would
	// spawn that subprocess on every ordinary run, -events json or not.
	if ev != nil {
		ev.Emit(probeEvent(input, streams, subs))
	}

	if len(subs) > 0 {
		// Many releases ship untagged tracks, and the listing is what tells you
		// which -track to pass.
		fmt.Print(formatTracks(subs))
	}

	source, needsConfirm, err := resolveSource(o.source, len(subs) > 0, stdinIsTerminal())
	if err != nil {
		return errf("%v", err)
	}

	// The one -fast check that genuinely needs resolveSource to have run: only
	// now is it known whether "auto" found a subtitle track.
	if err := validateFastSource(o.fast, source); err != nil {
		return errf("%v", err)
	}

	// Whisper is resolved before anything is asked of the user: a missing
	// binary or model is a prerequisite failure, and making someone answer a
	// scary confirmation and type a language code first only to be told
	// whisper-cli is not installed wastes their time.
	var whisperOpts whisper.Options
	if source == sourceAudio {
		cfg, err := config.Load()
		if err != nil {
			return errf("%v", err)
		}
		whisperOpts, err = resolveWhisper(cfg, o.whisperModel, o.whisperBin, o.vadModel)
		if err != nil {
			if kind, ok := classifyWhisperErr(err); ok {
				return kindErrf(kind, "%v", err)
			}
			return errf("%v", err)
		}
	}

	if needsConfirm {
		fmt.Printf("No subtitle tracks in %s.\n", filepath.Base(input))
		// stdinIsTerminal cannot tell a real terminal from /dev/null, because
		// /dev/null is itself a character device, so the terminal check can
		// report interactive when nobody is there to answer. The actionable
		// message here must not depend on that guess being right.
		ok, err := readConfirm(stdin, proseErr, fmt.Sprintf(
			"Transcribe the audio track with %s? This can take a while",
			filepath.Base(whisperOpts.Model)))
		if err != nil {
			return errf("no subtitle tracks, and the transcription prompt got no answer; " +
				"pass -source audio to transcribe the audio track")
		}
		if !ok {
			return errf("nothing to do")
		}
	}

	if source == sourceSub && len(subs) == 0 {
		return kindErrf(errNoSubtitleTrack, "no subtitle tracks in %s — nothing to translate", filepath.Base(input))
	}

	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)

	// In audio mode whisper detects the source language, so only the target is
	// genuinely required.
	if from == "" && source == sourceSub {
		from = promptLang("Source")
	}
	if to == "" {
		to = promptLang("Target")
	}

	var blocks []srt.Block

	if source == sourceAudio {
		if o.fast {
			if err := runProgressive(input, o.tmpDir, streams, o.atrack, from, to,
				time.Duration(o.chunkMinutes)*time.Minute, whisperOpts, o.translateOpts, o.out); err != nil {
				if kind, ok := classifyTranscribeErr(err); ok {
					return kindErrf(kind, "%v", err)
				}
				return errf("%v", err)
			}
			return nil
		}

		var lang string
		blocks, lang, err = transcribe(input, o.tmpDir, streams, o.atrack, from, whisperOpts)
		if err != nil {
			if kind, ok := classifyTranscribeErr(err); ok {
				return kindErrf(kind, "%v", err)
			}
			return errf("%v", err)
		}
		// The language whisper actually worked in wins over the one that was
		// asked for: transcribe drops -from when no track carries that tag, and
		// translating from a language the transcript is not in would be worse
		// than useless.
		from = reportDetectedLanguage(from, lang)

		// Transcription is by far the most expensive step here; keeping its
		// output costs one small file. A media library on a NAS share or a
		// read-only mount makes this write fail routinely, and throwing away an
		// hour of CPU over a sidecar file would be indefensible — so it warns
		// and carries on to the translation the user actually asked for.
		transcriptPath := media.DefaultSRTPath(input, from)
		if transcriptPath == srtOutputPath(input, to, o.out, mode) {
			// Source and target are the same language — reachable by mis-guessing
			// a film's language, or by -to en on an English film — so the sidecar
			// and the translation want the same file. The sidecar is written
			// first, so a fatal translate error below would leave untranslated
			// source text under the translated filename: precisely the artifact
			// this branch exists to make impossible.
			fmt.Fprintf(proseErr, "warning: not saving the %s transcript to %s: "+
				"that is where the translated subtitles go\n", from, transcriptPath)
		} else if err := srt.Write(transcriptPath, blocks); err != nil {
			fmt.Fprintf(proseErr, "warning: could not save transcript to %s: %v\n", transcriptPath, err)
		} else {
			fmt.Printf("Saved transcript: %s\n", transcriptPath)
		}
	} else {
		var srcStream media.Stream
		if o.track >= 0 {
			found := false
			for _, s := range subs {
				if s.Index == o.track {
					srcStream = s
					found = true
					break
				}
			}
			if !found {
				return kindErrf(errNoSubtitleTrack, "no subtitle stream at index %d", o.track)
			}
		} else {
			var ok bool
			srcStream, ok = media.FindSubtitleByLang(subs, from)
			if !ok {
				return kindErrf(errNoSubtitleTrack, "no subtitle track tagged lang=%s — pick one from the list above with -track", from)
			}
		}
		fmt.Printf("Source: #%d  lang=%s  %q\n", srcStream.Index, srcStream.Tags.Language, srcStream.Tags.Title)

		tmpSRT := filepath.Join(o.tmpDir, "source.srt")
		fmt.Printf("Extracting subtitle track #%d...\n", srcStream.Index)
		// The whole pre-translate phase of a -source sub run. Without this the
		// stream goes probe → stage translate and a window has nothing to show
		// for the extraction and the SRT parse.
		ev.Emit(&events.Stage{Name: "extract-subs"})
		// No progress callback here: pulling one subtitle track out of a
		// container is a text demux, not the minutes-long decode ExtractAudio
		// and MuxSubtitle can be, and TestOrdinaryRunSpawnsFfprobeOnlyWhenEventsAreOn
		// pins that an off feature (nothing listening for progress here) must
		// not cost a ContainerDuration probe it has no use for.
		if err := media.ExtractSubtitle(input, srcStream.Index, tmpSRT, nil); err != nil {
			return errf("extract: %v", err)
		}

		blocks, err = srt.Parse(tmpSRT)
		if err != nil {
			return errf("parse SRT: %v", err)
		}
		if len(blocks) == 0 {
			return errf("subtitle track #%d is empty — nothing to translate", srcStream.Index)
		}
	}
	fmt.Printf("Parsed %d subtitle blocks\n", len(blocks))

	// Translate
	client, err := newTranslateClient(withLangs(o.translateOpts, from, to))
	if err != nil {
		return errf("%v", err)
	}
	fmt.Printf("Translating %s → %s via %s...\n", from, to, describeProvider(o.translateOpts))
	ev.Emit(&events.Stage{Name: "translate"})
	texts := srt.Texts(blocks)
	translated, failedBlocks, err := client.TranslateAll(texts, func(done, total int) {
		fmt.Print(progressLine(done, total))
		ev.Emit(&events.Progress{Pct: pctOf(done, total)})
	})
	fmt.Println()
	if err != nil {
		// translateAll returns an error only when every block failed: the
		// endpoint refused the whole run rather than a few blocks. Which refusal
		// it was is the error's to say, not this call site's — hard-coding
		// errRateLimited here is what reported a mistyped key as a rate limit.
		return kindErrf(classifyTranslateErr(err), "translate: %v", err)
	}
	if len(failedBlocks) > 0 {
		first := firstN(failedBlocks, 3)
		firstStr := fmt.Sprint(first)
		if len(failedBlocks) > len(first) {
			firstStr += "…"
		}
		fmt.Fprintf(proseErr,
			"warning: %d of %d blocks could not be translated and kept their original text (first: %s)\n",
			len(failedBlocks), len(texts), firstStr)
		ev.Emit(&events.Warning{
			Kind:  "blocks_untranslated",
			Count: len(failedBlocks),
			Total: len(texts),
			First: first,
		})
	}

	// Rebuild blocks
	outBlocks, err := srt.WithTexts(blocks, translated)
	if err != nil {
		return errf("rebuild: %v", err)
	}

	// AVI and friends can't embed subtitle tracks — fall back to a sidecar.
	mode, note := resolveMode(mode, media.ContainerSupportsEmbeddedSubs(input))
	if note != "" {
		fmt.Println(note)
	}

	// Inside the run-scoped directory, not a fixed name in $TMPDIR: two runs on
	// two different films would otherwise share the file, and a mux could pick
	// up the other run's subtitles.
	tmpTranslated := filepath.Join(o.tmpDir, "translated.srt")

	var srtOut string
	if mode.writesSRT() {
		srtOut = srtOutputPath(input, to, o.out, mode)
		if err := srt.Write(srtOut, outBlocks); err != nil {
			return kindErrf(errOutputUnwritable, "write SRT: %v", err)
		}
		fmt.Printf("Saved SRT: %s\n", srtOut)
	}

	if !mode.writesContainer() {
		fmt.Println("Done.")
		// !writesContainer() implies writesSRT() (mode.go), so srtOut was set
		// just above — the file the "Done." line refers to.
		emitDone(&events.Done{Path: srtOut})
		return nil
	}

	if err := srt.Write(tmpTranslated, outBlocks); err != nil {
		return errf("write temp SRT: %v", err)
	}

	// Mux
	outMKV := o.out
	if outMKV == "" {
		outMKV = media.DefaultOutputPath(input, to)
	}
	langTitle := map[string]string{
		"es": "Spanish", "fr": "French", "de": "German",
		"it": "Italian", "pt": "Portuguese", "ru": "Russian",
		"zh": "Chinese", "ja": "Japanese", "ko": "Korean",
		"ar": "Arabic", "pl": "Polish", "nl": "Dutch",
	}
	title := langTitle[to]
	if title == "" {
		title = to
	}

	fmt.Printf("Muxing → %s...\n", filepath.Base(outMKV))
	// Rewriting a multi-gigabyte container takes minutes, during which the last
	// thing a consumer heard was translate at 100%.
	ev.Emit(&events.Stage{Name: "mux"})
	if err := media.MuxSubtitle(input, tmpTranslated, to, title, outMKV, muxProgress()); err != nil {
		return kindErrf(errOutputUnwritable, "mux: %v", err)
	}
	fmt.Printf("Done: %s\n", outMKV)
	emitDone(&events.Done{Path: outMKV})
	return nil
}
