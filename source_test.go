package main

import (
	"strings"
	"testing"
)

func TestParseSource(t *testing.T) {
	tests := []struct {
		in      string
		want    sourceMode
		wantErr bool
	}{
		{"auto", sourceAuto, false},
		{"sub", sourceSub, false},
		{"audio", sourceAudio, false},
		{"AUDIO", sourceAudio, false},
		{"  sub  ", sourceSub, false},
		{"", 0, true},
		{"whisper", 0, true},
	}
	for _, tt := range tests {
		got, err := parseSource(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseSource(%q): want error, got %v", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSource(%q): unexpected error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseSource(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestParseSourceErrorNamesValidValues(t *testing.T) {
	_, err := parseSource("bogus")
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"sub", "audio", "auto", "bogus"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestSourceModeString(t *testing.T) {
	for _, tt := range []struct {
		mode sourceMode
		want string
	}{
		{sourceAuto, "auto"},
		{sourceSub, "sub"},
		{sourceAudio, "audio"},
	} {
		if got := tt.mode.String(); got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
}

// Existing subtitles are free; transcription costs many minutes of full-load
// CPU. auto therefore always prefers a subtitle track when there is one.
func TestResolveSourceAutoPrefersExistingSubtitles(t *testing.T) {
	got, confirm, err := resolveSource(sourceAuto, true, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != sourceSub {
		t.Errorf("auto with subtitles = %v, want sub", got)
	}
	if confirm {
		t.Error("reading an existing subtitle track needs no confirmation")
	}
}

// Falling into a multi-minute transcription unannounced is a bad surprise, so
// auto asks first.
func TestResolveSourceAutoAsksBeforeTranscribing(t *testing.T) {
	got, confirm, err := resolveSource(sourceAuto, false, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != sourceAudio {
		t.Errorf("auto without subtitles = %v, want audio", got)
	}
	if !confirm {
		t.Error("want a confirmation before transcribing")
	}
}

// With no terminal there is nobody to answer the question, and silently
// starting an hour of work in a script is worse than failing loudly.
func TestResolveSourceAutoFailsWhenItCannotAsk(t *testing.T) {
	_, _, err := resolveSource(sourceAuto, false, false)
	if err == nil {
		t.Fatal("want error when auto cannot confirm")
	}
	if !strings.Contains(err.Error(), "-source audio") {
		t.Errorf("error %q does not say how to proceed", err)
	}
}

// An explicit choice is never second-guessed and never re-confirmed.
func TestResolveSourceExplicitModesPassThrough(t *testing.T) {
	for _, mode := range []sourceMode{sourceSub, sourceAudio} {
		for _, hasSubs := range []bool{true, false} {
			for _, interactive := range []bool{true, false} {
				got, confirm, err := resolveSource(mode, hasSubs, interactive)
				if err != nil {
					t.Errorf("resolveSource(%v, %v, %v): %v", mode, hasSubs, interactive, err)
				}
				if got != mode {
					t.Errorf("resolveSource(%v, ...) = %v, want unchanged", mode, got)
				}
				if confirm {
					t.Errorf("resolveSource(%v, ...) asked for confirmation", mode)
				}
			}
		}
	}
}
