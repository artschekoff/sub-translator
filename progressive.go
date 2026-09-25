package main

import (
	"fmt"
	"time"
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
