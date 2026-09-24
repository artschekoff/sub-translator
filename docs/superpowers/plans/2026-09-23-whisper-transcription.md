# Whisper Transcription Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let `sub-translator` generate subtitles from a video's audio track using whisper.cpp when the file carries no subtitle track, with the whisper binary and ggml model path configurable once and remembered.

**Architecture:** Three new leaf packages — `internal/config` (a JSON settings file), `internal/models` (model catalog, discovery and download), `internal/whisper` (runs the external `whisper-cli`) — plus audio helpers in `internal/media` and a `sourceMode` type in `main`. whisper is asked for `-osrt`, so its output re-enters the existing pipeline at `srt.Parse` and everything downstream is untouched.

**Tech Stack:** Go 1.26, standard library only. `whisper-cli` and `ffmpeg` are invoked as subprocesses.

**Spec:** `docs/superpowers/specs/2026-09-23-whisper-transcription-design.md`

## Global Constraints

- Go version floor: **1.26** (`go.mod` says `go 1.26.2`) — do not lower it, do not add a toolchain directive.
- **No new third-party dependencies.** `go.mod` has zero requires today and must still have zero when this plan is done.
- All code must pass `make validate` (runs `gofmt -w .`, `go vet ./...`, `go test ./... -v`).
- `internal/srt` and `internal/translate` are **not modified** by any task.
- Never write to `~/Library/Application Support/net.slaive.app/models`. It belongs to another application; we only read it.
- Tests must pass on a machine where `whisper-cli` is **not** installed. Any test needing the real binary calls `t.Skip`.
- Tests must never write to the real user config directory. They set `XDG_CONFIG_HOME` to `t.TempDir()`.
- Flag names and values are lowercase and exact: `-source` with values `sub`, `audio`, `auto`.
- Conventional commit messages (`feat:`, `test:`, `docs:`, `refactor:`), matching the existing history.
- Do not reformat or restructure code unrelated to this feature.

---

## File Structure

| File | Status | Responsibility |
|---|---|---|
| `internal/config/config.go` | Create | Typed config struct, load/save, dotted-key get/set, config file path. |
| `internal/config/config_test.go` | Create | Round-trip, key table, `~` expansion, permissions, unknown keys. |
| `internal/models/models.go` | Create | Model catalog, search directories, discovery, download. |
| `internal/models/models_test.go` | Create | Catalog shape, URL/filename derivation, discovery classification, search order. |
| `internal/whisper/whisper.go` | Create | `Options`, pure `Args`, `FindBinary`, `Run` with progress and detected language. |
| `internal/whisper/whisper_test.go` | Create | Argument contract, binary lookup, progress-line parsing, JSON language read. |
| `internal/media/media.go` | Modify | Add `AudioStreams`, `FindAudioByLang`, `ExtractAudio` and the shared `findByLang` helper. |
| `internal/media/media_test.go` | Modify | Audio stream selection and `ffmpeg` audio-extraction arguments. |
| `source.go` | Create | `sourceMode` type, parsing, and `auto` resolution. Pure logic, no I/O. |
| `source_test.go` | Create | Table tests for parsing and resolution. |
| `cmd_config.go` | Create | `sub-translator config list/get/set/path`. |
| `cmd_model.go` | Create | `sub-translator model list/pull`. |
| `cmd_test.go` | Create | Subcommand argument validation. |
| `ui.go` | Modify | Add `readConfirm`/`confirm` and `stdinIsTerminal`. |
| `ui_test.go` | Modify | Confirmation prompt parsing. |
| `main.go` | Modify | Subcommand dispatch, new flags, source branch, whisper orchestration. |
| `README.md` | Modify | Document transcription, the config and model subcommands, the new flags. |

---

### Task 1: Config file

**Files:**
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces, in package `config`:
  - `type Config struct { Whisper WhisperConfig; Models ModelsConfig }`
  - `type WhisperConfig struct { Bin, Model, VADModel, Language string; VADThreshold float64; Threads int }`
  - `type ModelsConfig struct { Dir string }`
  - `func Path() (string, error)`
  - `func Load() (*Config, error)`
  - `func (c *Config) Save() error`
  - `func (c *Config) Get(key string) (string, error)`
  - `func (c *Config) Set(key, value string) error`
  - `func Keys() []string`

- [ ] **Step 1: Write the failing test**

Create `internal/config/config_test.go`:

```go
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
		"whisper.bin":            "/usr/local/bin/whisper-cli",
		"whisper.model":          "/models/ggml-large-v3-turbo.bin",
		"whisper.vad-model":      "/models/ggml-silero-v5.1.2.bin",
		"whisper.vad-threshold":  "0.5",
		"whisper.threads":        "8",
		"whisper.language":       "auto",
		"models.dir":             "/models",
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -v`
Expected: FAIL — the package does not exist yet.

- [ ] **Step 3: Write minimal implementation**

Create `internal/config/config.go`:

```go
// Package config stores the settings sub-translator remembers between runs —
// chiefly where the whisper binary and its ggml model live, so those paths are
// typed once rather than on every invocation.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

type WhisperConfig struct {
	Bin          string  `json:"bin,omitempty"`
	Model        string  `json:"model,omitempty"`
	VADModel     string  `json:"vadModel,omitempty"`
	VADThreshold float64 `json:"vadThreshold,omitempty"`
	Threads      int     `json:"threads,omitempty"`
	Language     string  `json:"language,omitempty"`
}

type ModelsConfig struct {
	Dir string `json:"dir,omitempty"`
}

type Config struct {
	Whisper WhisperConfig `json:"whisper"`
	Models  ModelsConfig  `json:"models"`
}

// field couples a dotted key to its accessors, so Keys, Get and Set can never
// drift out of sync the way three parallel switch statements would.
type field struct {
	get func(*Config) string
	set func(*Config, string) error
}

var fields = map[string]field{
	"whisper.bin": {
		get: func(c *Config) string { return c.Whisper.Bin },
		set: func(c *Config, v string) error { c.Whisper.Bin = expandPath(v); return nil },
	},
	"whisper.model": {
		get: func(c *Config) string { return c.Whisper.Model },
		set: func(c *Config, v string) error { c.Whisper.Model = expandPath(v); return nil },
	},
	"whisper.vad-model": {
		get: func(c *Config) string { return c.Whisper.VADModel },
		set: func(c *Config, v string) error { c.Whisper.VADModel = expandPath(v); return nil },
	},
	"whisper.vad-threshold": {
		get: func(c *Config) string {
			if c.Whisper.VADThreshold == 0 {
				return ""
			}
			return strconv.FormatFloat(c.Whisper.VADThreshold, 'g', -1, 64)
		},
		set: func(c *Config, v string) error {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f < 0 || f > 1 {
				return fmt.Errorf("whisper.vad-threshold must be a number between 0 and 1, got %q", v)
			}
			c.Whisper.VADThreshold = f
			return nil
		},
	},
	"whisper.threads": {
		get: func(c *Config) string {
			if c.Whisper.Threads == 0 {
				return ""
			}
			return strconv.Itoa(c.Whisper.Threads)
		},
		set: func(c *Config, v string) error {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				return fmt.Errorf("whisper.threads must be a non-negative integer, got %q", v)
			}
			c.Whisper.Threads = n
			return nil
		},
	},
	"whisper.language": {
		get: func(c *Config) string { return c.Whisper.Language },
		set: func(c *Config, v string) error { c.Whisper.Language = strings.TrimSpace(v); return nil },
	},
	"models.dir": {
		get: func(c *Config) string { return c.Models.Dir },
		set: func(c *Config, v string) error { c.Models.Dir = expandPath(v); return nil },
	},
}

// Keys returns every valid config key, sorted, for `config list` and for the
// "did you mean" list in errors.
func Keys() []string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func unknownKey(key string) error {
	return fmt.Errorf("unknown config key %q; valid keys: %s", key, strings.Join(Keys(), ", "))
}

func (c *Config) Get(key string) (string, error) {
	f, ok := fields[key]
	if !ok {
		return "", unknownKey(key)
	}
	return f.get(c), nil
}

func (c *Config) Set(key, value string) error {
	f, ok := fields[key]
	if !ok {
		return unknownKey(key)
	}
	return f.set(c, value)
}

// expandPath resolves a leading ~ and makes the path absolute, so what lands in
// the file means the same thing from any working directory.
func expandPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// Path is the config file location: $XDG_CONFIG_HOME/sub-translator/config.json,
// falling back to ~/.config. The same layout is used on every OS deliberately —
// os.UserConfigDir would put it under ~/Library on macOS, which is harder to
// find and to talk about in documentation.
func Path() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "sub-translator", "config.json"), nil
}

// Load reads the config file. A missing file yields an empty config and no
// error: that is simply a machine where nothing has been configured yet.
func Load() (*Config, error) {
	p, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return &c, nil
}

func (c *Config) Save() error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(p, data, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -v`
Expected: PASS, every test.

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat: add persistent config file for whisper settings"
```

---

### Task 2: Model catalog, discovery and download

**Files:**
- Create: `internal/models/models.go`
- Test: `internal/models/models_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces, in package `models`:
  - `type Kind int` with `KindTranscribe`, `KindVAD`
  - `type Model struct { Name, Filename, URL string; Kind Kind }`
  - `func Catalog() []Model`
  - `func Find(name string) (Model, bool)`
  - `func DefaultDir() (string, error)`
  - `func SearchDirs(configuredDir string) []string`
  - `func Discover(dirs []string) (transcribe, vad []string)`
  - `func IsVADFile(name string) bool`
  - `func Pull(m Model, destDir string, progress func(done, total int64)) (string, error)`

- [ ] **Step 1: Write the failing test**

Create `internal/models/models_test.go`:

```go
package models

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The catalog must name models exactly as whisper.cpp does, because the
// filename it produces has to match what other whisper.cpp front-ends already
// have on disk — that is the whole of "compatibility" here.
func TestCatalogFilenamesFollowGGMLConvention(t *testing.T) {
	for _, m := range Catalog() {
		if !strings.HasPrefix(m.Filename, "ggml-") || !strings.HasSuffix(m.Filename, ".bin") {
			t.Errorf("%s: filename %q is not ggml-<name>.bin", m.Name, m.Filename)
		}
		if !strings.HasSuffix(m.URL, m.Filename) {
			t.Errorf("%s: URL %q does not end in its filename %q", m.Name, m.URL, m.Filename)
		}
	}
}

// large-v3-turbo and the Silero VAD model are the two files already present in
// the author's whisper.cpp setup; both must be in the catalog and must resolve
// to exactly those filenames.
func TestCatalogCoversTheModelsAlreadyOnDisk(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		kind     Kind
	}{
		{"large-v3-turbo", "ggml-large-v3-turbo.bin", KindTranscribe},
		{"silero-vad", "ggml-silero-v5.1.2.bin", KindVAD},
		{"base", "ggml-base.bin", KindTranscribe},
	}
	for _, tt := range tests {
		m, ok := Find(tt.name)
		if !ok {
			t.Errorf("Find(%q): not in catalog", tt.name)
			continue
		}
		if m.Filename != tt.filename {
			t.Errorf("Find(%q).Filename = %q, want %q", tt.name, m.Filename, tt.filename)
		}
		if m.Kind != tt.kind {
			t.Errorf("Find(%q).Kind = %v, want %v", tt.name, m.Kind, tt.kind)
		}
	}
	if _, ok := Find("gpt-4"); ok {
		t.Error("Find: want miss for a name that is not a whisper model")
	}
}

// A VAD model is not a transcription model. Feeding Silero to -m makes
// whisper-cli fail with an opaque tensor error, so discovery must keep the two
// kinds apart.
func TestIsVADFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"ggml-silero-v5.1.2.bin", true},
		{"/abs/path/ggml-silero-v5.1.2.bin", true},
		{"ggml-vad.bin", true},
		{"ggml-large-v3-turbo.bin", false},
		{"ggml-base.en.bin", false},
	}
	for _, tt := range tests {
		if got := IsVADFile(tt.name); got != tt.want {
			t.Errorf("IsVADFile(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestDiscoverSplitsModelsByKindAndSkipsOtherFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"ggml-large-v3-turbo.bin",
		"ggml-silero-v5.1.2.bin",
		"notes.txt",
		"ggml-base.bin.part", // an interrupted download must never be offered
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	transcribe, vad := Discover([]string{dir, filepath.Join(dir, "does-not-exist")})

	if len(transcribe) != 1 || filepath.Base(transcribe[0]) != "ggml-large-v3-turbo.bin" {
		t.Errorf("transcribe models = %v, want just ggml-large-v3-turbo.bin", transcribe)
	}
	if len(vad) != 1 || filepath.Base(vad[0]) != "ggml-silero-v5.1.2.bin" {
		t.Errorf("vad models = %v, want just ggml-silero-v5.1.2.bin", vad)
	}
}

// The configured directory must win, and our own directory must be consulted
// before another application's, so `model pull` output is preferred over a
// file we do not control.
func TestSearchDirsOrder(t *testing.T) {
	dirs := SearchDirs("/configured")
	if len(dirs) == 0 || dirs[0] != "/configured" {
		t.Fatalf("configured dir must come first, got %v", dirs)
	}
	if slices.Contains(SearchDirs(""), "/configured") {
		t.Error("empty configured dir must not appear in the search path")
	}

	if runtime.GOOS != "darwin" {
		return
	}
	own, err := DefaultDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	iOwn := slices.Index(dirs, own)
	iSlaive := -1
	for i, d := range dirs {
		if strings.Contains(d, "net.slaive.app") {
			iSlaive = i
		}
	}
	if iOwn < 0 {
		t.Fatalf("our own model dir %q missing from %v", own, dirs)
	}
	if iSlaive < 0 {
		t.Fatalf("the Slaive model dir is missing from %v", dirs)
	}
	if iOwn > iSlaive {
		t.Errorf("our dir (%d) must precede Slaive's (%d): %v", iOwn, iSlaive, dirs)
	}
}

// A pull that dies halfway must not leave a truncated .bin behind, because
// Discover would happily hand it to whisper on the next run.
func TestPullRejectsShortBodyAndLeavesNoFile(t *testing.T) {
	dir := t.TempDir()
	srv := shortBodyServer(t)
	m := Model{Name: "test", Filename: "ggml-test.bin", URL: srv, Kind: KindTranscribe}

	if _, err := Pull(m, dir, nil); err == nil {
		t.Fatal("want error when the body is shorter than Content-Length")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("failed pull left files behind: %v", entries)
	}
}

func TestPullWritesFileAndReportsProgress(t *testing.T) {
	dir := t.TempDir()
	srv := okServer(t, []byte("hello whisper"))
	m := Model{Name: "test", Filename: "ggml-test.bin", URL: srv, Kind: KindTranscribe}

	var lastDone, lastTotal int64
	path, err := Pull(m, dir, func(done, total int64) { lastDone, lastTotal = done, total })
	if err != nil {
		t.Fatalf("Pull: %v", err)
	}
	if got := filepath.Base(path); got != "ggml-test.bin" {
		t.Errorf("saved as %q, want ggml-test.bin", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello whisper" {
		t.Errorf("content = %q", data)
	}
	if lastTotal != int64(len("hello whisper")) || lastDone != lastTotal {
		t.Errorf("progress ended at %d/%d, want %d/%d",
			lastDone, lastTotal, lastTotal, lastTotal)
	}
}
```

Add the two test servers in the same file:

```go
func okServer(t *testing.T, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// shortBodyServer promises more bytes than it delivers, which is what a dropped
// connection looks like to the client.
func shortBodyServer(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.Write([]byte("truncated"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
```

and extend the import block with `"net/http"`, `"net/http/httptest"`, `"strconv"`.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/models/ -v`
Expected: FAIL — the package does not exist yet.

- [ ] **Step 3: Write minimal implementation**

Create `internal/models/models.go`:

```go
// Package models knows where whisper.cpp ggml models live, which ones can be
// downloaded, and how to fetch one. It deliberately shares the upstream
// ggml-<name>.bin naming so models downloaded by other whisper.cpp front-ends
// are found and reused rather than duplicated.
package models

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type Kind int

const (
	// KindTranscribe is a Whisper speech-recognition model, passed to -m.
	KindTranscribe Kind = iota
	// KindVAD is a voice-activity-detection model, passed to --vad-model.
	KindVAD
)

func (k Kind) String() string {
	if k == KindVAD {
		return "vad"
	}
	return "transcribe"
}

type Model struct {
	Name     string
	Filename string
	URL      string
	Kind     Kind
}

const (
	whisperBase = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/"
	vadBase     = "https://huggingface.co/ggml-org/whisper-vad/resolve/main/"
)

// whisperModels are the upstream names worth offering. Quantised and
// English-only variants exist upstream too, but every extra row is a row to
// keep correct, and these cover the useful quality/size range.
var whisperModels = []string{
	"tiny", "base", "small", "medium", "large-v2", "large-v3", "large-v3-turbo",
}

func Catalog() []Model {
	out := make([]Model, 0, len(whisperModels)+1)
	for _, name := range whisperModels {
		filename := "ggml-" + name + ".bin"
		out = append(out, Model{
			Name:     name,
			Filename: filename,
			URL:      whisperBase + filename,
			Kind:     KindTranscribe,
		})
	}
	const vadFile = "ggml-silero-v5.1.2.bin"
	out = append(out, Model{
		Name:     "silero-vad",
		Filename: vadFile,
		URL:      vadBase + vadFile,
		Kind:     KindVAD,
	})
	return out
}

func Find(name string) (Model, bool) {
	name = strings.TrimSpace(strings.ToLower(name))
	for _, m := range Catalog() {
		if m.Name == name {
			return m, true
		}
	}
	return Model{}, false
}

// DefaultDir is where `model pull` writes. It is ours alone; models found in
// other applications' directories are read but never written.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "sub-translator", "models"), nil
	}
	if base := os.Getenv("XDG_DATA_HOME"); base != "" {
		return filepath.Join(base, "sub-translator", "models"), nil
	}
	return filepath.Join(home, ".local", "share", "sub-translator", "models"), nil
}

// SearchDirs lists the directories scanned for an already-present model, in
// precedence order. Directories that do not exist are harmless: Discover skips
// them.
func SearchDirs(configuredDir string) []string {
	var dirs []string
	if strings.TrimSpace(configuredDir) != "" {
		dirs = append(dirs, configuredDir)
	}
	if own, err := DefaultDir(); err == nil {
		dirs = append(dirs, own)
	}
	if home, err := os.UserHomeDir(); err == nil {
		if runtime.GOOS == "darwin" {
			// Slaive is a macOS whisper.cpp front-end. Reading its models saves
			// re-downloading well over a gigabyte.
			dirs = append(dirs, filepath.Join(home,
				"Library", "Application Support", "net.slaive.app", "models"))
		}
		dirs = append(dirs, filepath.Join(home, ".cache", "whisper.cpp"))
	}
	return append(dirs, "models")
}

// IsVADFile reports whether a ggml file is a voice-activity-detection model.
// Passing one to -m fails deep inside whisper with an unhelpful message, so the
// two kinds are separated by name before they ever reach the binary.
func IsVADFile(name string) bool {
	lower := strings.ToLower(filepath.Base(name))
	return strings.Contains(lower, "silero") || strings.Contains(lower, "vad")
}

// Discover scans dirs for ggml models and splits them by kind. Paths are
// returned in the order their directories were given, so the caller's
// precedence survives.
func Discover(dirs []string) (transcribe, vad []string) {
	seen := map[string]bool{}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasPrefix(name, "ggml-") || !strings.HasSuffix(name, ".bin") {
				continue
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			path := filepath.Join(dir, name)
			if IsVADFile(name) {
				vad = append(vad, path)
			} else {
				transcribe = append(transcribe, path)
			}
		}
	}
	return transcribe, vad
}

// Pull downloads m into destDir and returns the saved path. It writes to a
// .part file and renames on success, so an interrupted download can never be
// mistaken for a usable model by Discover.
func Pull(m Model, destDir string, progress func(done, total int64)) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", destDir, err)
	}
	final := filepath.Join(destDir, m.Filename)
	tmp := final + ".part"

	resp, err := http.Get(m.URL)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", m.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: %s", m.Name, resp.Status)
	}

	f, err := os.Create(tmp)
	if err != nil {
		return "", fmt.Errorf("create %s: %w", tmp, err)
	}
	total := resp.ContentLength
	written, err := io.Copy(f, &progressReader{r: resp.Body, total: total, report: progress})
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil && total > 0 && written != total {
		err = fmt.Errorf("download %s: got %d bytes, expected %d", m.Name, written, total)
	}
	if err != nil {
		os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, final); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("finalize %s: %w", final, err)
	}
	return final, nil
}

type progressReader struct {
	r      io.Reader
	done   int64
	total  int64
	report func(done, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if p.report != nil && n > 0 {
		p.report(p.done, p.total)
	}
	return n, err
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/models/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/models/
git commit -m "feat: add whisper model catalog, discovery and download"
```

---

### Task 3: Whisper runner

**Files:**
- Create: `internal/whisper/whisper.go`
- Test: `internal/whisper/whisper_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces, in package `whisper`:
  - `type Options struct { Bin, Model, VADModel, Audio, OutBase, Language string; VADThreshold float64; Threads int }`
  - `type Result struct { Language, SRTPath string }`
  - `func Args(o Options) []string`
  - `func FindBinary(configured string) (string, error)`
  - `func Run(o Options, progress func(pct int)) (Result, error)`

Flag reference, verified against whisper.cpp `examples/cli/cli.cpp`: `-m` model, `-f` input audio, `-of` output base path without extension, `-osrt` SRT output, `-oj` JSON output, `-l` language (`auto` to detect), `-t` threads, `-pp` print progress, `--vad` / `-vm` / `-vt` voice activity detection.

- [ ] **Step 1: Write the failing test**

Create `internal/whisper/whisper_test.go`:

```go
package whisper

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func argValue(args []string, flag string) (string, bool) {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return "", false
	}
	return args[i+1], true
}

// The argument contract is the whole interface to whisper-cli. Getting -of
// wrong (passing a path *with* an extension) makes whisper write
// "out.srt.srt", which the caller then fails to find.
func TestArgsCarriesModelAudioAndOutputBase(t *testing.T) {
	args := Args(Options{
		Model:   "/models/ggml-large-v3-turbo.bin",
		Audio:   "/tmp/audio.wav",
		OutBase: "/tmp/out",
	})

	for _, tt := range []struct{ flag, want string }{
		{"-m", "/models/ggml-large-v3-turbo.bin"},
		{"-f", "/tmp/audio.wav"},
		{"-of", "/tmp/out"},
	} {
		got, ok := argValue(args, tt.flag)
		if !ok || got != tt.want {
			t.Errorf("%s = %q (found=%v), want %q", tt.flag, got, ok, tt.want)
		}
	}
	for _, want := range []string{"-osrt", "-oj", "-pp"} {
		if !slices.Contains(args, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
	if strings.HasSuffix(mustArg(t, args, "-of"), ".srt") {
		t.Error("-of must be an extension-less base path")
	}
}

func mustArg(t *testing.T, args []string, flag string) string {
	t.Helper()
	v, ok := argValue(args, flag)
	if !ok {
		t.Fatalf("flag %q not found in %v", flag, args)
	}
	return v
}

// An empty Language means "let whisper decide", which the binary spells "auto".
func TestArgsDefaultsLanguageToAuto(t *testing.T) {
	if got := mustArg(t, Args(Options{}), "-l"); got != "auto" {
		t.Errorf("-l = %q, want auto", got)
	}
	if got := mustArg(t, Args(Options{Language: "en"}), "-l"); got != "en" {
		t.Errorf("-l = %q, want en", got)
	}
}

// Zero threads means "whisper picks", which it does when -t is absent. Passing
// "-t 0" instead would pin it to zero threads and hang.
func TestArgsOmitsThreadsWhenZero(t *testing.T) {
	if slices.Contains(Args(Options{}), "-t") {
		t.Error("-t must be omitted when Threads is 0")
	}
	if got := mustArg(t, Args(Options{Threads: 8}), "-t"); got != "8" {
		t.Errorf("-t = %q, want 8", got)
	}
}

// --vad without -vm makes whisper-cli exit immediately, so the VAD flags are
// all-or-nothing on the VAD model path.
func TestArgsEmitsVADOnlyWithAModel(t *testing.T) {
	plain := Args(Options{})
	for _, unwanted := range []string{"--vad", "-vm", "-vt"} {
		if slices.Contains(plain, unwanted) {
			t.Errorf("args must not contain %q without a VAD model: %v", unwanted, plain)
		}
	}

	withVAD := Args(Options{VADModel: "/models/ggml-silero-v5.1.2.bin", VADThreshold: 0.5})
	if !slices.Contains(withVAD, "--vad") {
		t.Errorf("args missing --vad: %v", withVAD)
	}
	if got := mustArg(t, withVAD, "-vm"); got != "/models/ggml-silero-v5.1.2.bin" {
		t.Errorf("-vm = %q", got)
	}
	if got := mustArg(t, withVAD, "-vt"); got != "0.5" {
		t.Errorf("-vt = %q, want 0.5", got)
	}
	if slices.Contains(Args(Options{VADModel: "/m.bin"}), "-vt") {
		t.Error("-vt must be omitted when no threshold is configured")
	}
}

// A configured path is taken on trust only if it exists; otherwise the user
// gets a clear error instead of exec's "file not found".
func TestFindBinaryPrefersConfiguredPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "my-whisper")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := FindBinary(bin)
	if err != nil {
		t.Fatalf("FindBinary: %v", err)
	}
	if got != bin {
		t.Errorf("FindBinary = %q, want %q", got, bin)
	}

	if _, err := FindBinary(filepath.Join(dir, "nope")); err == nil {
		t.Error("want error for a configured path that does not exist")
	}
}

// With nothing configured and nothing installed, the error must tell the user
// both ways out: install it, or point the config at it.
func TestFindBinaryErrorIsActionable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := FindBinary("")
	if err == nil {
		t.Fatal("want error when no binary can be found")
	}
	for _, want := range []string{"whisper-cli", "brew install", "config set whisper.bin"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestFindBinaryLocatesWhisperCliOnPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "whisper-cli")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	got, err := FindBinary("")
	if err != nil {
		t.Fatalf("FindBinary: %v", err)
	}
	if filepath.Base(got) != "whisper-cli" {
		t.Errorf("FindBinary = %q, want the whisper-cli on PATH", got)
	}
}

// whisper prints progress on stderr as "progress =  42%". The percentage is
// the only number worth surfacing; everything else is noise.
func TestParseProgress(t *testing.T) {
	tests := []struct {
		line string
		want int
		ok   bool
	}{
		{"whisper_print_progress_callback: progress =  42%", 42, true},
		{"whisper_print_progress_callback: progress = 100%", 100, true},
		{"progress=7%", 7, true},
		{"whisper_full_with_state: decoding", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		got, ok := parseProgress(tt.line)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("parseProgress(%q) = (%d, %v), want (%d, %v)", tt.line, got, ok, tt.want, tt.ok)
		}
	}
}

// The detected language comes from whisper's JSON sidecar rather than from
// scraping stderr, because the JSON is a stable, structured contract.
func TestReadDetectedLanguage(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "out")
	doc := `{"systeminfo":"x","params":{"language":"auto"},` +
		`"result":{"language":"ru"},"transcription":[]}`
	if err := os.WriteFile(base+".json", []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := readDetectedLanguage(base, "auto"); got != "ru" {
		t.Errorf("readDetectedLanguage = %q, want ru", got)
	}
	// No JSON written: fall back to what was requested rather than failing the
	// whole run over a missing convenience file.
	if got := readDetectedLanguage(filepath.Join(dir, "missing"), "en"); got != "en" {
		t.Errorf("fallback = %q, want en", got)
	}
}

func TestRunRequiresInstalledBinary(t *testing.T) {
	if _, err := os.Stat("/nonexistent-whisper"); err == nil {
		t.Skip("unexpected binary present")
	}
	_, err := Run(Options{Bin: "/nonexistent-whisper", Model: "m", Audio: "a", OutBase: "o"}, nil)
	if err == nil {
		t.Fatal("want error when the binary cannot be executed")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/whisper/ -v`
Expected: FAIL — the package does not exist yet.

- [ ] **Step 3: Write minimal implementation**

Create `internal/whisper/whisper.go`:

```go
// Package whisper runs the external whisper.cpp CLI to turn an audio file into
// an SRT transcript. The binary is invoked as a subprocess for the same reason
// ffmpeg is: it keeps the project free of cgo, so `make build-all` can still
// cross-compile to every platform from one machine.
package whisper

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

type Options struct {
	Bin      string // path to whisper-cli
	Model    string // path to the ggml transcription model
	VADModel string // path to a Silero VAD model; empty disables VAD
	Audio    string // 16 kHz mono WAV
	OutBase  string // output path WITHOUT extension; whisper appends .srt and .json
	Language string // ISO 639-1 code, or empty/"auto" to detect

	VADThreshold float64 // 0 leaves whisper's own default in place
	Threads      int     // 0 lets whisper choose
}

type Result struct {
	Language string // the language actually used, detected when Language was auto
	SRTPath  string // the transcript whisper wrote
}

// candidates are the names whisper.cpp has shipped its CLI under. Homebrew's
// formula installs "whisper-cli"; older builds and some distributions use
// "whisper-cpp".
var candidates = []string{"whisper-cli", "whisper-cpp"}

// FindBinary resolves the whisper executable: an explicitly configured path
// first, then PATH. The error is written to be actionable, since "executable
// file not found in $PATH" tells a user nothing about what to install.
func FindBinary(configured string) (string, error) {
	if c := strings.TrimSpace(configured); c != "" {
		if _, err := os.Stat(c); err != nil {
			return "", fmt.Errorf("whisper binary not found at %s "+
				"(set another with: sub-translator config set whisper.bin <path>)", c)
		}
		return c, nil
	}
	for _, name := range candidates {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("whisper-cli not found — install it (brew install whisper.cpp) " +
		"or point at it: sub-translator config set whisper.bin <path>")
}

// Args builds the whisper-cli argument list. It is pure so the entire flag
// contract can be tested without the binary installed.
func Args(o Options) []string {
	lang := strings.TrimSpace(o.Language)
	if lang == "" {
		lang = "auto"
	}

	args := []string{
		"-m", o.Model,
		"-f", o.Audio,
		"-of", o.OutBase,
		"-l", lang,
		"-osrt",
		"-oj",
		"-pp",
	}
	if o.Threads > 0 {
		args = append(args, "-t", strconv.Itoa(o.Threads))
	}
	// --vad is meaningless without a model, and whisper-cli exits if given one
	// without the other.
	if strings.TrimSpace(o.VADModel) != "" {
		args = append(args, "--vad", "-vm", o.VADModel)
		if o.VADThreshold > 0 {
			args = append(args, "-vt", strconv.FormatFloat(o.VADThreshold, 'g', -1, 64))
		}
	}
	return args
}

var progressRE = regexp.MustCompile(`progress\s*=\s*(\d+)%`)

func parseProgress(line string) (int, bool) {
	m := progressRE.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// readDetectedLanguage reads the language out of whisper's JSON sidecar. The
// file is a convenience, not the deliverable, so anything unreadable falls back
// to the language that was requested.
func readDetectedLanguage(outBase, requested string) string {
	data, err := os.ReadFile(outBase + ".json")
	if err != nil {
		return requested
	}
	var doc struct {
		Result struct {
			Language string `json:"language"`
		} `json:"result"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return requested
	}
	if lang := strings.TrimSpace(doc.Result.Language); lang != "" {
		return lang
	}
	return requested
}

// Run transcribes o.Audio and returns the SRT path plus the language used.
// Progress is reported as whole percentages as whisper emits them.
func Run(o Options, progress func(pct int)) (Result, error) {
	cmd := exec.Command(o.Bin, Args(o)...)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, fmt.Errorf("whisper: %w", err)
	}
	cmd.Stdout = io.Discard

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("whisper: %w", err)
	}

	// Keep the last few lines: when whisper fails, the reason is at the end of
	// its output, and dumping thousands of lines of model chatter helps nobody.
	var tail []string
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if pct, ok := parseProgress(line); ok {
			if progress != nil {
				progress(pct)
			}
			continue
		}
		if strings.TrimSpace(line) != "" {
			tail = append(tail, line)
			if len(tail) > 10 {
				tail = tail[1:]
			}
		}
	}

	if err := cmd.Wait(); err != nil {
		return Result{}, fmt.Errorf("whisper: %w\n%s", err, strings.Join(tail, "\n"))
	}

	srtPath := o.OutBase + ".srt"
	if _, err := os.Stat(srtPath); err != nil {
		return Result{}, fmt.Errorf("whisper produced no transcript at %s\n%s",
			srtPath, strings.Join(tail, "\n"))
	}
	return Result{
		Language: readDetectedLanguage(o.OutBase, o.Language),
		SRTPath:  srtPath,
	}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/whisper/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/whisper/
git commit -m "feat: add whisper.cpp runner with progress and language detection"
```

---

### Task 4: Audio stream selection and extraction

**Files:**
- Modify: `internal/media/media.go`
- Test: `internal/media/media_test.go`

**Interfaces:**
- Consumes: the existing `Stream` type and `normLang` helper in the same package.
- Produces, in package `media`:
  - `func AudioStreams(streams []Stream) []Stream`
  - `func FindAudioByLang(streams []Stream, lang string) (Stream, bool)`
  - `func ExtractAudio(input string, streamIndex int, outWAV string) error`
  - unexported `func extractAudioArgs(input string, streamIndex int, outWAV string) []string`

- [ ] **Step 1: Write the failing test**

Append to `internal/media/media_test.go`:

```go
func audioFixture() []Stream {
	mk := func(idx int, codecType, lang string) Stream {
		s := Stream{Index: idx, CodecType: codecType}
		s.Tags.Language = lang
		return s
	}
	return []Stream{
		mk(0, "video", ""),
		mk(1, "audio", "eng"),
		mk(2, "audio", "rus"),
		mk(3, "subtitle", "eng"),
	}
}

func TestAudioStreamsSelectsOnlyAudio(t *testing.T) {
	got := AudioStreams(audioFixture())
	if len(got) != 2 {
		t.Fatalf("got %d audio streams, want 2: %v", len(got), got)
	}
	for _, s := range got {
		if s.CodecType != "audio" {
			t.Errorf("stream #%d is %q, not audio", s.Index, s.CodecType)
		}
	}
}

// Audio tracks are tagged with ISO 639-2 in containers just as subtitles are,
// so a 2-letter -from code has to match a 3-letter tag.
func TestFindAudioByLangNormalizesCodes(t *testing.T) {
	tests := []struct {
		lang      string
		wantIndex int
		wantOK    bool
	}{
		{"en", 1, true},
		{"eng", 1, true},
		{"ru", 2, true},
		{"RU", 2, true},
		{"fr", 0, false},
	}
	for _, tt := range tests {
		got, ok := FindAudioByLang(audioFixture(), tt.lang)
		if ok != tt.wantOK {
			t.Errorf("FindAudioByLang(%q) ok = %v, want %v", tt.lang, ok, tt.wantOK)
			continue
		}
		if ok && got.Index != tt.wantIndex {
			t.Errorf("FindAudioByLang(%q) = #%d, want #%d", tt.lang, got.Index, tt.wantIndex)
		}
	}
}

// A subtitle track tagged eng must never be returned as an audio track.
func TestFindAudioByLangIgnoresNonAudioStreams(t *testing.T) {
	got, ok := FindAudioByLang(audioFixture(), "en")
	if !ok {
		t.Fatal("want a match")
	}
	if got.CodecType != "audio" {
		t.Errorf("returned a %q stream", got.CodecType)
	}
}

// whisper.cpp's bundled decoder reads 16 kHz mono signed-16-bit PCM WAV and
// nothing else. Any deviation here produces "read_wav: unsupported format".
func TestExtractAudioArgsProduceWhisperReadyWAV(t *testing.T) {
	args := extractAudioArgs("in.mkv", 2, "out.wav")

	for _, pair := range [][2]string{
		{"-map", "0:2"},
		{"-ac", "1"},
		{"-ar", "16000"},
		{"-c:a", "pcm_s16le"},
	} {
		i := slices.Index(args, pair[0])
		if i < 0 || i+1 >= len(args) || args[i+1] != pair[1] {
			t.Errorf("want %s %s\ngot: %v", pair[0], pair[1], args)
		}
	}
	if !slices.Contains(args, "-vn") {
		t.Errorf("want -vn to drop the video stream\ngot: %v", args)
	}
	if args[len(args)-1] != "out.wav" {
		t.Errorf("output must be the last arg, got %q", args[len(args)-1])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/media/ -v`
Expected: FAIL — `AudioStreams`, `FindAudioByLang` and `extractAudioArgs` are undefined.

- [ ] **Step 3: Write minimal implementation**

In `internal/media/media.go`, replace the existing `FindSubtitleByLang` with a version delegating to a shared helper, and add the audio functions after it:

```go
// findByLang returns the first stream of the given codec type whose language
// tag matches lang once both are normalised to ISO 639-2.
func findByLang(streams []Stream, lang, codecType string) (Stream, bool) {
	norm := normLang(lang)
	for _, s := range streams {
		if s.CodecType == codecType && normLang(s.Tags.Language) == norm {
			return s, true
		}
	}
	return Stream{}, false
}

func FindSubtitleByLang(streams []Stream, lang string) (Stream, bool) {
	return findByLang(streams, lang, "subtitle")
}

func AudioStreams(streams []Stream) []Stream {
	var out []Stream
	for _, s := range streams {
		if s.CodecType == "audio" {
			out = append(out, s)
		}
	}
	return out
}

func FindAudioByLang(streams []Stream, lang string) (Stream, bool) {
	return findByLang(streams, lang, "audio")
}

// ExtractAudio decodes one audio stream to the only format whisper.cpp's
// bundled WAV reader accepts: 16 kHz mono signed 16-bit PCM. Whisper resamples
// to exactly this internally, so nothing is lost by doing it here.
func ExtractAudio(input string, streamIndex int, outWAV string) error {
	cmd := exec.Command("ffmpeg", extractAudioArgs(input, streamIndex, outWAV)...)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func extractAudioArgs(input string, streamIndex int, outWAV string) []string {
	return []string{
		"-y",
		"-i", input,
		"-map", fmt.Sprintf("0:%d", streamIndex),
		"-vn",
		"-ac", "1",
		"-ar", "16000",
		"-c:a", "pcm_s16le",
		outWAV,
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/media/ -v`
Expected: PASS, including the pre-existing mux tests.

- [ ] **Step 5: Commit**

```bash
git add internal/media/
git commit -m "feat: add audio stream selection and whisper-ready extraction"
```

---

### Task 5: Source mode

**Files:**
- Create: `source.go`
- Test: `source_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces, in package `main`:
  - `type sourceMode int` with `sourceAuto`, `sourceSub`, `sourceAudio`
  - `func (s sourceMode) String() string`
  - `func parseSource(s string) (sourceMode, error)`
  - `func resolveSource(s sourceMode, hasSubs, interactive bool) (mode sourceMode, needsConfirm bool, err error)`

- [ ] **Step 1: Write the failing test**

Create `source_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run TestParseSource -v`
Expected: FAIL — `sourceMode` is undefined.

- [ ] **Step 3: Write minimal implementation**

Create `source.go`:

```go
package main

import (
	"fmt"
	"strings"
)

// sourceMode selects where the subtitle text comes from.
type sourceMode int

const (
	// sourceAuto reads an existing subtitle track when the file has one and
	// otherwise offers to transcribe the audio.
	sourceAuto sourceMode = iota
	// sourceSub reads an existing subtitle track and fails if there is none.
	sourceSub
	// sourceAudio transcribes the audio with whisper, ignoring any subtitle
	// tracks the file may already carry.
	sourceAudio
)

func (s sourceMode) String() string {
	switch s {
	case sourceSub:
		return "sub"
	case sourceAudio:
		return "audio"
	default:
		return "auto"
	}
}

func parseSource(s string) (sourceMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "auto":
		return sourceAuto, nil
	case "sub":
		return sourceSub, nil
	case "audio":
		return sourceAudio, nil
	default:
		return 0, fmt.Errorf("invalid -source %q: want sub, audio or auto", s)
	}
}

// resolveSource turns auto into a concrete choice. It returns the mode to use
// and whether the user still has to approve it: transcription is expensive
// enough that auto must never start one silently.
func resolveSource(s sourceMode, hasSubs, interactive bool) (sourceMode, bool, error) {
	if s != sourceAuto {
		return s, false, nil
	}
	if hasSubs {
		return sourceSub, false, nil
	}
	if !interactive {
		return 0, false, fmt.Errorf(
			"no subtitle tracks, and -source auto cannot ask for confirmation here; " +
				"pass -source audio to transcribe the audio track")
	}
	return sourceAudio, true, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test . -v`
Expected: PASS, including the existing mode and ui tests.

- [ ] **Step 5: Commit**

```bash
git add source.go source_test.go
git commit -m "feat: add source mode selecting subtitles or audio transcription"
```

---

### Task 6: Confirmation prompt and terminal detection

**Files:**
- Modify: `ui.go`
- Test: `ui_test.go`

**Interfaces:**
- Consumes: the existing package-level `stdin` reader and `fatalf` in `main`.
- Produces, in package `main`:
  - `func readConfirm(r *bufio.Reader, w io.Writer, question string) (bool, error)`
  - `func confirm(question string) bool`
  - `func stdinIsTerminal() bool`

- [ ] **Step 1: Write the failing test**

Append to `ui_test.go`:

```go
func TestReadConfirmAcceptsYesForms(t *testing.T) {
	for _, in := range []string{"y\n", "Y\n", "yes\n", "  yes  \n"} {
		var out strings.Builder
		got, err := readConfirm(bufio.NewReader(strings.NewReader(in)), &out, "Transcribe?")
		if err != nil {
			t.Errorf("readConfirm(%q): %v", in, err)
			continue
		}
		if !got {
			t.Errorf("readConfirm(%q) = false, want true", in)
		}
	}
}

// Anything that is not an explicit yes means no. Transcription is expensive and
// must not start on a stray keypress.
func TestReadConfirmTreatsEverythingElseAsNo(t *testing.T) {
	for _, in := range []string{"n\n", "no\n", "\n", "maybe\n"} {
		var out strings.Builder
		got, err := readConfirm(bufio.NewReader(strings.NewReader(in)), &out, "Transcribe?")
		if err != nil {
			t.Errorf("readConfirm(%q): %v", in, err)
			continue
		}
		if got {
			t.Errorf("readConfirm(%q) = true, want false", in)
		}
	}
}

func TestReadConfirmShowsTheQuestion(t *testing.T) {
	var out strings.Builder
	if _, err := readConfirm(bufio.NewReader(strings.NewReader("y\n")), &out, "Transcribe?"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Transcribe?") {
		t.Errorf("prompt %q does not contain the question", out.String())
	}
}

func TestReadConfirmErrorsOnExhaustedInput(t *testing.T) {
	var out strings.Builder
	if _, err := readConfirm(bufio.NewReader(strings.NewReader("")), &out, "Transcribe?"); err == nil {
		t.Error("want an error when there is no answer to read")
	}
}
```

Check the existing import block of `ui_test.go` and add `"bufio"` and `"strings"` if they are not already there.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run TestReadConfirm -v`
Expected: FAIL — `readConfirm` is undefined.

- [ ] **Step 3: Write minimal implementation**

Append to `ui.go`:

```go
// readConfirm asks a yes/no question and reads the answer from r. Only an
// explicit yes counts: the caller uses this to guard work measured in minutes
// of full-load CPU, where the safe default is to do nothing.
func readConfirm(r *bufio.Reader, w io.Writer, question string) (bool, error) {
	fmt.Fprintf(w, "%s [y/N]: ", question)
	line, err := r.ReadString('\n')
	answer := strings.ToLower(strings.TrimSpace(line))
	if answer == "" && err != nil {
		fmt.Fprintln(w)
		return false, fmt.Errorf("no answer given")
	}
	return answer == "y" || answer == "yes", nil
}

// confirm is the interactive wrapper used by main: it reads from the shared
// stdin and prompts on stderr so piped stdout stays clean.
func confirm(question string) bool {
	ok, err := readConfirm(stdin, os.Stderr, question)
	if err != nil {
		fatalf("%v", err)
	}
	return ok
}

// stdinIsTerminal reports whether there is a human to answer a prompt. When
// stdin is a pipe or /dev/null, asking a question would block or silently read
// EOF, so callers must decide without one.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test . -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add ui.go ui_test.go
git commit -m "feat: add yes/no confirmation prompt and terminal detection"
```

---

### Task 7: config and model subcommands

**Files:**
- Create: `cmd_config.go`
- Create: `cmd_model.go`
- Test: `cmd_test.go`
- Modify: `main.go` (dispatch only; the pipeline is Task 8)

**Interfaces:**
- Consumes: `internal/config` (Task 1) and `internal/models` (Task 2).
- Produces, in package `main`:
  - `func runConfig(args []string, out io.Writer) error`
  - `func runModel(args []string, out io.Writer) error`
  - `func dispatchSubcommand(args []string, out io.Writer) (handled bool, err error)`

- [ ] **Step 1: Write the failing test**

Create `cmd_test.go`:

```go
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

func TestRunConfigRejectsBadUsage(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	for _, args := range [][]string{
		{},
		{"frobnicate"},
		{"set", "whisper.threads"},
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test . -run 'TestRunConfig|TestRunModel|TestDispatch' -v`
Expected: FAIL — `runConfig`, `runModel` and `dispatchSubcommand` are undefined.

- [ ] **Step 3: Write minimal implementation**

Create `cmd_config.go`:

```go
package main

import (
	"fmt"
	"io"

	"github.com/artschekoff/sub-translator/internal/config"
)

const configUsage = `Usage:
  sub-translator config list              show every setting and its value
  sub-translator config get <key>         print one setting
  sub-translator config set <key> <value> change one setting
  sub-translator config path              print the config file location`

func runConfig(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("config: missing subcommand\n\n%s", configUsage)
	}

	switch args[0] {
	case "path":
		p, err := config.Path()
		if err != nil {
			return err
		}
		fmt.Fprintln(out, p)
		return nil

	case "list":
		c, err := config.Load()
		if err != nil {
			return err
		}
		for _, key := range config.Keys() {
			value, err := c.Get(key)
			if err != nil {
				return err
			}
			if value == "" {
				value = "(unset)"
			}
			fmt.Fprintf(out, "  %-22s %s\n", key, value)
		}
		return nil

	case "get":
		if len(args) != 2 {
			return fmt.Errorf("config get: want exactly one key\n\n%s", configUsage)
		}
		c, err := config.Load()
		if err != nil {
			return err
		}
		value, err := c.Get(args[1])
		if err != nil {
			return err
		}
		fmt.Fprintln(out, value)
		return nil

	case "set":
		if len(args) != 3 {
			return fmt.Errorf("config set: want a key and a value\n\n%s", configUsage)
		}
		c, err := config.Load()
		if err != nil {
			return err
		}
		if err := c.Set(args[1], args[2]); err != nil {
			return err
		}
		if err := c.Save(); err != nil {
			return err
		}
		value, _ := c.Get(args[1])
		fmt.Fprintf(out, "%s = %s\n", args[1], value)
		return nil

	default:
		return fmt.Errorf("config: unknown subcommand %q\n\n%s", args[0], configUsage)
	}
}
```

Create `cmd_model.go`:

```go
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/artschekoff/sub-translator/internal/config"
	"github.com/artschekoff/sub-translator/internal/models"
)

const modelUsage = `Usage:
  sub-translator model list          show available models and which are installed
  sub-translator model pull <name>   download a model`

func runModel(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("model: missing subcommand\n\n%s", modelUsage)
	}

	switch args[0] {
	case "list":
		return listModels(out)

	case "pull":
		if len(args) != 2 {
			return fmt.Errorf("model pull: want exactly one model name\n\n%s", modelUsage)
		}
		return pullModel(args[1], out)

	default:
		return fmt.Errorf("model: unknown subcommand %q\n\n%s", args[0], modelUsage)
	}
}

func listModels(out io.Writer) error {
	c, err := config.Load()
	if err != nil {
		return err
	}
	dirs := models.SearchDirs(c.Models.Dir)
	transcribe, vad := models.Discover(dirs)

	installed := map[string]string{}
	for _, p := range append(append([]string{}, transcribe...), vad...) {
		installed[filepath.Base(p)] = p
	}

	fmt.Fprintln(out, "Available models:")
	for _, m := range models.Catalog() {
		status := ""
		if path, ok := installed[m.Filename]; ok {
			status = "installed: " + path
		}
		fmt.Fprintf(out, "  %-16s %-8s %s\n", m.Name, m.Kind, status)
	}

	fmt.Fprintln(out, "\nSearched directories:")
	for _, d := range dirs {
		fmt.Fprintf(out, "  %s\n", d)
	}
	return nil
}

func pullModel(name string, out io.Writer) error {
	m, ok := models.Find(name)
	if !ok {
		return fmt.Errorf("unknown model %q — run `sub-translator model list` to see the options", name)
	}

	c, err := config.Load()
	if err != nil {
		return err
	}
	dest := c.Models.Dir
	if dest == "" {
		if dest, err = models.DefaultDir(); err != nil {
			return err
		}
	}

	fmt.Fprintf(out, "Downloading %s → %s\n", m.Name, filepath.Join(dest, m.Filename))
	lastPct := -1
	path, err := models.Pull(m, dest, func(done, total int64) {
		if total <= 0 {
			return
		}
		if pct := int(done * 100 / total); pct != lastPct {
			lastPct = pct
			fmt.Fprintf(os.Stderr, "\r  progress: %d%% (%d/%d MB)   ",
				pct, done/(1<<20), total/(1<<20))
		}
	})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Saved: %s\n", path)
	return nil
}

// dispatchSubcommand runs a subcommand when args start with one. It reports
// whether it handled the call, so main can fall through to normal flag parsing
// for an ordinary translation run.
func dispatchSubcommand(args []string, out io.Writer) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case "config":
		return true, runConfig(args[1:], out)
	case "model":
		return true, runModel(args[1:], out)
	default:
		return false, nil
	}
}
```

In `main.go`, add the dispatch as the first thing `main` does, before `flag.String` declarations:

```go
func main() {
	if handled, err := dispatchSubcommand(os.Args[1:], os.Stdout); handled {
		if err != nil {
			fatalf("%v", err)
		}
		return
	}

	from := flag.String("from", "", "source language (required)")
	// ... rest unchanged for now
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test . -v`

Then build and confirm the already-present models are reported:

```bash
go build -o /tmp/st . && /tmp/st model list
```

```
  large-v3-turbo   transcribe installed: /Users/<you>/Library/Application Support/net.slaive.app/models/ggml-large-v3-turbo.bin
  silero-vad       vad        installed: /Users/<you>/Library/Application Support/net.slaive.app/models/ggml-silero-v5.1.2.bin
```

Expected: tests PASS and the two already-present models are reported as installed.

- [ ] **Step 5: Commit**

```bash
git add cmd_config.go cmd_model.go cmd_test.go main.go
git commit -m "feat: add config and model subcommands"
```

---

### Task 8: Wire transcription into the pipeline

**Files:**
- Modify: `main.go`

**Interfaces:**
- Consumes: `resolveSource`/`parseSource` (Task 5), `confirm`/`stdinIsTerminal` (Task 6), `config` (Task 1), `models` (Task 2), `whisper` (Task 3), `media.AudioStreams`/`FindAudioByLang`/`ExtractAudio` (Task 4).
- Produces: no new exported API. Internally:
  - `func resolveWhisper(c *config.Config, modelFlag, binFlag, vadFlag string) (whisper.Options, error)`
  - `func transcribe(input string, atrack int, from string, opts whisper.Options) (blocks []srt.Block, lang string, err error)`

This task has no unit test of its own: it is orchestration over units already
covered, and its verification is the manual acceptance run in Task 10.

- [ ] **Step 1: Add whisper resolution**

Append to `main.go`:

```go
// resolveWhisper assembles the whisper settings from flags, config and
// discovery, in that order of precedence, and fails with an actionable message
// when a required piece is missing.
func resolveWhisper(c *config.Config, modelFlag, binFlag, vadFlag string) (whisper.Options, error) {
	bin, err := whisper.FindBinary(firstNonEmpty(binFlag, c.Whisper.Bin))
	if err != nil {
		return whisper.Options{}, err
	}

	dirs := models.SearchDirs(c.Models.Dir)
	found, vads := models.Discover(dirs)

	model := firstNonEmpty(modelFlag, c.Whisper.Model)
	switch {
	case model != "":
		if _, err := os.Stat(model); err != nil {
			return whisper.Options{}, fmt.Errorf("whisper model not found: %s", model)
		}
	case len(found) == 1:
		model = found[0]
	case len(found) > 1:
		var b strings.Builder
		fmt.Fprintf(&b, "several whisper models found; pick one with -whisper-model "+
			"or `sub-translator config set whisper.model <path>`:\n")
		for _, m := range found {
			fmt.Fprintf(&b, "  %s\n", m)
		}
		return whisper.Options{}, errors.New(b.String())
	default:
		return whisper.Options{}, fmt.Errorf(
			"no whisper model found — download one (sub-translator model pull large-v3-turbo) " +
				"or set it: sub-translator config set whisper.model <path>")
	}

	// VAD is optional: whisper works without it, but on a feature film it keeps
	// the decoder from looping over long silences.
	vad := firstNonEmpty(vadFlag, c.Whisper.VADModel)
	if vad == "" && len(vads) > 0 {
		vad = vads[0]
	}

	return whisper.Options{
		Bin:          bin,
		Model:        model,
		VADModel:     vad,
		VADThreshold: c.Whisper.VADThreshold,
		Threads:      c.Whisper.Threads,
		Language:     c.Whisper.Language,
	}, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
```

- [ ] **Step 2: Add the transcription step**

Append to `main.go`:

```go
// transcribe extracts one audio stream, runs whisper over it, and returns the
// parsed transcript together with the language whisper used. Temporary files
// live in a directory of their own: a feature film's 16 kHz WAV is over a
// gigabyte and must not be left behind.
func transcribe(input string, streams []media.Stream, atrack int, from string, opts whisper.Options) ([]srt.Block, string, error) {
	audio := media.AudioStreams(streams)
	if len(audio) == 0 {
		return nil, "", fmt.Errorf("no audio tracks in %s — nothing to transcribe", filepath.Base(input))
	}

	var src media.Stream
	switch {
	case atrack >= 0:
		found := false
		for _, s := range audio {
			if s.Index == atrack {
				src, found = s, true
				break
			}
		}
		if !found {
			return nil, "", fmt.Errorf("no audio stream at index %d", atrack)
		}
	case from != "":
		if s, ok := media.FindAudioByLang(audio, from); ok {
			src = s
		} else {
			src = audio[0]
		}
	default:
		src = audio[0]
	}
	fmt.Printf("Audio:  #%d  lang=%s  %q\n", src.Index, src.Tags.Language, src.Tags.Title)

	tmpDir, err := os.MkdirTemp("", "sub-translator-*")
	if err != nil {
		return nil, "", fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	wav := filepath.Join(tmpDir, "audio.wav")
	fmt.Printf("Extracting audio track #%d...\n", src.Index)
	if err := media.ExtractAudio(input, src.Index, wav); err != nil {
		return nil, "", fmt.Errorf("extract audio: %w", err)
	}

	opts.Audio = wav
	opts.OutBase = filepath.Join(tmpDir, "transcript")
	if from != "" {
		opts.Language = from
	}

	fmt.Printf("Transcribing with %s...\n", filepath.Base(opts.Model))
	if opts.VADModel != "" {
		fmt.Printf("  VAD: %s\n", filepath.Base(opts.VADModel))
	}
	result, err := whisper.Run(opts, func(pct int) {
		fmt.Printf("\r  progress: %d%%   ", pct)
	})
	fmt.Println()
	if err != nil {
		return nil, "", err
	}

	blocks, err := srt.Parse(result.SRTPath)
	if err != nil {
		return nil, "", fmt.Errorf("parse transcript: %w", err)
	}
	if len(blocks) == 0 {
		return nil, "", fmt.Errorf("transcript is empty — the audio track may be silent")
	}
	return blocks, result.Language, nil
}
```

- [ ] **Step 3: Rewrite the source-selection section of main**

Replace the block in `main.go` that currently runs from the `subs := media.SubtitleStreams(streams)` line through the `fmt.Printf("Parsed %d subtitle blocks\n", len(blocks))` line with:

```go
	subs := media.SubtitleStreams(streams)
	if len(subs) > 0 {
		// Many releases ship untagged tracks, and the listing is what tells you
		// which -track to pass.
		fmt.Print(formatTracks(subs))
	}

	source, needsConfirm, err := resolveSource(source, len(subs) > 0, stdinIsTerminal())
	if err != nil {
		fatalf("%v", err)
	}
	if needsConfirm {
		fmt.Printf("No subtitle tracks in %s.\n", filepath.Base(input))
		if !confirm("Transcribe the audio track with whisper? This can take a while") {
			fatalf("nothing to do")
		}
	}

	*from = strings.TrimSpace(*from)
	*to = strings.TrimSpace(*to)

	// In audio mode whisper detects the source language, so only the target is
	// genuinely required.
	if *from == "" && source == sourceSub {
		*from = promptLang("Source")
	}
	if *to == "" {
		*to = promptLang("Target")
	}

	var blocks []srt.Block

	if source == sourceAudio {
		cfg, err := config.Load()
		if err != nil {
			fatalf("%v", err)
		}
		opts, err := resolveWhisper(cfg, *whisperModel, *whisperBin, *vadModel)
		if err != nil {
			fatalf("%v", err)
		}

		var lang string
		blocks, lang, err = transcribe(input, streams, *atrack, *from, opts)
		if err != nil {
			fatalf("%v", err)
		}
		if *from == "" {
			*from = lang
			fmt.Printf("Detected language: %s\n", lang)
		}

		// Transcription is by far the most expensive step here; keeping its
		// output costs one small file and saves repeating it.
		transcriptPath := media.DefaultSRTPath(input, *from)
		if err := srt.Write(transcriptPath, blocks); err != nil {
			fatalf("write transcript: %v", err)
		}
		fmt.Printf("Saved transcript: %s\n", transcriptPath)
	} else {
		if len(subs) == 0 {
			fatalf("no subtitle tracks in %s — nothing to translate", filepath.Base(input))
		}

		var srcStream media.Stream
		if *track >= 0 {
			found := false
			for _, s := range subs {
				if s.Index == *track {
					srcStream = s
					found = true
					break
				}
			}
			if !found {
				fatalf("no subtitle stream at index %d", *track)
			}
		} else {
			var ok bool
			srcStream, ok = media.FindSubtitleByLang(subs, *from)
			if !ok {
				fatalf("no subtitle track tagged lang=%s — pick one from the list above with -track", *from)
			}
		}
		fmt.Printf("Source: #%d  lang=%s  %q\n", srcStream.Index, srcStream.Tags.Language, srcStream.Tags.Title)

		tmpSRT := filepath.Join(os.TempDir(), "sub_translator_src.srt")
		defer os.Remove(tmpSRT)
		fmt.Printf("Extracting subtitle track #%d...\n", srcStream.Index)
		if err := media.ExtractSubtitle(input, srcStream.Index, tmpSRT); err != nil {
			fatalf("extract: %v", err)
		}

		blocks, err = srt.Parse(tmpSRT)
		if err != nil {
			fatalf("parse SRT: %v", err)
		}
		if len(blocks) == 0 {
			fatalf("subtitle track #%d is empty — nothing to translate", srcStream.Index)
		}
	}
	fmt.Printf("Parsed %d subtitle blocks\n", len(blocks))
```

Declare the new flags next to the existing ones:

```go
	sourceFlag := flag.String("source", "auto", "subtitle source: sub, audio or auto")
	atrack := flag.Int("atrack", -1, "audio stream index (-1 = auto)")
	whisperModel := flag.String("whisper-model", "", "path to a whisper ggml model")
	whisperBin := flag.String("whisper-bin", "", "path to the whisper-cli binary")
	vadModel := flag.String("vad-model", "", "path to a Silero VAD model")
```

and parse it beside the output mode:

```go
	source, err := parseSource(*sourceFlag)
	if err != nil {
		fatalf("%v", err)
	}
```

Update the import block to add `errors`, `github.com/artschekoff/sub-translator/internal/config`, `github.com/artschekoff/sub-translator/internal/models` and `github.com/artschekoff/sub-translator/internal/whisper`.

Also update the `usage` constant: add the subcommand lines and the new flags.

```go
const usage = `sub-translator — subtitle translator for MKV, MP4, AVI and more

Usage:
  sub-translator [flags] <input>
  sub-translator config <list|get|set|path> [key] [value]
  sub-translator model <list|pull> [name]

Subtitle sources (-source):
  sub    read a subtitle track already in the file
  audio  transcribe the audio track with whisper.cpp
  auto   use a subtitle track when there is one, otherwise offer to transcribe (default)

Output modes (-mode):
  srt   write a translated .srt next to the input video (default)
  mux   write a new video file with the translated track embedded
  both  write the muxed video file and the .srt

Supported formats:
  MKV, MP4/M4V/MOV  — translated track embedded into output file
  AVI               — embedded subs not supported; -mode mux/both fall back to srt

Transcription needs whisper.cpp (brew install whisper.cpp) and a ggml model.
Models already downloaded by other whisper.cpp front-ends are found
automatically; otherwise run: sub-translator model pull large-v3-turbo

Flags:
  -from          source language code (prompted for if omitted; detected in audio mode)
  -to            target language code (required; prompted for if omitted)
  -source        subtitle source: sub, audio or auto (default: auto)
  -track         subtitle stream index, -1 = auto-detect by -from lang (default: -1)
  -atrack        audio stream index, -1 = auto (default: -1)
  -mode          output mode: srt, mux or both (default: srt)
  -out           output path (.srt in srt mode, container otherwise)
  -whisper-model path to a whisper ggml model
  -whisper-bin   path to the whisper-cli binary
  -vad-model     path to a Silero VAD model
  -version       print version and exit

Examples:
  sub-translator -to es movie.mkv
  sub-translator -to es -source audio movie.mkv
  sub-translator -to fr -mode mux movie.mp4
  sub-translator -to ru -track 3 movie.mkv
  sub-translator config set whisper.model ~/models/ggml-large-v3-turbo.bin
  sub-translator model pull large-v3-turbo
`
```

- [ ] **Step 4: Verify the build and the existing tests**

Run: `make validate`
Expected: gofmt clean, vet clean, all tests PASS.

Then check the two failure paths that need no model:

```bash
go build -o /tmp/st .
/tmp/st -to es -source sub /path/to/a/file/without/subs.mkv   # existing error message
/tmp/st -to es -source auto < /dev/null /path/to/that/file    # must say "pass -source audio"
```

- [ ] **Step 5: Commit**

```bash
git add main.go
git commit -m "feat: transcribe audio with whisper when no subtitle track exists"
```

---

### Task 9: Documentation

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Update the feature list and problem table**

In the "The Problem" table, add a row:

```markdown
| **No subtitles at all.** A rip with a single audio track and no subtitle stream leaves subtitle tools with nothing to work on. | Transcribes the audio with whisper.cpp, then translates the transcript — the same one command. |
```

In "Features", add:

```markdown
- **Audio transcription** — no subtitle track? whisper.cpp transcribes the audio and the transcript is translated like any other subtitle file
- **Shared model store** — reuses `ggml-*.bin` models other whisper.cpp front-ends already downloaded, so a 1.6 GB model is never fetched twice
- **Remembered settings** — binary and model paths are configured once with `sub-translator config set`
```

- [ ] **Step 2: Add a Transcription section after "Usage"**

```markdown
## Transcription

When a video has no subtitle track, `sub-translator` can generate one from the audio using [whisper.cpp](https://github.com/ggml-org/whisper.cpp).

**Prerequisites:**

```bash
brew install whisper.cpp              # provides the whisper-cli binary
sub-translator model pull large-v3-turbo
```

`model pull` downloads to `~/Library/Application Support/sub-translator/models` (macOS) or `~/.local/share/sub-translator/models` (Linux). If another whisper.cpp front-end has already downloaded a model, it is found and reused — the files use the same `ggml-<name>.bin` format and these locations are searched:

1. `models.dir` from the config, when set
2. `~/Library/Application Support/sub-translator/models`
3. `~/Library/Application Support/net.slaive.app/models` (macOS)
4. `~/.cache/whisper.cpp`
5. `./models`

**Usage:**

```bash
# auto: uses the subtitle track if there is one, otherwise offers to transcribe
sub-translator -to es movie.mkv

# force transcription even when subtitle tracks exist
sub-translator -to es -source audio movie.mkv

# pick a specific audio track and skip language detection
sub-translator -to es -source audio -atrack 1 -from en movie.mkv
```

The source language is detected automatically, so `-from` is optional here. Passing it is faster and more reliable when you already know it.

The untranslated transcript is saved next to the video as `<video>.<lang>.srt`, so a second translation into another language does not re-run transcription.

**Voice activity detection** is enabled automatically when a Silero VAD model is present. It stops whisper looping over long silent passages, which matters on feature-length input:

```bash
sub-translator model pull silero-vad
```

## Configuration

Settings live in `~/.config/sub-translator/config.json` and are managed from the command line:

```bash
sub-translator config list
sub-translator config set whisper.model ~/models/ggml-large-v3-turbo.bin
sub-translator config set whisper.bin /opt/homebrew/bin/whisper-cli
sub-translator config get whisper.model
sub-translator config path
```

| Key | Meaning |
|---|---|
| `whisper.bin` | Path to `whisper-cli`. Found on `$PATH` when unset. |
| `whisper.model` | Path to a ggml transcription model. Discovered when unset. |
| `whisper.vad-model` | Path to a Silero VAD model. Discovered when unset. |
| `whisper.vad-threshold` | Speech detection threshold, 0–1. whisper's default when unset. |
| `whisper.threads` | Threads for transcription. whisper chooses when unset. |
| `whisper.language` | Default source language. `auto` when unset. |
| `models.dir` | Where `model pull` writes, and the first directory searched. |

The corresponding flags — `-whisper-model`, `-whisper-bin`, `-vad-model` — override the config for a single run.
```

- [ ] **Step 3: Update the flags block in "Usage"**

Mirror the `usage` constant from Task 8 so the README and `-h` agree.

- [ ] **Step 4: Verify**

Run: `make validate`
Expected: PASS. Read the rendered README and confirm no example uses a flag that does not exist.

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "docs: document whisper transcription, config and model commands"
```

---

### Task 10: Acceptance run

**Files:** none — this task verifies, it does not change code.

The acceptance criterion set by the project owner: the program runs, subtitles are generated from the audio track using whisper, and those subtitles can be translated into Spanish.

- [ ] **Step 1: Install whisper.cpp**

```bash
brew install whisper.cpp
whisper-cli --help | head -3
```

- [ ] **Step 2: Confirm the existing models are found**

```bash
go build -o /tmp/st .
/tmp/st model list
```

Expected: `large-v3-turbo` and `silero-vad` both shown as installed, from the
`net.slaive.app` directory, with no configuration having been written.

- [ ] **Step 3: Cut a short clip**

A full film takes far too long for a first verification, so transcribe three minutes of it:

```bash
FILM="$HOME/Downloads/Бойцовский клуб (1999) WEBRip-AVC [Open Matte]/Бойцовский клуб (1999) WEBRip-AVC [Open Matte].mkv"
ffprobe -v error -show_entries stream=index,codec_type,codec_name:stream_tags=language -of compact "$FILM"
ffmpeg -y -ss 00:12:00 -t 00:03:00 -i "$FILM" -map 0:v:0 -map 0:a:0 -c copy /tmp/clip.mkv
```

Record whether the film has subtitle tracks; the `-source auto` path depends on it.

- [ ] **Step 4: Transcribe and translate the clip**

```bash
/tmp/st -to es -source audio /tmp/clip.mkv
```

Expected: the audio track is reported, extraction runs, whisper reports progress
to 100%, the detected language is printed, `/tmp/clip.<lang>.srt` is written,
translation progress runs to 100%, and `/tmp/clip.es.srt` is written.

- [ ] **Step 5: Verify the output is real**

```bash
head -20 /tmp/clip.es.srt
grep -c '^[0-9]\+$' /tmp/clip.es.srt          # block count > 0
diff <(grep -- '-->' /tmp/clip.*.srt | head -5) /dev/null   # timings present
```

Expected: sequential block numbers, `HH:MM:SS,mmm --> HH:MM:SS,mmm` timings
matching the source transcript exactly, and Spanish dialogue text.

- [ ] **Step 6: Verify the auto path on a file with no subtitles**

```bash
/tmp/st -to es /tmp/clip.mkv
```

Expected: the tool reports no subtitle tracks and asks for confirmation before
transcribing. Answering `n` exits without work.

- [ ] **Step 7: Report**

Report to the project owner: the commands run, the observed output, the head of
the Spanish SRT, and the wall-clock time of the three-minute transcription as a
basis for estimating the full film.

---

## Self-Review

**Spec coverage.** Every section of the spec maps to a task: `internal/config` → Task 1; `internal/models`, model compatibility and the search order → Task 2; `internal/whisper` and the whisper.cpp flag table → Task 3; `internal/media` additions → Task 4; the `-source` contract and `auto` resolution → Task 5; the interactive confirmation and non-terminal failure → Task 6; the subcommand CLI → Task 7; data flow, language handling, transcript retention, error handling and temp-file cleanup → Task 8; documentation → Task 9; manual acceptance → Task 10.

**Type consistency.** `whisper.Options` field names are identical in Tasks 3 and 8. `models.SearchDirs(configuredDir)` takes one argument everywhere. `resolveSource` returns `(sourceMode, bool, error)` in both its definition (Task 5) and its call site (Task 8). `config.Keys`, `Get` and `Set` are used with the same signatures in Tasks 1 and 7.

**Known gap.** Task 8 has no unit test, by design: it is orchestration over units already covered, and Go cannot easily test a `main` that calls `os.Exit`. Task 10 is its verification.
