package main

import (
	"strings"
	"testing"
	"time"
)

func TestPlanChunksEvenDivision(t *testing.T) {
	got := planChunks(30*time.Minute, 10*time.Minute)
	if len(got) != 3 {
		t.Fatalf("got %d chunks, want 3: %+v", len(got), got)
	}
	for i, c := range got {
		if c.Index != i {
			t.Errorf("chunk %d has Index %d", i, c.Index)
		}
		if c.Start != time.Duration(i)*10*time.Minute {
			t.Errorf("chunk %d starts at %v", i, c.Start)
		}
		if c.Dur != 10*time.Minute {
			t.Errorf("chunk %d runs %v, want 10m", i, c.Dur)
		}
	}
}

// A film is never an exact multiple of the chunk length; the tail must be short
// rather than reading past the end of the audio.
func TestPlanChunksShortTail(t *testing.T) {
	got := planChunks(25*time.Minute, 10*time.Minute)
	if len(got) != 3 {
		t.Fatalf("got %d chunks, want 3", len(got))
	}
	last := got[2]
	if last.Start != 20*time.Minute || last.Dur != 5*time.Minute {
		t.Errorf("tail = start %v dur %v, want 20m/5m", last.Start, last.Dur)
	}
	var sum time.Duration
	for _, c := range got {
		sum += c.Dur
	}
	if sum != 25*time.Minute {
		t.Errorf("chunks cover %v, want exactly 25m", sum)
	}
}

func TestPlanChunksShorterThanOneChunk(t *testing.T) {
	got := planChunks(3*time.Minute, 10*time.Minute)
	if len(got) != 1 {
		t.Fatalf("got %d chunks, want 1", len(got))
	}
	if got[0].Start != 0 || got[0].Dur != 3*time.Minute {
		t.Errorf("chunk = start %v dur %v, want 0/3m", got[0].Start, got[0].Dur)
	}
}

func TestPlanChunksDegenerateInputs(t *testing.T) {
	if got := planChunks(0, 10*time.Minute); len(got) != 0 {
		t.Errorf("zero total = %v, want none", got)
	}
	if got := planChunks(10*time.Minute, 0); len(got) != 0 {
		t.Errorf("zero chunk length = %v, want none", got)
	}
	if got := planChunks(-time.Minute, time.Minute); len(got) != 0 {
		t.Errorf("negative total = %v, want none", got)
	}
}

// -fast only makes sense when there is a slow step to hide. A subtitle track is
// already on disk.
func TestValidateFastRejectsSubtitleSource(t *testing.T) {
	err := validateFast(true, sourceSub, modeSRT, 10)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "-source audio") {
		t.Errorf("error %q does not say what to do instead", err)
	}
}

// A container cannot be rewritten incrementally, so the progressive promise
// cannot be kept in mux modes.
func TestValidateFastRejectsContainerModes(t *testing.T) {
	for _, m := range []outputMode{modeMux, modeBoth} {
		err := validateFast(true, sourceAudio, m, 10)
		if err == nil {
			t.Errorf("mode %v: want an error", m)
			continue
		}
		if !strings.Contains(err.Error(), "-mode srt") {
			t.Errorf("mode %v: error %q does not name the working mode", m, err)
		}
	}
}

func TestValidateFastRejectsBadChunkLength(t *testing.T) {
	for _, n := range []int{0, -5} {
		if err := validateFast(true, sourceAudio, modeSRT, n); err == nil {
			t.Errorf("chunk %d: want an error", n)
		}
	}
}

func TestValidateFastAcceptsTheWorkingCombination(t *testing.T) {
	if err := validateFast(true, sourceAudio, modeSRT, 10); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// Without -fast nothing is validated, because nothing changes.
func TestValidateFastIgnoresEverythingWhenOff(t *testing.T) {
	for _, m := range []outputMode{modeSRT, modeMux, modeBoth} {
		for _, s := range []sourceMode{sourceSub, sourceAudio} {
			if err := validateFast(false, s, m, 0); err != nil {
				t.Errorf("fast=false must never error, got %v", err)
			}
		}
	}
}

// resolveChunkMinutes is what stands between the -chunk flag and validateFast. A
// plain 0 default there would make "-chunk 0" indistinguishable from omitting the
// flag, silently handing the user a 10-minute chunk instead of the error
// validateFast promises — so only the -1 sentinel may be treated as "not given";
// 0 and every other value must pass through untouched for validateFast to see.
func TestResolveChunkMinutes(t *testing.T) {
	tests := []struct {
		name        string
		flagValue   int
		configValue int
		want        int
	}{
		{"unset, no config, falls back to 10", -1, 0, 10},
		{"unset, config set, uses config", -1, 5, 5},
		{"explicit zero survives for validateFast to reject", 0, 5, 0},
		{"explicit negative survives for validateFast to reject", -2, 5, -2},
		{"explicit value wins over config", 7, 5, 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveChunkMinutes(tt.flagValue, tt.configValue); got != tt.want {
				t.Errorf("resolveChunkMinutes(%d, %d) = %d, want %d",
					tt.flagValue, tt.configValue, got, tt.want)
			}
		})
	}
}
