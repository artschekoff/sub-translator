package main

import (
	"strings"
	"testing"
)

func TestParseMode(t *testing.T) {
	tests := []struct {
		in      string
		want    outputMode
		wantErr bool
	}{
		{"srt", modeSRT, false},
		{"mux", modeMux, false},
		{"both", modeBoth, false},
		{"SRT", modeSRT, false},
		{"  mux  ", modeMux, false},
		{"", 0, true},
		{"nomux", 0, true},
		{"srt,mux", 0, true},
	}

	for _, tt := range tests {
		got, err := parseMode(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseMode(%q): want error, got mode %v", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMode(%q): unexpected error: %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseMode(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestParseModeErrorNamesValidValues(t *testing.T) {
	_, err := parseMode("bogus")
	if err == nil {
		t.Fatal("want error for bogus mode")
	}
	for _, want := range []string{"srt", "mux", "both", "bogus"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestModePredicates(t *testing.T) {
	tests := []struct {
		mode          outputMode
		wantSRT       bool
		wantContainer bool
		wantString    string
	}{
		{modeSRT, true, false, "srt"},
		{modeMux, false, true, "mux"},
		{modeBoth, true, true, "both"},
	}

	for _, tt := range tests {
		if got := tt.mode.writesSRT(); got != tt.wantSRT {
			t.Errorf("%v.writesSRT() = %v, want %v", tt.mode, got, tt.wantSRT)
		}
		if got := tt.mode.writesContainer(); got != tt.wantContainer {
			t.Errorf("%v.writesContainer() = %v, want %v", tt.mode, got, tt.wantContainer)
		}
		if got := tt.mode.String(); got != tt.wantString {
			t.Errorf("String() = %q, want %q", got, tt.wantString)
		}
	}
}

func TestResolveModeDowngradesWhenContainerCannotEmbed(t *testing.T) {
	for _, mode := range []outputMode{modeMux, modeBoth} {
		got, note := resolveMode(mode, false)
		if got != modeSRT {
			t.Errorf("resolveMode(%v, false) = %v, want modeSRT", mode, got)
		}
		if note == "" {
			t.Errorf("resolveMode(%v, false): want an explanatory note, got empty string", mode)
		}
	}
}

func TestResolveModeLeavesEmbeddableContainersAlone(t *testing.T) {
	for _, mode := range []outputMode{modeSRT, modeMux, modeBoth} {
		got, note := resolveMode(mode, true)
		if got != mode {
			t.Errorf("resolveMode(%v, true) = %v, want unchanged", mode, got)
		}
		if note != "" {
			t.Errorf("resolveMode(%v, true): want no note, got %q", mode, note)
		}
	}
}

func TestResolveModeSRTNeedsNoDowngrade(t *testing.T) {
	got, note := resolveMode(modeSRT, false)
	if got != modeSRT {
		t.Errorf("resolveMode(modeSRT, false) = %v, want modeSRT", got)
	}
	if note != "" {
		t.Errorf("want no note when nothing was downgraded, got %q", note)
	}
}
