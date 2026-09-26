package main

import (
	"os"
	"path/filepath"
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
// friends: flag, then config, then default. The default provider in particular
// must stay google — nobody should start spending money because they upgraded.
func TestResolveTranslateOptions(t *testing.T) {
	// A key exported in the caller's environment must not leak into the cases
	// that are about config.
	t.Setenv("OPENAI_API_KEY", "")

	empty := &config.Config{}
	configured := &config.Config{}
	for key, value := range map[string]string{
		"translate.provider": "openai",
		"translate.api-key":  "sk-from-config",
		"translate.model":    "gpt-6-sol",
		"translate.workers":  "8",
	} {
		if err := configured.Set(key, value); err != nil {
			t.Fatalf("Set(%q): %v", key, err)
		}
	}

	tests := []struct {
		name         string
		cfg          *config.Config
		providerFlag string
		workersFlag  int
		env          string
		want         translate.Options
	}{
		{
			name: "nothing configured", cfg: empty, workersFlag: -1,
			want: translate.Options{
				Provider: translate.ProviderGoogle,
				Model:    translate.DefaultModel,
				Workers:  translate.DefaultWorkersFor(translate.ProviderGoogle),
			},
		},
		{
			name: "config supplies everything", cfg: configured, workersFlag: -1,
			want: translate.Options{
				Provider: translate.ProviderOpenAI,
				APIKey:   "sk-from-config",
				Model:    "gpt-6-sol",
				Workers:  8,
			},
		},
		{
			name: "flags beat config", cfg: configured,
			providerFlag: "google", workersFlag: 2,
			want: translate.Options{
				Provider: translate.ProviderGoogle,
				APIKey:   "sk-from-config",
				Model:    "gpt-6-sol",
				Workers:  2,
			},
		},
		{
			name: "the environment beats the config file", cfg: configured,
			workersFlag: -1, env: "sk-from-env",
			want: translate.Options{
				Provider: translate.ProviderOpenAI,
				APIKey:   "sk-from-env",
				Model:    "gpt-6-sol",
				Workers:  8,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OPENAI_API_KEY", tt.env)
			got := resolveTranslateOptions(tt.cfg, tt.providerFlag, tt.workersFlag)
			if got != tt.want {
				t.Errorf("resolveTranslateOptions() = %+v, want %+v", got, tt.want)
			}
			if err := got.Validate(); err != nil {
				t.Errorf("the resolved options do not validate: %v", err)
			}
		})
	}
}

// An openai run with no key anywhere must fail with a message that names both
// ways of supplying one, and it must fail before anything expensive happens.
func TestResolveTranslateOptionsWithoutAKeyIsRefused(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	c := &config.Config{}
	if err := c.Set("translate.provider", "openai"); err != nil {
		t.Fatal(err)
	}

	err := resolveTranslateOptions(c, "", -1).Validate()
	if err == nil {
		t.Fatal("want an error for the openai provider with no key")
	}
	for _, want := range []string{"OPENAI_API_KEY", "translate.api-key"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestResolveWorkers(t *testing.T) {
	for _, tt := range []struct {
		flagValue, configValue int
		provider               string
		want                   int
	}{
		// Nothing given: the default follows the provider, because one batch at a
		// time is what a free public endpoint tolerates.
		{-1, 0, translate.ProviderGoogle, 1},
		{-1, 0, translate.ProviderOpenAI, 4},
		{-1, 6, translate.ProviderGoogle, 6}, // config
		{2, 6, translate.ProviderGoogle, 2},  // flag wins
		{4, 0, translate.ProviderGoogle, 4},  // including asking Google for four
		{1, 6, translate.ProviderOpenAI, 1},  // and asking openai for the sequential run
		{0, 6, translate.ProviderGoogle, 0},  // a typed 0 survives, for main to reject
	} {
		if got := resolveWorkers(tt.flagValue, tt.configValue, tt.provider); got != tt.want {
			t.Errorf("resolveWorkers(%d, %d, %q) = %d, want %d",
				tt.flagValue, tt.configValue, tt.provider, got, tt.want)
		}
	}
}

// describeProvider is printed before a run spends anything, so it has to name
// the model when there is one to pay for.
func TestDescribeProviderNamesWhatIsBeingSpent(t *testing.T) {
	got := describeProvider(translate.Options{
		Provider: translate.ProviderOpenAI, Model: "gpt-6-luna", Workers: 4,
	})
	for _, want := range []string{"openai", "gpt-6-luna", "4"} {
		if !strings.Contains(got, want) {
			t.Errorf("describeProvider() = %q, does not mention %s", got, want)
		}
	}
	if got := describeProvider(translate.Options{
		Provider: translate.ProviderGoogle, Model: translate.DefaultModel, Workers: 1,
	}); strings.Contains(got, translate.DefaultModel) {
		t.Errorf("describeProvider() = %q: the Google path has no model to name", got)
	}
}
