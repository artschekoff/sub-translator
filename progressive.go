package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

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

// validateFast rejects the combinations where a progressive run cannot deliver
// what it promises. It is a no-op when -fast is off, so the ordinary path is
// untouched.
func validateFast(fast bool, source sourceMode, mode outputMode, chunkMinutes int) error {
	if !fast {
		return nil
	}
	if source != sourceAudio {
		return fmt.Errorf("-fast only applies to transcription; there is nothing to wait for " +
			"when reading an existing subtitle track — use -source audio")
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

// pickAudioStream chooses which audio track to transcribe: an explicit index
// first, then a language-tag match, then the first track. A -from that matches no
// track falls back with a warning and clears the language, so whisper detects it
// rather than being told to hear a language that is not there.
func pickAudioStream(input string, streams []media.Stream, atrack int, from string) (media.Stream, error) {
	audio := media.AudioStreams(streams)
	if len(audio) == 0 {
		return media.Stream{}, fmt.Errorf("no audio tracks in %s — nothing to transcribe", filepath.Base(input))
	}
	if atrack >= 0 {
		for _, s := range audio {
			if s.Index == atrack {
				return s, nil
			}
		}
		return media.Stream{}, fmt.Errorf("no audio stream at index %d", atrack)
	}
	if from != "" {
		if s, ok := media.FindAudioByLang(audio, from); ok {
			return s, nil
		}
	}
	return audio[0], nil
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
	chunkLen time.Duration, opts whisper.Options) error {

	src, err := pickAudioStream(input, streams, atrack, from)
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

	client := translate.New(from, to)
	var srcBlocks, outBlocks []srt.Block
	lang := from

	for _, c := range plan {
		chunkWAV := filepath.Join(tmpDir, fmt.Sprintf("chunk-%03d.wav", c.Index))
		if err := media.SliceWAV(fullWAV, c.Start, c.Dur, chunkWAV); err != nil {
			return fmt.Errorf("chunk %d: slice audio: %w", c.Index+1, err)
		}

		chunkOpts := opts
		chunkOpts.Audio = chunkWAV
		chunkOpts.OutBase = filepath.Join(tmpDir, fmt.Sprintf("chunk-%03d", c.Index))
		// The language is settled by the first chunk and then forced, so whisper
		// cannot change its mind halfway and hand back a two-language file.
		chunkOpts.Language = lang

		result, err := whisper.Run(chunkOpts, func(pct int) {
			fmt.Printf("\r  chunk %d/%d: %d%%   ", c.Index+1, len(plan), pct)
		})
		fmt.Println()
		if err != nil {
			return fmt.Errorf("chunk %d (%s): %w", c.Index+1, formatClock(c.Start), err)
		}
		if lang == "" {
			lang = result.Language
			fmt.Printf("Detected language: %s\n", lang)
		}

		parsed, err := srt.Parse(result.SRTPath)
		if err != nil {
			return fmt.Errorf("chunk %d: parse transcript: %w", c.Index+1, err)
		}
		shifted, err := srt.Shift(parsed, c.Start)
		if err != nil {
			return fmt.Errorf("chunk %d: %w", c.Index+1, err)
		}
		srcBlocks = append(srcBlocks, shifted...)

		translated, failed, err := client.TranslateAll(srt.Texts(shifted), nil)
		if err != nil {
			return fmt.Errorf("chunk %d: translate: %w", c.Index+1, err)
		}
		if len(failed) > 0 {
			fmt.Fprintf(os.Stderr,
				"warning: chunk %d: %d of %d blocks kept their original text\n",
				c.Index+1, len(failed), len(shifted))
		}
		chunkOut, err := srt.WithTexts(shifted, translated)
		if err != nil {
			return fmt.Errorf("chunk %d: %w", c.Index+1, err)
		}
		outBlocks = append(outBlocks, chunkOut...)

		transcriptPath := media.DefaultSRTPath(input, lang)
		if err := srt.Write(transcriptPath, srt.Renumber(srcBlocks)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not save transcript to %s: %v\n", transcriptPath, err)
		}
		outPath := media.DefaultSRTPath(input, to)
		if err := srt.Write(outPath, srt.Renumber(outBlocks)); err != nil {
			return fmt.Errorf("chunk %d: write %s: %w", c.Index+1, outPath, err)
		}

		covered := formatClock(c.Start + c.Dur)
		if c.Index == 0 {
			fmt.Printf("Saved SRT: %s — covers 0:00–%s, you can start watching\n", outPath, covered)
		} else {
			fmt.Printf("  extended to %s\n", covered)
		}
	}

	outPath := media.DefaultSRTPath(input, to)
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
