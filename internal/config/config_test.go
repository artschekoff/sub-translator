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
		"translate.api-key":     "sk-abcdefghijklmnop-a4f2",
		"translate.model":       "gpt-6-luna",
		"translate.workers":     "6",
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

// Save() must tighten permissions even on existing files. A config file that
// ever ends up at 0644 (e.g., if umask permitted it or permissions were later
// relaxed) will remain readable to all users on a shared machine for the rest
// of its life unless Save() re-asserts 0600. This test ensures that on repeated
// calls to Save(), permissions are tightened, not just set once.
func TestSaveTightensExistingFilePermissions(t *testing.T) {
	sandbox(t)
	p, _ := Path()
	dir := filepath.Dir(p)

	// Pre-create config directory and file with loose permissions
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// Verify they start loose
	fi, _ := os.Stat(p)
	if perm := fi.Mode().Perm(); perm != 0o644 {
		t.Fatalf("pre-existing file mode = %o, want 644", perm)
	}
	di, _ := os.Stat(dir)
	if perm := di.Mode().Perm(); perm != 0o755 {
		t.Fatalf("pre-existing dir mode = %o, want 755", perm)
	}

	// Call Save() which should tighten permissions
	c := &Config{}
	if err := c.Set("whisper.model", "/models/base.bin"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Verify permissions were tightened
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat after save: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("config file mode after Save = %o, want 600", perm)
	}
	di, err = os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir after save: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("config dir mode after Save = %o, want 700", perm)
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

// A worker count that cannot run and an empty model are both mistakes worth
// catching at `config set`, where the message can still name what is acceptable,
// rather than in the middle of a film.
func TestSetValidatesTranslateSettings(t *testing.T) {
	c := &Config{}
	for _, tt := range []struct {
		key, value, mustName string
	}{
		{"translate.workers", "0", "positive"},
		{"translate.workers", "-2", "positive"},
		{"translate.workers", "lots", "positive"},
		{"translate.model", "", "gpt-4o-mini"},
		{"translate.model", "   ", "gpt-4o-mini"},
	} {
		err := c.Set(tt.key, tt.value)
		if err == nil {
			t.Errorf("Set(%q, %q): want error", tt.key, tt.value)
			continue
		}
		if !strings.Contains(err.Error(), tt.mustName) {
			t.Errorf("Set(%q, %q) error %q does not say what is acceptable (%q)",
				tt.key, tt.value, err, tt.mustName)
		}
	}
	for _, ok := range []string{"1", "4", "16"} {
		if err := c.Set("translate.workers", ok); err != nil {
			t.Errorf("Set(translate.workers, %q): %v", ok, err)
		}
	}
}

// The API key is the one secret in the file. Display is what `config list`
// prints, and a listing is what people paste into an issue.
func TestDisplayMasksTheAPIKeyButGetDoesNot(t *testing.T) {
	c := &Config{}
	const key = "sk-proj-abcdefghijklmnopqrstuvwxyz-a4f2"
	if err := c.Set("translate.api-key", key); err != nil {
		t.Fatalf("Set: %v", err)
	}

	full, err := c.Get("translate.api-key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if full != key {
		t.Errorf("Get returned %q, want the key in full — that is how it gets scripted with", full)
	}

	masked, err := c.Display("translate.api-key")
	if err != nil {
		t.Fatalf("Display: %v", err)
	}
	if want := "sk-…a4f2"; masked != want {
		t.Errorf("Display = %q, want %q", masked, want)
	}
	if strings.Contains(masked, "abcdefghij") {
		t.Errorf("Display leaked the middle of the key: %q", masked)
	}

	// Nothing else is masked, or a listing would stop being useful.
	if err := c.Set("whisper.language", "en"); err != nil {
		t.Fatal(err)
	}
	if got, _ := c.Display("whisper.language"); got != "en" {
		t.Errorf("Display(whisper.language) = %q, want en", got)
	}
	if _, err := c.Display("nope"); err == nil {
		t.Error("Display: want an error for an unknown key")
	}
}

func TestMask(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"", ""},
		{"short", "…"},
		{"sk-1234", "…"},         // seven characters: too short to show both ends
		{"sk-12345", "sk-…2345"}, // eight is the shortest that masks anything
		{"sk-proj-deadbeefa4f2", "sk-…a4f2"},
		{"ключ-очень-длинный-1234", "клю…1234"}, // runes, not bytes
	} {
		if got := Mask(tt.in); got != tt.want {
			t.Errorf("Mask(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestKeysAreSortedAndComplete(t *testing.T) {
	// The comparison is exact in both directions on purpose. Checking only that
	// every expected key is present cannot notice a key that should have gone —
	// translate.provider was removed here, and a per-key Contains would have kept
	// saying so if it came back.
	want := []string{
		"models.dir", "translate.api-key", "translate.model",
		"translate.workers", "whisper.bin", "whisper.chunk-minutes",
		"whisper.language", "whisper.model", "whisper.threads", "whisper.vad-model",
		"whisper.vad-threshold",
	}
	if !slices.IsSorted(want) {
		t.Fatalf("the expected list is itself unsorted: %v", want)
	}
	if keys := Keys(); !slices.Equal(keys, want) {
		t.Errorf("Keys() = %v, want exactly %v", keys, want)
	}
}
