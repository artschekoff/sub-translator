package main

import (
	"io"
	"strings"
	"testing"
)

// Subcommands must be recognised before flag parsing, or `config` would be
// treated as the input filename.
func TestDispatchSubcommandRecognisesOnlyKnownVerbs(t *testing.T) {
	tests := []struct {
		args        []string
		wantHandled bool
	}{
		{[]string{"config", "path"}, true},
		{[]string{"model", "list"}, true},
		{[]string{"movie.mkv"}, false},
		{[]string{"-to", "es", "movie.mkv"}, false},
		{nil, false},
	}
	for _, tt := range tests {
		handled, _ := dispatchSubcommand(tt.args, io.Discard)
		if handled != tt.wantHandled {
			t.Errorf("dispatchSubcommand(%v) handled = %v, want %v",
				tt.args, handled, tt.wantHandled)
		}
	}
}

func TestRunConfigPathPrintsTheConfigLocation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out strings.Builder
	if err := runConfig([]string{"path"}, &out); err != nil {
		t.Fatalf("runConfig path: %v", err)
	}
	if !strings.Contains(out.String(), "config.json") {
		t.Errorf("output %q does not name the config file", out.String())
	}
}

func TestRunConfigSetThenGetRoundTrips(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if err := runConfig([]string{"set", "whisper.threads", "4"}, io.Discard); err != nil {
		t.Fatalf("set: %v", err)
	}
	var out strings.Builder
	if err := runConfig([]string{"get", "whisper.threads"}, &out); err != nil {
		t.Fatalf("get: %v", err)
	}
	if strings.TrimSpace(out.String()) != "4" {
		t.Errorf("get printed %q, want 4", out.String())
	}
}

func TestRunConfigListShowsEveryKey(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	var out strings.Builder
	if err := runConfig([]string{"list"}, &out); err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, key := range []string{"whisper.bin", "whisper.model", "models.dir"} {
		if !strings.Contains(out.String(), key) {
			t.Errorf("list output missing %q:\n%s", key, out.String())
		}
	}
}

// `config list` is what gets pasted into an issue or a screenshot; `config get`
// is what gets read back by a script. Only the first one may hide the key.
func TestRunConfigMasksTheAPIKeyInListButNotInGet(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	const key = "sk-proj-abcdefghijklmnopqrstuvwxyz-a4f2"
	if err := runConfig([]string{"set", "translate.api-key", key}, io.Discard); err != nil {
		t.Fatalf("set: %v", err)
	}

	var set strings.Builder
	if err := runConfig([]string{"set", "translate.api-key", key}, &set); err != nil {
		t.Fatalf("set: %v", err)
	}
	if strings.Contains(set.String(), key) {
		t.Errorf("config set echoed the key in full: %q", set.String())
	}

	var list strings.Builder
	if err := runConfig([]string{"list"}, &list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if strings.Contains(list.String(), key) {
		t.Errorf("config list printed the key in full:\n%s", list.String())
	}
	if !strings.Contains(list.String(), "sk-…a4f2") {
		t.Errorf("config list does not show the masked key:\n%s", list.String())
	}

	var get strings.Builder
	if err := runConfig([]string{"get", "translate.api-key"}, &get); err != nil {
		t.Fatalf("get: %v", err)
	}
	if strings.TrimSpace(get.String()) != key {
		t.Errorf("config get printed %q, want the key in full", get.String())
	}
}

func TestRunConfigRejectsBadUsage(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, args := range [][]string{
		{},
		{"frobnicate"},
		{"set", "whisper.threads"},
		{"set", "translate.provider", "deepl"},
		{"set", "translate.workers", "0"},
		{"set"},
		{"get"},
		{"get", "nope"},
	} {
		if err := runConfig(args, io.Discard); err == nil {
			t.Errorf("runConfig(%v): want error", args)
		}
	}
}

func TestRunModelListNamesCatalogEntries(t *testing.T) {
	var out strings.Builder
	if err := runModel([]string{"list"}, &out); err != nil {
		t.Fatalf("model list: %v", err)
	}
	for _, want := range []string{"large-v3-turbo", "silero-vad"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("model list missing %q:\n%s", want, out.String())
		}
	}
}

func TestRunModelRejectsBadUsage(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"delete"},
		{"pull"},
		{"pull", "gpt-4"},
	} {
		if err := runModel(args, io.Discard); err == nil {
			t.Errorf("runModel(%v): want error", args)
		}
	}
}

// An unknown model name must name the command that lists the real ones.
func TestRunModelPullUnknownNameIsActionable(t *testing.T) {
	err := runModel([]string{"pull", "gpt-4"}, io.Discard)
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "model list") {
		t.Errorf("error %q does not point at `model list`", err)
	}
}
