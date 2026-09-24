package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Every test must point HOME-derived lookups at a scratch dir: a test that
// writes the developer's real ~/.config/sub-translator/config.json is a bug.
func sandbox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return dir
}

func TestPathHonoursXDGConfigHome(t *testing.T) {
	dir := sandbox(t)
	got, err := Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	want := filepath.Join(dir, "sub-translator", "config.json")
	if got != want {
		t.Errorf("Path() = %q, want %q", got, want)
	}
}

// A first run has no config file. That is the normal case, not a failure.
func TestLoadMissingFileReturnsEmptyConfig(t *testing.T) {
	sandbox(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load on missing file: %v", err)
	}
	if c.Whisper.Model != "" {
		t.Errorf("want zero-value config, got model %q", c.Whisper.Model)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	sandbox(t)
	c := &Config{}
	for key, value := range map[string]string{
		"whisper.bin":           "/usr/local/bin/whisper-cli",
		"whisper.model":         "/models/ggml-large-v3-turbo.bin",
		"whisper.vad-model":     "/models/ggml-silero-v5.1.2.bin",
		"whisper.vad-threshold": "0.5",
		"whisper.threads":       "8",
		"whisper.language":      "auto",
		"models.dir":            "/models",
	} {
		if err := c.Set(key, value); err != nil {
			t.Fatalf("Set(%q): %v", key, err)
		}
	}
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, key := range Keys() {
		want, _ := c.Get(key)
		have, err := got.Get(key)
		if err != nil {
			t.Fatalf("Get(%q) after reload: %v", key, err)
		}
		if have != want {
			t.Errorf("%s = %q after round trip, want %q", key, have, want)
		}
	}
}

// The config file records paths, which are not secret but are personal. It is
// created 0600 in a 0700 directory so a shared machine does not leak them.
func TestSaveUsesPrivatePermissions(t *testing.T) {
	sandbox(t)
	c := &Config{}
	if err := c.Set("whisper.model", "/models/ggml-base.bin"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	p, _ := Path()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file mode = %o, want 600", perm)
	}
	di, err := os.Stat(filepath.Dir(p))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("config dir mode = %o, want 700", perm)
	}
}

// A path stored as "~/models/x.bin" would have to be re-expanded by every
// consumer. Expanding once at Set time keeps the file unambiguous.
func TestSetExpandsTilde(t *testing.T) {
	sandbox(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	c := &Config{}
	if err := c.Set("whisper.model", "~/models/ggml-base.bin"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, _ := c.Get("whisper.model")
	want := filepath.Join(home, "models", "ggml-base.bin")
	if got != want {
		t.Errorf("Get = %q, want %q", got, want)
	}
}

func TestUnknownKeyErrorNamesValidKeys(t *testing.T) {
	c := &Config{}
	err := c.Set("whisper.modle", "x")
	if err == nil {
		t.Fatal("want error for unknown key")
	}
	if !strings.Contains(err.Error(), "whisper.modle") {
		t.Errorf("error %q does not quote the bad key", err)
	}
	if !strings.Contains(err.Error(), "whisper.model") {
		t.Errorf("error %q does not list the valid keys", err)
	}
	if _, err := c.Get("nope"); err == nil {
		t.Error("Get: want error for unknown key")
	}
}

func TestSetRejectsMalformedNumbers(t *testing.T) {
	c := &Config{}
	for _, tt := range []struct{ key, value string }{
		{"whisper.threads", "many"},
		{"whisper.threads", "-1"},
		{"whisper.vad-threshold", "high"},
		{"whisper.vad-threshold", "1.5"},
	} {
		if err := c.Set(tt.key, tt.value); err == nil {
			t.Errorf("Set(%q, %q): want error", tt.key, tt.value)
		}
	}
}

func TestKeysAreSortedAndComplete(t *testing.T) {
	keys := Keys()
	if !slices.IsSorted(keys) {
		t.Errorf("Keys() must be sorted for stable output, got %v", keys)
	}
	for _, want := range []string{
		"models.dir", "whisper.bin", "whisper.language",
		"whisper.model", "whisper.threads", "whisper.vad-model",
		"whisper.vad-threshold",
	} {
		if !slices.Contains(keys, want) {
			t.Errorf("Keys() missing %q", want)
		}
	}
}
