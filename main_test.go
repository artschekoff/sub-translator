package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/artschekoff/sub-translator/internal/config"
	"github.com/artschekoff/sub-translator/internal/translate"
)

// touch creates an empty file, making the parent directories as needed.
// resolveWhisper only ever stats these paths, so the contents do not matter.
func touch(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("ggml"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestResolveWhisper covers the branch's central cross-cutting decision: flag
// beats config, config beats discovery, discovery only decides when it finds
// exactly one candidate.
//
// The search path SearchDirs builds reaches into $HOME and ./models, so the
// test rewrites both rather than the function: pointing HOME and the working
// directory at a fresh temp tree makes discovery see only what each case puts
// there, whatever models the developer running the test happens to own.
func TestResolveWhisper(t *testing.T) {
	tests := []struct {
		name string
		// setup prepares the temp tree and returns the config and the
		// -whisper-model flag value for this case.
		setup func(t *testing.T, dir string) (*config.Config, string)
		// want is the model path resolveWhisper must choose, relative to dir.
		want string
		// wantErr are substrings the error must contain; empty means success.
		wantErr []string
	}{
		{
			name: "flag beats config",
			setup: func(t *testing.T, dir string) (*config.Config, string) {
				touch(t, filepath.Join(dir, "cfg", "ggml-config.bin"))
				flagModel := touch(t, filepath.Join(dir, "flag", "ggml-flag.bin"))
				return &config.Config{
					Whisper: config.WhisperConfig{Model: filepath.Join(dir, "cfg", "ggml-config.bin")},
				}, flagModel
			},
			want: "flag/ggml-flag.bin",
		},
		{
			name: "config beats discovery",
			setup: func(t *testing.T, dir string) (*config.Config, string) {
				touch(t, filepath.Join(dir, "cfg", "ggml-config.bin"))
				touch(t, filepath.Join(dir, "store", "ggml-discovered.bin"))
				return &config.Config{
					Whisper: config.WhisperConfig{Model: filepath.Join(dir, "cfg", "ggml-config.bin")},
					Models:  config.ModelsConfig{Dir: filepath.Join(dir, "store")},
				}, ""
			},
			want: "cfg/ggml-config.bin",
		},
		{
			name: "a single discovered model is used",
			setup: func(t *testing.T, dir string) (*config.Config, string) {
				touch(t, filepath.Join(dir, "store", "ggml-large-v3-turbo.bin"))
				// A VAD model in the same directory is not a candidate: it is
				// classified separately and must not count as the one hit.
				touch(t, filepath.Join(dir, "store", "ggml-silero-v5.1.2.bin"))
				return &config.Config{
					Models: config.ModelsConfig{Dir: filepath.Join(dir, "store")},
				}, ""
			},
			want: "store/ggml-large-v3-turbo.bin",
		},
		{
			name: "several discovered models are an error listing them",
			setup: func(t *testing.T, dir string) (*config.Config, string) {
				touch(t, filepath.Join(dir, "store", "ggml-base.bin"))
				touch(t, filepath.Join(dir, "store", "ggml-large-v3-turbo.bin"))
				return &config.Config{
					Models: config.ModelsConfig{Dir: filepath.Join(dir, "store")},
				}, ""
			},
			wantErr: []string{
				"several whisper models found",
				"ggml-base.bin",
				"ggml-large-v3-turbo.bin",
				"-whisper-model",
			},
		},
		{
			name: "a configured model that is not there is an error",
			setup: func(t *testing.T, dir string) (*config.Config, string) {
				return &config.Config{
					Whisper: config.WhisperConfig{Model: filepath.Join(dir, "gone", "ggml-missing.bin")},
				}, ""
			},
			wantErr: []string{"whisper model not found", "ggml-missing.bin"},
		},
		{
			name: "no model anywhere is an actionable error",
			setup: func(t *testing.T, dir string) (*config.Config, string) {
				return &config.Config{}, ""
			},
			wantErr: []string{"no whisper model found", "model pull large-v3-turbo"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			// SearchDirs ends with the relative "models" and consults $HOME for
			// the platform default and the other front-ends' stores; both are
			// redirected into the empty temp tree so discovery is hermetic.
			t.Setenv("HOME", filepath.Join(dir, "home"))
			t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "home", "share"))
			cwd := filepath.Join(dir, "cwd")
			if err := os.MkdirAll(cwd, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Chdir(cwd)

			cfg, modelFlag := tt.setup(t, dir)
			// A real file on disk, so FindBinary succeeds without depending on
			// whisper.cpp being installed on the machine running the test.
			bin := touch(t, filepath.Join(dir, "bin", "whisper-cli"))
			cfg.Whisper.Bin = bin

			opts, err := resolveWhisper(cfg, modelFlag, "", "")

			if len(tt.wantErr) > 0 {
				if err == nil {
					t.Fatalf("resolveWhisper() = %+v, want error", opts)
				}
				for _, want := range tt.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not mention %q", err, want)
					}
				}
				return
			}

			if err != nil {
				t.Fatalf("resolveWhisper() error = %v", err)
			}
			if want := filepath.Join(dir, tt.want); opts.Model != want {
				t.Errorf("Model = %q, want %q", opts.Model, want)
			}
			if opts.Bin != bin {
				t.Errorf("Bin = %q, want %q", opts.Bin, bin)
			}
		})
	}
}

// TestResolveWhisperFlagsBeatConfigForBinaryAndVAD pins the other two settings
// the flags override, which the model cases above do not touch.
func TestResolveWhisperFlagsBeatConfigForBinaryAndVAD(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "home", "share"))
	cwd := filepath.Join(dir, "cwd")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)

	binFlag := touch(t, filepath.Join(dir, "flagbin", "whisper-cli"))
	model := touch(t, filepath.Join(dir, "ggml-model.bin"))
	cfg := &config.Config{Whisper: config.WhisperConfig{
		Bin:      touch(t, filepath.Join(dir, "cfgbin", "whisper-cli")),
		Model:    model,
		VADModel: filepath.Join(dir, "cfg-vad.bin"),
		Threads:  4,
		Language: "ru",
	}}

	opts, err := resolveWhisper(cfg, "", binFlag, filepath.Join(dir, "flag-vad.bin"))
	if err != nil {
		t.Fatalf("resolveWhisper() error = %v", err)
	}
	if opts.Bin != binFlag {
		t.Errorf("Bin = %q, want the flag value %q", opts.Bin, binFlag)
	}
	if want := filepath.Join(dir, "flag-vad.bin"); opts.VADModel != want {
		t.Errorf("VADModel = %q, want %q", opts.VADModel, want)
	}
	if opts.Threads != 4 || opts.Language != "ru" {
		t.Errorf("config passthrough lost: threads=%d language=%q", opts.Threads, opts.Language)
	}
}

// The translation settings follow the same precedence as -whisper-model and
// friends: flag, then config, then default.
func TestResolveTranslateOptions(t *testing.T) {
	// A key exported in the caller's environment must not leak into the cases
	// that are about config.
	t.Setenv("OPENAI_API_KEY", "")

	empty := &config.Config{}
	configured := &config.Config{}
	for key, value := range map[string]string{
		"translate.api-key": "sk-from-config",
		"translate.model":   "gpt-6-sol",
		"translate.workers": "8",
	} {
		if err := configured.Set(key, value); err != nil {
			t.Fatalf("Set(%q): %v", key, err)
		}
	}

	tests := []struct {
		name        string
		cfg         *config.Config
		workersFlag int
		env         string
		want        translate.Options
	}{
		{
			// No key anywhere: the defaults still resolve, and Validate is what
			// refuses the run — see TestResolveTranslateOptionsWithoutAKeyIsRefused.
			name: "nothing configured", cfg: empty, workersFlag: -1,
			want: translate.Options{
				Model:   translate.DefaultModel,
				Workers: translate.DefaultWorkers,
			},
		},
		{
			name: "config supplies everything", cfg: configured, workersFlag: -1,
			want: translate.Options{
				APIKey:  "sk-from-config",
				Model:   "gpt-6-sol",
				Workers: 8,
			},
		},
		{
			name: "flags beat config", cfg: configured, workersFlag: 2,
			want: translate.Options{
				APIKey:  "sk-from-config",
				Model:   "gpt-6-sol",
				Workers: 2,
			},
		},
		{
			name: "the environment beats the config file", cfg: configured,
			workersFlag: -1, env: "sk-from-env",
			want: translate.Options{
				APIKey:  "sk-from-env",
				Model:   "gpt-6-sol",
				Workers: 8,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OPENAI_API_KEY", tt.env)
			got := resolveTranslateOptions(tt.cfg, tt.workersFlag)
			if got != tt.want {
				t.Errorf("resolveTranslateOptions() = %+v, want %+v", got, tt.want)
			}
			if tt.want.APIKey == "" {
				return
			}
			if err := got.Validate(); err != nil {
				t.Errorf("the resolved options do not validate: %v", err)
			}
		})
	}
}

// A run with no key anywhere must fail with a message that names both ways of
// supplying one, and it must fail before anything expensive happens.
func TestResolveTranslateOptionsWithoutAKeyIsRefused(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")

	err := resolveTranslateOptions(&config.Config{}, -1).Validate()
	if err == nil {
		t.Fatal("want an error when there is no API key anywhere")
	}
	for _, want := range []string{"OPENAI_API_KEY", "translate.api-key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestResolveWorkers(t *testing.T) {
	for _, tt := range []struct {
		flagValue, configValue, want int
	}{
		{-1, 0, translate.DefaultWorkers}, // nothing given
		{-1, 6, 6},                        // config
		{2, 6, 2},                         // flag wins
		{1, 6, 1},                         // including asking for the sequential run
		{0, 6, 0},                         // a typed 0 survives, for main to reject
	} {
		if got := resolveWorkers(tt.flagValue, tt.configValue); got != tt.want {
			t.Errorf("resolveWorkers(%d, %d) = %d, want %d",
				tt.flagValue, tt.configValue, got, tt.want)
		}
	}
}

// describeProvider is printed before a run spends anything, so it has to name
// the model being paid for and how many batches are in flight — including when
// the options have not been through NewTranslator yet and still hold zeroes.
func TestDescribeProviderNamesWhatIsBeingSpent(t *testing.T) {
	got := describeProvider(translate.Options{Model: "gpt-6-luna", Workers: 4})
	for _, want := range []string{"openai", "gpt-6-luna", "4"} {
		if !strings.Contains(got, want) {
			t.Errorf("describeProvider() = %q, does not mention %s", got, want)
		}
	}
	if got := describeProvider(translate.Options{}); !strings.Contains(got, translate.DefaultModel) ||
		strings.Contains(got, "0 workers") {
		t.Errorf("describeProvider() = %q, want the defaults named rather than zeroes", got)
	}
}

// buildCLI compiles the real binary, so a test can assert on the order main does
// things in. Nothing else can: the ordering lives in main(), which ends in
// os.Exit, and calling the pieces in the right order from a test only proves the
// test knows the order.
func buildCLI(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the ffprobe stub is a POSIX shell script")
	}
	bin := filepath.Join(t.TempDir(), "sub-translator")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

// A missing API key must be refused before the video is probed. With no fallback
// provider there is nothing a keyless run can produce, and under -source audio
// the alternative is transcribing a whole film and only then discovering there is
// no key — which is the waste this ordering exists to prevent.
//
// The proof has to be a real subprocess with a real ffprobe on PATH: only an
// ffprobe that was never called can show the probe did not happen.
func TestCLIRefusesAMissingAPIKeyBeforeProbing(t *testing.T) {
	bin := buildCLI(t)

	for _, tt := range []struct {
		name string
		args []string
	}{
		{"ordinary run", []string{"-to", "es"}},
		{"progressive run", []string{"-to", "es", "-source", "audio", "-fast"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			binDir := filepath.Join(dir, "bin")
			cfgDir := filepath.Join(dir, "config") // empty: no translate.api-key
			for _, d := range []string{binDir, cfgDir} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			probeLog := filepath.Join(dir, "ffprobe.log")
			writeStub(t, filepath.Join(binDir, "ffprobe"), "#!/bin/sh\necho called >> "+probeLog+"\nexit 1\n")

			input := filepath.Join(dir, "movie.mkv")
			if err := os.WriteFile(input, []byte("not really a film"), 0o644); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command(bin, append(tt.args, input)...)
			cmd.Env = append(envWithout(os.Environ(), "OPENAI_API_KEY"),
				"XDG_CONFIG_HOME="+cfgDir,
				"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			var stderr strings.Builder
			cmd.Stderr = &stderr
			var stdout strings.Builder
			cmd.Stdout = &stdout
			err := cmd.Run()

			if err == nil {
				t.Fatalf("the run succeeded with no API key:\n%s", stdout.String())
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("the binary did not run: %v", err)
			}
			if code := exit.ExitCode(); code != 1 {
				t.Errorf("exit status %d, want 1", code)
			}
			for _, want := range []string{"OPENAI_API_KEY", "translate.api-key"} {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr does not mention %s:\n%s", want, stderr.String())
				}
			}
			// The assertion that matters: ffprobe was never reached, so nothing was
			// probed, extracted or transcribed before the key was checked.
			if b, err := os.ReadFile(probeLog); err == nil && len(b) > 0 {
				t.Errorf("ffprobe ran %d time(s) before the key was checked:\n%s%s",
					len(strings.Fields(string(b))), stdout.String(), stderr.String())
			}
		})
	}
}

// envWithout is os.Environ() with one variable removed, which is how a test
// reproduces `env -u NAME`.
func envWithout(env []string, name string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// The event stream is an addition, not a replacement: a person running the tool
// in a terminal must see exactly what they saw before. Anything else makes
// -events json a breaking change for every existing user and script.
func TestEventsFlagLeavesStdoutUnchanged(t *testing.T) {
	bin := buildCLI(t)

	run := func(extra ...string) (string, string) {
		t.Helper()
		args := append([]string{"-version"}, extra...)
		cmd := exec.Command(bin, args...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		_ = cmd.Run()
		return stdout.String(), stderr.String()
	}

	plainOut, plainErr := run()
	jsonOut, jsonErr := run("-events", "json")

	if plainOut != jsonOut {
		t.Errorf("stdout differs with -events json:\nwithout: %q\nwith:    %q", plainOut, jsonOut)
	}
	if plainErr != "" {
		t.Errorf("a plain run wrote to stderr: %q", plainErr)
	}
	if !strings.Contains(jsonErr, `"type":"version"`) {
		t.Errorf("stderr carries no version event: %q", jsonErr)
	}
}

func TestEventsFlagRejectsAnythingButJSON(t *testing.T) {
	bin := buildCLI(t)
	cmd := exec.Command(bin, "-events", "yaml", "-to", "es", "movie.mkv")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("want a non-zero exit for an unsupported -events value")
	}
	if !strings.Contains(stderr.String(), "json") {
		t.Errorf("error %q does not name the supported value", stderr.String())
	}
}

// A wrapper routes on the kind — a missing key sends the user to settings, a
// missing model offers a download. An unclassified failure would leave it with
// nothing to do but show red text.
func TestClassifiedErrorsCarryTheirKind(t *testing.T) {
	bin := buildCLI(t)
	cfg := t.TempDir()
	input := touch(t, filepath.Join(t.TempDir(), "movie.mkv"))

	tests := []struct {
		name string
		args []string
		env  []string
		kind string
	}{
		{
			name: "missing key",
			args: []string{"-events", "json", "-to", "es", input},
			kind: "missing_key",
		},
		{
			name: "input file not found",
			args: []string{"-events", "json", "-to", "es", "/nonexistent/movie.mkv"},
			env:  []string{"OPENAI_API_KEY=sk-test"},
			kind: "probe_failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(bin, tt.args...)
			cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+cfg)
			cmd.Env = append(cmd.Env, tt.env...)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err == nil {
				t.Fatalf("want a non-zero exit\nstderr: %s", stderr.String())
			}

			var found string
			for _, line := range strings.Split(stderr.String(), "\n") {
				var m map[string]any
				if json.Unmarshal([]byte(line), &m) != nil {
					continue
				}
				if m["type"] == "error" {
					found, _ = m["kind"].(string)
				}
			}
			if found != tt.kind {
				t.Errorf("error kind = %q, want %q\nstderr: %s", found, tt.kind, stderr.String())
			}
		})
	}
}
