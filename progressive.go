package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"github.com/artschekoff/sub-translator/internal/events"
	"github.com/artschekoff/sub-translator/internal/media"
	"github.com/artschekoff/sub-translator/internal/srt"
	"github.com/artschekoff/sub-translator/internal/translate"
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

// fastSourceModes, fastOutputModes and fastMinChunkMins are the fast-mode rule.
// The validators below read them, and so does the probe event, so a caller can
// be told the rule instead of reimplementing it — and the rule cannot be
// changed in one place only. TestTheStatedRuleIsTheEnforcedRule holds all three
// together, the chunk floor included.
var (
	fastSourceModes  = []sourceMode{sourceAudio}
	fastOutputModes  = []outputMode{modeSRT}
	fastMinChunkMins = 1
)

// fastRule states the rule as data, for the probe event. The names come from
// each mode's String method, not from a conversion: both types are ints backed
// by iota, so string(m) would yield a control character rather than "srt".
func fastRule() *events.FastRule {
	r := &events.FastRule{MinChunkMinutes: fastMinChunkMins}
	for _, s := range fastSourceModes {
		r.Sources = append(r.Sources, s.String())
	}
	for _, m := range fastOutputModes {
		r.Modes = append(r.Modes, m.String())
	}
	return r
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
	if !slices.Contains(fastOutputModes, mode) {
		return fmt.Errorf("-fast cannot be combined with -mode %s: a video container "+
			"cannot be rewritten piece by piece — use the default -mode srt", mode)
	}
	if chunkMinutes < fastMinChunkMins {
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
	if !slices.Contains(fastSourceModes, source) {
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
		fmt.Fprintf(proseErr, "warning: no audio track tagged lang=%s; using #%d and letting whisper detect the language\n", from, audio[0].Index)
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

// runningTexts returns the first "done" entries of translated, treating any
// remaining empty strings as not-yet-translated and returning the source text
// placeholder "…" for them. The caller supplies the chunk size so the window
// does not see more lines than actually exist.
func runningTexts(translated []string, chunkSize int) []string {
	if len(translated) == 0 {
		return nil
	}
	out := make([]string, 0, chunkSize)
	for i := 0; i < chunkSize && i < len(translated); i++ {
		if translated[i] == "" {
			out = append(out, "…")
		} else {
			out = append(out, translated[i])
		}
	}
	return out
}

// emitCaptionPreview sends the last completed block plus its neighbours to the
// event stream so the Run screen can render a scrolling ticker. Source is always
// present; Translation is empty until the block has finished translating.
func emitCaptionPreview(blocks []srt.Block, translated []string, absoluteOffset int) {
	if len(translated) == 0 || ev == nil {
		return
	}
	current := len(translated) - 1 + absoluteOffset
	start := current - 2
	if start < 0 {
		start = 0
	}
	end := current + 3
	if end > absoluteOffset+len(blocks) {
		end = absoluteOffset + len(blocks)
	}
	if start >= end {
		return
	}
	for i := start; i < end; i++ {
		idx := i - absoluteOffset
		if idx < 0 || idx >= len(blocks) {
			continue
		}
		src := blocks[idx].Text
		tr := ""
		if idx < len(translated) {
			tr = translated[idx]
		}
		ev.Emit(&events.Caption{
			Index:       i,
			Source:      src,
			Translation: tr,
		})
	}
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
	chunkLen time.Duration, opts whisper.Options, topts translate.Options, out string) error {

	src, lang, err := pickAudioStream(input, streams, atrack, from)
	if err != nil {
		return err
	}
	fmt.Printf("Audio:  #%d  lang=%s  %q\n", src.Index, src.Tags.Language, src.Tags.Title)

	fullWAV := filepath.Join(tmpDir, "audio.wav")
	fmt.Printf("Extracting audio track #%d...\n", src.Index)
	ev.Emit(&events.Stage{Name: "extract"})
	if err := media.ExtractAudio(input, src.Index, fullWAV, extractionProgress()); err != nil {
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

	client, err := newTranslateClient(withLangs(topts, lang, to))
	if err != nil {
		return err
	}
	// Said once, here, for the same reason the ordinary path says it: -fast is
	// what people reach for on long films, which are the expensive ones, and
	// nothing else in this loop names what is being spent.
	fmt.Printf("Translating via %s...\n", describeProvider(topts))
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
		// Before the slice, not after it. A progress event carries no stage tag
		// of its own — a consumer reads it against the most recent stage — so a
		// slice that reported while this chunk's stage was still unannounced
		// attributed its burst to whatever came before: extract for chunk 1, and
		// the *previous* chunk's translate for the rest. A window showing "the
		// current stage in words, a progress bar" then resets the bar to 0 and
		// climbs it again under a stale label, with nothing on the wire to say
		// the burst does not belong there.
		//
		// The printed "Chunk n/N" line deliberately stays below the slice, where
		// it has always been, so stdout is byte-for-byte what it was.
		ev.Emit(&events.Stage{
			Name:  "transcribe",
			Chunk: c.Index + 1,
			Of:    len(plan),
			From:  formatClock(c.Start),
			To:    formatClock(c.Start + c.Dur),
		})
		if err := media.SliceWAV(fullWAV, c.Start, c.Dur, chunkWAV, sliceProgress()); err != nil {
			return fail("chunk %d: slice audio: %w", c.Index+1, err)
		}

		fmt.Printf("Chunk %d/%d (%s–%s):\n", c.Index+1, len(plan),
			formatClock(c.Start), formatClock(c.Start+c.Dur))
		// The language is settled by the first chunk and then forced, so whisper
		// cannot change its mind halfway and hand back a two-language file.
		parsed, detected, err := transcribeInto(opts, chunkWAV,
			filepath.Join(tmpDir, fmt.Sprintf("chunk-%03d", c.Index)), lang, len(srcBlocks))
		// The slice has done its job the moment whisper has read it. Keeping
		// every chunk to the end of the run roughly doubles peak scratch usage,
		// on top of a full film's WAV that is already over a gigabyte.
		os.Remove(chunkWAV)
		switch {
		case errors.Is(err, errEmptyTranscript):
			// Silence is ordinary anywhere in a film — an overture, a title
			// sequence, a wordless stretch mid-reel — and the opening is no
			// exception. The chunk simply contributes nothing. Refusing here
			// would cost the user the entire film over a quiet first ten
			// minutes; whether the film has any speech at all is a question
			// only the whole run can answer, and it is answered after the loop.
		case err != nil:
			return fail("chunk %d (%s): %w", c.Index+1, formatClock(c.Start), err)
		}
		switch {
		case lang != "":
			// Already settled; nothing to announce.
		case detected != "":
			lang = detected
			fmt.Printf("Detected language: %s\n", lang)
			ev.Emit(&events.Language{Detected: lang})
			// The client was built before anything was known about the audio,
			// so its source language is only settled now — otherwise the first
			// chunk translates with an empty source language for the whole call.
			// The options have already been validated, so this cannot newly fail;
			// if it somehow did, carrying on with the old client is better than
			// losing the film.
			if c, err := newTranslateClient(withLangs(topts, lang, to)); err == nil {
				client = c
			}
		case !langMissingWarned:
			// whisper.Run falls back to the *requested* language when its JSON
			// sidecar is missing or unparseable, and the requested value is
			// empty here — so there is nothing to settle and nothing to force.
			// Say so once rather than printing a blank detection line per chunk
			// and building a "movie..srt" sidecar out of the empty code.
			langMissingWarned = true
			fmt.Fprintln(proseErr, "warning: whisper reported no detected language; "+
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
				fmt.Fprintf(proseErr, "warning: not saving the %s transcript to %s: "+
					"that is where the translated subtitles go\n", lang, transcriptPath)
			}
		} else if err := srt.Write(transcriptPath, srt.Renumber(srcBlocks)); err != nil {
			fmt.Fprintf(proseErr, "warning: could not save transcript to %s: %v\n", transcriptPath, err)
		}

		ev.Emit(&events.Stage{Name: "translate", Chunk: c.Index + 1, Of: len(plan)})
		// A -fast run is watched while it happens, so minutes of silence between
		// the transcription bar and the next "extended to" line read as a hang —
		// longer still when the endpoint is throttling and every batch retries.
		translated, failed, err := client.TranslateAll(srt.Texts(shifted), func(done, total int) {
			fmt.Printf("\r  translating: %d/%d   ", done, total)
			ev.Emit(&events.Progress{Pct: pctOf(done, total)})
		})
		// The last few translated lines replace what the ticker showed of them
		// as heard. The results only exist once TranslateAll returns, so this is
		// once per chunk.
		running := runningTexts(translated, len(shifted))
		emitCaptionPreview(shifted, running, len(srcBlocks)-len(shifted))
		fmt.Println()
		if err != nil {
			return fail("chunk %d: translate: %w", c.Index+1, err)
		}
		if len(failed) > 0 {
			fmt.Fprintf(proseErr,
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
		if len(outBlocks) == 0 {
			// Nothing has been heard yet — a silent overture, a title sequence.
			// An empty file is not something to announce as ready to watch, and
			// it covers nothing, so a failure in the next chunk must not claim
			// it does.
			continue
		}

		if err := srt.Write(outPath, srt.Renumber(outBlocks)); err != nil {
			// outPath is srtOutputPath's answer — the user's own -out, or the
			// default sidecar path next to their film — never a scratch file, so
			// a failure writing it is one they can fix by choosing somewhere
			// else. kindErrf's message is %v-formatted, so wrapping it here with
			// %w leaves this error's text exactly what it was.
			return fail("chunk %d: write %s: %w", c.Index+1, outPath, kindErrf(errOutputUnwritable, "%v", err))
		}

		// Whether this is the first watchable file, rather than whether this is
		// the first chunk: a silent opening means the two are not the same.
		firstWrite := covered == ""
		covered = formatClock(c.Start + c.Dur)
		if firstWrite {
			fmt.Printf("Saved SRT: %s — covers 0:00–%s, you can start watching\n", outPath, covered)
		} else {
			fmt.Printf("  extended to %s\n", covered)
		}
		ev.Emit(&events.Saved{Path: outPath, Covers: covered, Watchable: true})
	}

	if len(srcBlocks) == 0 {
		// Every chunk was silent, so the whole audio track is. Without this the
		// run would end by announcing that a file holding a single newline
		// covers the whole film. This is the guard; the per-chunk one above
		// deliberately is not.
		return fail("%w", errEmptyTranscript)
	}

	if len(failedAll) > 0 {
		first := firstN(failedAll, 3)
		firstStr := fmt.Sprint(first)
		if len(failedAll) > len(first) {
			firstStr += "…"
		}
		fmt.Printf("Done: %s covers the full %s, but %d of %d blocks could not be "+
			"translated and kept their original text (first: %s) — reload subtitles in your player\n",
			outPath, formatClock(total), len(failedAll), len(outBlocks), firstStr)
		ev.Emit(&events.Warning{
			Kind:  "blocks_untranslated",
			Count: len(failedAll),
			Total: len(outBlocks),
			First: first,
		})
		emitDone(&events.Done{Path: outPath, Covers: formatClock(total)})
		notifyDone("Subtitles ready, with gaps",
			fmt.Sprintf("%s — %d blocks untranslated", filepath.Base(outPath), len(failedAll)))
		return nil
	}

	fmt.Printf("Done: %s covers the full %s — reload subtitles in your player\n",
		outPath, formatClock(total))
	emitDone(&events.Done{Path: outPath, Covers: formatClock(total)})
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
