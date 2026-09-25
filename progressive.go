package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/artschekoff/sub-translator/internal/media"
	"github.com/artschekoff/sub-translator/internal/srt"
	"github.com/artschekoff/sub-translator/internal/whisper"
)

// chunkPlan is one slice of the audio: where it starts in the film and how long
// it runs.
type chunkPlan struct {
	Index int
	Start time.Duration
	Dur   time.Duration
}

// planChunks divides a film into equal pieces, the last one short. Equal pieces
// are deliberate: the viewer's lead grows with every chunk, so by the time they
// reach minute ten the file already covers minute one hundred and twenty.
func planChunks(total, chunkLen time.Duration) []chunkPlan {
	if total <= 0 || chunkLen <= 0 {
		return nil
	}
	var out []chunkPlan
	for start, i := time.Duration(0), 0; start < total; start, i = start+chunkLen, i+1 {
		dur := chunkLen
		if start+dur > total {
			dur = total - start
		}
		out = append(out, chunkPlan{Index: i, Start: start, Dur: dur})
	}
	return out
}

// validateFastFlags rejects the -fast combinations that depend on nothing but
// the flags. They are split out from the source check so they can be raised
// before any work: -fast -mode mux used to probe the container, resolve whisper,
// ask "Transcribe the audio track? This can take a while", wait for the user to
// type y, and only then refuse. Both are a no-op when -fast is off, so the
// ordinary path is untouched.
func validateFastFlags(fast bool, mode outputMode, chunkMinutes int) error {
	if !fast {
		return nil
	}
	if mode != modeSRT {
		return fmt.Errorf("-fast cannot be combined with -mode %s: a video container "+
			"cannot be rewritten piece by piece — use the default -mode srt", mode)
	}
	if chunkMinutes < 1 {
		return fmt.Errorf("-chunk must be at least 1 minute, got %d", chunkMinutes)
	}
	return nil
}

// validateFastSource is the rest of the -fast validation: the part that cannot
// run until resolveSource has decided whether "auto" found a subtitle track.
func validateFastSource(fast bool, source sourceMode) error {
	if !fast {
		return nil
	}
	if source != sourceAudio {
		return fmt.Errorf("-fast only applies to transcription; there is nothing to wait for " +
			"when reading an existing subtitle track — use -source audio")
	}
	return nil
}

// resolveChunkMinutes turns the -chunk flag into the value validateFast sees.
// Only the sentinel -1 means "not given"; 0 and negatives must survive so the
// validation can reject them, which is exactly what a plain 0 default prevented.
func resolveChunkMinutes(flagValue, configValue int) int {
	if flagValue != -1 {
		return flagValue
	}
	if configValue > 0 {
		return configValue
	}
	return 10
}

// pickAudioStream chooses which audio track to transcribe: an explicit index
// first, then a language-tag match, then the first track. It also settles the
// language to hand whisper: an explicit -atrack or a matching -from passes
// through unchanged, but a -from that matches no track warns to stderr, falls
// back to the first track, and returns "" for the language instead — forcing
// -l <from> onto a track that is not tagged that way makes whisper emit
// fluent, confident nonsense in the wrong language rather than an error, and
// that nonsense then translates into a finished, entirely wrong subtitle file.
func pickAudioStream(input string, streams []media.Stream, atrack int, from string) (media.Stream, string, error) {
	audio := media.AudioStreams(streams)
	if len(audio) == 0 {
		return media.Stream{}, "", fmt.Errorf("no audio tracks in %s — nothing to transcribe", filepath.Base(input))
	}
	if atrack >= 0 {
		for _, s := range audio {
			if s.Index == atrack {
				return s, from, nil
			}
		}
		return media.Stream{}, "", fmt.Errorf("no audio stream at index %d", atrack)
	}
	if from != "" {
		if s, ok := media.FindAudioByLang(audio, from); ok {
			return s, from, nil
		}
		fmt.Fprintf(os.Stderr, "warning: no audio track tagged lang=%s; using #%d and letting whisper detect the language\n", from, audio[0].Index)
		return audio[0], "", nil
	}
	return audio[0], "", nil
}

// resolveChunkLanguage decides what language to hand whisper for every chunk.
//
// picked is what pickAudioStream returned: empty either because -from was never
// given, or because it was given and deliberately cleared when no audio track
// carried it. Those two cases must not be treated alike — falling back to the
// configured default in the second one would tell whisper to hear a language the
// track does not contain, right after warning the user we would not.
func resolveChunkLanguage(from, picked, configured string) string {
	if picked != "" {
		return picked
	}
	if from != "" {
		// -from was given but pickAudioStream cleared it: the user was already
		// warned that track doesn't carry that language, so the configured
		// default — which may be that very same language — must not slip back
		// in behind the warning.
		return ""
	}
	return configured
}

// notifyDone raises a desktop notification on macOS. It is best-effort by design:
// a finished file is the deliverable, and a missing osascript must never turn a
// successful run into a failed one.
func notifyDone(title, message string) {
	if runtime.GOOS != "darwin" {
		return
	}
	script := fmt.Sprintf("display notification %q with title %q", message, title)
	_ = exec.Command("osascript", "-e", script).Run()
}

// runProgressive transcribes and translates the film in order, rewriting the
// output file after every chunk so the opening is watchable while the rest is
// still being produced.
func runProgressive(input, tmpDir string, streams []media.Stream, atrack int, from, to string,
	chunkLen time.Duration, opts whisper.Options, out string) error {

	src, lang, err := pickAudioStream(input, streams, atrack, from)
	if err != nil {
		return err
	}
	fmt.Printf("Audio:  #%d  lang=%s  %q\n", src.Index, src.Tags.Language, src.Tags.Title)

	fullWAV := filepath.Join(tmpDir, "audio.wav")
	fmt.Printf("Extracting audio track #%d...\n", src.Index)
	if err := media.ExtractAudio(input, src.Index, fullWAV); err != nil {
		return fmt.Errorf("extract audio: %w", err)
	}

	total, err := media.WAVDuration(fullWAV)
	if err != nil {
		return err
	}
	plan := planChunks(total, chunkLen)
	if len(plan) == 0 {
		return fmt.Errorf("audio track is empty — nothing to transcribe")
	}
	fmt.Printf("Transcribing %s in %d chunks of up to %s...\n",
		formatClock(total), len(plan), formatClock(chunkLen))

	lang = resolveChunkLanguage(from, lang, opts.Language)

	// -fast is only reachable in srt mode, so the output path is the same one
	// the ordinary run writes.
	outPath := srtOutputPath(input, to, out, modeSRT)

	client := newTranslateClient(lang, to)
	var srcBlocks, outBlocks []srt.Block
	// Failed block numbers accumulate across the whole run in absolute,
	// post-merge numbering, because that is the only numbering the finished file
	// has. Per-chunk warnings alone scroll past behind fourteen chunks of
	// \r-rewritten progress, and the run then closes with an unqualified "Done".
	var failedAll []int
	// What outPath already covers, empty until the first successful write. Every
	// fatal error below carries it, because a bare "error:" reads as "nothing
	// was produced" while a perfectly good hour of subtitles sits next to the
	// film.
	covered := ""
	langMissingWarned := false
	sidecarSkipWarned := false

	// fail attaches the surviving coverage to an error, so no failure can leave
	// the user guessing whether the file next to the film is worth anything.
	fail := func(format string, args ...any) error {
		err := fmt.Errorf(format, args...)
		if covered == "" {
			return err
		}
		return fmt.Errorf("%w; %s already covers 0:00–%s", err, outPath, covered)
	}

	for _, c := range plan {
		chunkWAV := filepath.Join(tmpDir, fmt.Sprintf("chunk-%03d.wav", c.Index))
		if err := media.SliceWAV(fullWAV, c.Start, c.Dur, chunkWAV); err != nil {
			return fail("chunk %d: slice audio: %w", c.Index+1, err)
		}

		fmt.Printf("Chunk %d/%d (%s–%s):\n", c.Index+1, len(plan),
			formatClock(c.Start), formatClock(c.Start+c.Dur))
		// The language is settled by the first chunk and then forced, so whisper
		// cannot change its mind halfway and hand back a two-language file.
		parsed, detected, err := transcribeInto(opts, chunkWAV,
			filepath.Join(tmpDir, fmt.Sprintf("chunk-%03d", c.Index)), lang)
		// The slice has done its job the moment whisper has read it. Keeping
		// every chunk to the end of the run roughly doubles peak scratch usage,
		// on top of a full film's WAV that is already over a gigabyte.
		os.Remove(chunkWAV)
		switch {
		case errors.Is(err, errEmptyTranscript) && c.Index > 0:
			// Silence mid-film is ordinary — credits, a long wordless sequence.
			// The chunk simply contributes nothing.
		case err != nil:
			return fail("chunk %d (%s): %w", c.Index+1, formatClock(c.Start), err)
		}
		switch {
		case lang != "":
			// Already settled; nothing to announce.
		case detected != "":
			lang = detected
			fmt.Printf("Detected language: %s\n", lang)
			// The client was built before anything was known about the audio,
			// so its source language is only settled now — otherwise the first
			// chunk translates with an empty sl for the whole call.
			client.From = lang
		case !langMissingWarned:
			// whisper.Run falls back to the *requested* language when its JSON
			// sidecar is missing or unparseable, and the requested value is
			// empty here — so there is nothing to settle and nothing to force.
			// Say so once rather than printing a blank detection line per chunk
			// and building a "movie..srt" sidecar out of the empty code.
			langMissingWarned = true
			fmt.Fprintln(os.Stderr, "warning: whisper reported no detected language; "+
				"every chunk will detect its own and no source transcript will be saved — "+
				"pass -from to settle it")
		}

		shifted, err := srt.Shift(parsed, c.Start)
		if err != nil {
			return fail("chunk %d: %w", c.Index+1, err)
		}
		srcBlocks = append(srcBlocks, shifted...)

		// The transcript is saved before translation is even attempted: it is
		// by far the most expensive step here, and a translation failure below
		// must never throw away CPU time already spent transcribing.
		if transcriptPath := media.DefaultSRTPath(input, lang); lang == "" {
			// No language, no filename: "movie..srt" is not a transcript path.
		} else if transcriptPath == outPath {
			// Source and target are the same language, so the sidecar and the
			// translation want the same file. The sidecar goes in first, which
			// would leave untranslated source text under the translated name if
			// the translation then failed — the one artifact this must not make.
			if !sidecarSkipWarned {
				sidecarSkipWarned = true
				fmt.Fprintf(os.Stderr, "warning: not saving the %s transcript to %s: "+
					"that is where the translated subtitles go\n", lang, transcriptPath)
			}
		} else if err := srt.Write(transcriptPath, srt.Renumber(srcBlocks)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not save transcript to %s: %v\n", transcriptPath, err)
		}

		// A -fast run is watched while it happens, so minutes of silence between
		// the transcription bar and the next "extended to" line read as a hang —
		// longer still when the endpoint is throttling and every batch retries.
		translated, failed, err := client.TranslateAll(srt.Texts(shifted), func(done, total int) {
			fmt.Printf("\r  translating: %d/%d   ", done, total)
		})
		fmt.Println()
		if err != nil {
			return fail("chunk %d: translate: %w", c.Index+1, err)
		}
		if len(failed) > 0 {
			fmt.Fprintf(os.Stderr,
				"warning: chunk %d: %d of %d blocks kept their original text\n",
				c.Index+1, len(failed), len(shifted))
		}
		base := len(outBlocks)
		for _, f := range failed {
			failedAll = append(failedAll, base+f)
		}

		chunkOut, err := srt.WithTexts(shifted, translated)
		if err != nil {
			return fail("chunk %d: %w", c.Index+1, err)
		}
		outBlocks = append(outBlocks, chunkOut...)

		if err := srt.Write(outPath, srt.Renumber(outBlocks)); err != nil {
			return fail("chunk %d: write %s: %w", c.Index+1, outPath, err)
		}

		covered = formatClock(c.Start + c.Dur)
		if c.Index == 0 {
			fmt.Printf("Saved SRT: %s — covers 0:00–%s, you can start watching\n", outPath, covered)
		} else {
			fmt.Printf("  extended to %s\n", covered)
		}
	}

	if len(srcBlocks) == 0 {
		// Every chunk was silent. Without this the run would end by announcing
		// that a file holding a single newline covers the whole film.
		return errEmptyTranscript
	}

	if len(failedAll) > 0 {
		fmt.Printf("Done: %s covers the full %s, but %d of %d blocks could not be "+
			"translated and kept their original text (first: %v) — reload subtitles in your player\n",
			outPath, formatClock(total), len(failedAll), len(outBlocks), firstFew(failedAll, 3))
		notifyDone("Subtitles ready, with gaps",
			fmt.Sprintf("%s — %d blocks untranslated", filepath.Base(outPath), len(failedAll)))
		return nil
	}

	fmt.Printf("Done: %s covers the full %s — reload subtitles in your player\n",
		outPath, formatClock(total))
	notifyDone("Subtitles ready", filepath.Base(outPath))
	return nil
}

// formatClock renders a duration as H:MM:SS, which is how a viewer reads a film's
// position.
func formatClock(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	return fmt.Sprintf("%d:%02d:%02d", h, m, d/time.Second)
}
