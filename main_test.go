package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/artschekoff/sub-translator/internal/config"
	"github.com/artschekoff/sub-translator/internal/events"
	"github.com/artschekoff/sub-translator/internal/media"
	"github.com/artschekoff/sub-translator/internal/translate"
	"github.com/artschekoff/sub-translator/internal/whisper"
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

// pctOf must never disagree with the percentage the progress line prints
// beside it. Fix round 2 used math.Round, which rounds halves away from
// zero, while the printed line's %.0f rounds halves to even — 12.5% (1/8)
// printed as 12 while pctOf emitted 13, and the review found 126 such pairs
// for totals up to 200. This asserts the actual invariant — pctOf's result
// equals int(%.0f of the same division) — rather than pinning today's
// numbers by hand, so any future rounding-rule mismatch fails here again
// regardless of which values happen to expose it.
func TestPctOfAgreesWithThePrintedPercentage(t *testing.T) {
	for _, tt := range []struct{ done, total int }{
		{1, 8},  // 12.5% — exact half: even (12) vs away-from-zero (13)
		{3, 8},  // 37.5% — exact half
		{5, 8},  // 62.5% — exact half
		{1, 40}, // 2.5%  — exact half
		{3, 40}, // 7.5%  — exact half
		{0, 1},
		{1, 1},
		{1, 3},
		{2, 3},
		{7, 8},
		{99, 200},
		{100, 200},
		{101, 200},
	} {
		want := fmt.Sprintf("%.0f", float64(tt.done)/float64(tt.total)*100)
		wantInt, err := strconv.Atoi(want)
		if err != nil {
			t.Fatalf("test setup: %q (from done=%d total=%d) is not an integer string", want, tt.done, tt.total)
		}
		if got := pctOf(tt.done, tt.total); got != wantInt {
			t.Errorf("pctOf(%d, %d) = %d, want %d (the printed line's %%.0f gives %q)",
				tt.done, tt.total, got, wantInt, want)
		}
	}
}

// pctOfDuration is pctOf's sibling for the extraction and mux progress
// callbacks, which track a time.Duration pair rather than a block count. This
// is the same invariant TestPctOfAgreesWithThePrintedPercentage pins for
// pctOf, asserted independently of pctOfDuration's own implementation (which
// goes through pctOf via Milliseconds()) — this test computes its "want"
// straight from Seconds(), so a change that quietly loses sub-second
// precision (say, truncating to whole seconds before dividing) fails here
// even though it might still satisfy pctOf's own test.
func TestPctOfDurationAgreesWithThePrintedPercentage(t *testing.T) {
	for _, tt := range []struct{ done, total time.Duration }{
		{1 * time.Second, 8 * time.Second},        // 12.5% — exact half
		{830 * time.Millisecond, 1 * time.Second}, // sub-second precision
		{83*time.Second + 450*time.Millisecond, 200 * time.Second},
		{0, time.Minute},
		{time.Minute, time.Minute},
		{90 * time.Second, 4 * time.Minute},
	} {
		want := fmt.Sprintf("%.0f", tt.done.Seconds()/tt.total.Seconds()*100)
		wantInt, err := strconv.Atoi(want)
		if err != nil {
			t.Fatalf("test setup: %q (from done=%v total=%v) is not an integer string", want, tt.done, tt.total)
		}
		if got := pctOfDuration(tt.done, tt.total); got != wantInt {
			t.Errorf("pctOfDuration(%v, %v) = %d, want %d (the printed line's %%.0f gives %q)",
				tt.done, tt.total, got, wantInt, want)
		}
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

// subtitleTrackSRT is the body subtitleSourceStubs' ffmpeg stub writes out for
// -source sub to extract and parse: two ordinary blocks, enough to prove a real
// translation request went out, with nothing about their content that matters.
const subtitleTrackSRT = "1\n00:00:01,000 --> 00:00:03,000\nHello there\n\n" +
	"2\n00:00:05,000 --> 00:00:07,000\nGeneral Kenobi\n\n"

// TestTheCompiledBinaryHonoursOPENAI_BASE_URL is the test that the ordinary
// run's whole event stream depends on: without it no test can drive a real
// process through a successful translation, because the only other way to
// reach a stub endpoint is a package variable (newTranslateClient, swapped by
// stubTranslator) a subprocess cannot see. It drives the compiled binary
// through a real -source sub run and asserts the translation request reached
// the env var's endpoint rather than the real one.
func TestTheCompiledBinaryHonoursOPENAI_BASE_URL(t *testing.T) {
	srv := stubTranslateEndpoint(t)

	bin := buildCLI(t)
	dir := t.TempDir()
	binDir := subtitleSourceStubs(t, dir, subtitleTrackSRT)

	film := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(film, []byte("not really a film"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "-to", "es", "-source", "sub", "-from", "en", "-track", "0", film)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"OPENAI_API_KEY=sk-test",
		"OPENAI_BASE_URL="+srv.URL,
		"XDG_CONFIG_HOME="+filepath.Join(dir, "cfg"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run failed: %v\n%s", err, out)
	}

	// The stub echoes every line back prefixed "ES:". The real endpoint would
	// have rejected "sk-test" or been unreachable from this sandbox, either of
	// which fails the run above — so finding the prefix in the file the run
	// wrote is direct proof OPENAI_BASE_URL routed the request here, not there.
	got, err := os.ReadFile(media.DefaultSRTPath(film, "es"))
	if err != nil {
		t.Fatalf("reading the translated SRT: %v\n%s", err, out)
	}
	if !strings.Contains(string(got), "ES:") {
		t.Fatalf("translated SRT carries no ES: prefix, so the stub endpoint was never reached:\n%s\n%s", got, out)
	}
}

// stubTranslateEndpoint answers every batch with the source text it was sent,
// each prefixed "ES:" and keyed exactly as asked. A fixed reply cannot be used
// here: batch()'s keys are 0-indexed per batch, and mapReply requires the
// reply's key set to match the request's exactly, so a canned
// {"1":"hola"}-style reply fails every block and exits the run non-zero before
// any assertion about the event stream gets to run.
func stubTranslateEndpoint(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var user string
		for _, m := range req.Messages {
			if m.Role == "user" {
				user = m.Content
			}
		}
		var payload struct {
			Translate map[string]string `json:"translate"`
		}
		json.Unmarshal([]byte(user), &payload)
		out := make(map[string]string, len(payload.Translate))
		for k, v := range payload.Translate {
			out[k] = "ES:" + v
		}
		content, _ := json.Marshal(out)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, content)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// untranslatableBlockSRT is a two-block subtitle body where the second block's
// text carries refusedBlockMarker, the substring
// stubTranslateEndpointRefusingSomeBlocks refuses to translate.
const untranslatableBlockSRT = "1\n00:00:01,000 --> 00:00:03,000\nHello there\n\n" +
	"2\n00:00:05,000 --> 00:00:07,000\nUNTRANSLATABLE block\n\n"

const refusedBlockMarker = "UNTRANSLATABLE"

// stubTranslateEndpointRefusingSomeBlocks behaves like stubTranslateEndpoint,
// except any block whose source text contains marker is answered with a
// mismatched key instead of a translation. mapReply requires the reply's key
// set to equal the request's exactly, so that block's batch attempt fails,
// which is what drives openai.go's per-block fallback (one()) — where the same
// content-based check refuses it again, permanently. The refusal is decided
// from each request's own content, never from a shared call counter, so it
// stays deterministic across the batch's own retry and across concurrent
// workers, neither of which promises a stable call order.
func stubTranslateEndpointRefusingSomeBlocks(t *testing.T, marker string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var user string
		for _, m := range req.Messages {
			if m.Role == "user" {
				user = m.Content
			}
		}
		var payload struct {
			Translate map[string]string `json:"translate"`
		}
		json.Unmarshal([]byte(user), &payload)

		out := make(map[string]string, len(payload.Translate))
		for k, v := range payload.Translate {
			if strings.Contains(v, marker) {
				// A key the request never asked for, so mapReply's key-set check
				// fails this key specifically — unique per source key, so several
				// refused blocks in the same batch don't collide into one entry
				// and understate how many keys are actually missing.
				out["__refused_"+k+"__"] = "x"
				continue
			}
			out[k] = "ES:" + v
		}
		content, _ := json.Marshal(out)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, content)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// parseEventStream returns one map per JSON event line on stderr, skipping the
// prose lines the protocol's own skip rule says a consumer may ignore (any line
// that does not start with '{'). Unlike eventsOf/eventsOfKind above, it fails
// the test outright on a line that starts with '{' and does not parse as JSON,
// rather than silently skipping it: that shape — a line beginning like an event
// but broken — is exactly the stream corruption (an event split across two
// writes) the previous branch spent a Critical and a Major fixing, and a helper
// that skips it could never catch a regression of it.
func parseEventStream(t *testing.T, stderr []byte) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(string(stderr), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "{") {
			continue // a prose line, which the protocol says to skip
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("stderr line starts with '{' but does not parse as JSON: %v\nline: %q", err, line)
		}
		out = append(out, m)
	}
	return out
}

// kindsOf returns the "type" field of every event, in order.
func kindsOf(events []map[string]any) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		k, _ := e["type"].(string)
		out = append(out, k)
	}
	return out
}

// countOf returns how many events carry the given "type".
func countOf(events []map[string]any, kind string) int {
	n := 0
	for _, e := range events {
		if e["type"] == kind {
			n++
		}
	}
	return n
}

// containsSubsequence reports whether want appears in got in order, though not
// necessarily contiguously — want is a subsequence of got.
func containsSubsequence(got, want []string) bool {
	i := 0
	for _, g := range got {
		if i == len(want) {
			break
		}
		if g == want[i] {
			i++
		}
	}
	return i == len(want)
}

// TestTheOrdinaryRunEmitsItsWholeStream pins the default (non -fast) path's
// events end to end. It is a real, compiled process, so it also pins that exit
// 0 and exactly one done event agree — the pair no in-process test can assert,
// because only a real process both carries its own exit code and cannot be
// handed a fake events.Emitter to inspect afterwards.
func TestTheOrdinaryRunEmitsItsWholeStream(t *testing.T) {
	srv := stubTranslateEndpoint(t)
	bin := buildCLI(t)
	dir := t.TempDir()
	binDir := subtitleSourceStubs(t, dir, subtitleTrackSRT)

	film := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(film, []byte("not really a film"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	cmd := exec.Command(bin, "-events", "json", "-to", "es", "-source", "sub", "-from", "en", "-track", "0", film)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"OPENAI_API_KEY=sk-test",
		"OPENAI_BASE_URL="+srv.URL,
		"XDG_CONFIG_HOME="+filepath.Join(dir, "cfg"),
	)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("want exit 0, got %v\nstderr:\n%s", err, stderr.String())
	}

	events := parseEventStream(t, stderr.Bytes())
	wantInOrder := []string{"version", "probe", "stage", "stage", "progress", "done"}
	if got := kindsOf(events); !containsSubsequence(got, wantInOrder) {
		t.Errorf("event kinds = %v, want %v as a subsequence", got, wantInOrder)
	}
	if n := countOf(events, "done"); n != 1 {
		t.Errorf("%d done events, want exactly one", n)
	}
	if n := countOf(events, "error"); n != 0 {
		t.Errorf("%d error events on a successful run, want none", n)
	}
	for _, e := range events {
		if e["type"] == "done" && e["path"] == "" {
			t.Error("the done event carries no path, so a consumer cannot open the result")
		}
	}
}

// TestTheOrdinaryRunReportsUntranslatedBlocks pins the warning event on the
// default path: a run where a block could not be translated must say so
// through the event stream — kind, count and total — and must not look
// identical to a run where nothing failed.
func TestTheOrdinaryRunReportsUntranslatedBlocks(t *testing.T) {
	srv := stubTranslateEndpointRefusingSomeBlocks(t, refusedBlockMarker)
	bin := buildCLI(t)
	dir := t.TempDir()
	binDir := subtitleSourceStubs(t, dir, untranslatableBlockSRT)

	film := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(film, []byte("not really a film"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	cmd := exec.Command(bin, "-events", "json", "-to", "es", "-source", "sub", "-from", "en", "-track", "0", film)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"OPENAI_API_KEY=sk-test",
		"OPENAI_BASE_URL="+srv.URL,
		"XDG_CONFIG_HOME="+filepath.Join(dir, "cfg"),
	)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("want exit 0 — a partial translation failure is a warning, not a fatal error — got %v\nstderr:\n%s", err, stderr.String())
	}

	events := parseEventStream(t, stderr.Bytes())
	if got := kindsOf(events); !containsSubsequence(got, []string{"warning", "done"}) {
		t.Errorf("event kinds = %v, want a warning before the done event", got)
	}
	if n := countOf(events, "warning"); n != 1 {
		t.Fatalf("%d warning events, want exactly one:\n%s", n, stderr.String())
	}
	if n := countOf(events, "done"); n != 1 {
		t.Errorf("%d done events, want exactly one", n)
	}

	var w map[string]any
	for _, e := range events {
		if e["type"] == "warning" {
			w = e
		}
	}
	if kind, _ := w["kind"].(string); kind != "blocks_untranslated" {
		t.Errorf("warning kind = %q, want blocks_untranslated", kind)
	}
	if count, _ := w["count"].(float64); count <= 0 {
		t.Errorf("warning count = %v, want > 0", w["count"])
	}
	// untranslatableBlockSRT parses to exactly two blocks.
	if total, _ := w["total"].(float64); total != 2 {
		t.Errorf("warning total = %v, want 2 (the number of blocks in the source)", w["total"])
	}
}

// TestTheOrdinaryRunEmitsTheContainerDoneEvent pins the other done event on
// the default path. The sidecar test above only ever exercises the mode-srt
// branch (the tool's own default), which never reaches the mux branch's
// separate, later events.Done call — so -mode mux is what drives that one.
func TestTheOrdinaryRunEmitsTheContainerDoneEvent(t *testing.T) {
	srv := stubTranslateEndpoint(t)
	bin := buildCLI(t)
	dir := t.TempDir()
	binDir := subtitleSourceStubs(t, dir, subtitleTrackSRT)

	film := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(film, []byte("not really a film"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	cmd := exec.Command(bin, "-events", "json", "-to", "es", "-source", "sub",
		"-from", "en", "-track", "0", "-mode", "mux", film)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"OPENAI_API_KEY=sk-test",
		"OPENAI_BASE_URL="+srv.URL,
		"XDG_CONFIG_HOME="+filepath.Join(dir, "cfg"),
	)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("want exit 0, got %v\nstderr:\n%s", err, stderr.String())
	}

	events := parseEventStream(t, stderr.Bytes())

	// A loose containsSubsequence(kindsOf(events), []string{"stage", "done"})
	// check is satisfied by ANY earlier stage (extract-subs or translate both
	// fire first on this path), so it never actually requires the mux stage —
	// and pinning only mux's *presence* before done is not much stronger: a
	// reviewer once moved the mux emit to the very start of the run and this
	// test still passed. Pin the whole ordered stage sequence instead — it is
	// both stronger and simpler, and stage order is what the GUI displays.
	var stageNames []string
	for _, e := range events {
		if e["type"] != "stage" {
			continue
		}
		name, _ := e["name"].(string)
		stageNames = append(stageNames, name)
	}
	wantStages := []string{"extract-subs", "translate", "mux"}
	if !slices.Equal(stageNames, wantStages) {
		t.Errorf("stage names = %v, want %v in that order", stageNames, wantStages)
	}

	if n := countOf(events, "done"); n != 1 {
		t.Fatalf("%d done events, want exactly one:\n%s", n, stderr.String())
	}
	var done map[string]any
	doneIdx := -1
	muxIdx := -1
	for i, e := range events {
		switch e["type"] {
		case "done":
			done = e
			doneIdx = i
		case "stage":
			if name, _ := e["name"].(string); name == "mux" {
				muxIdx = i
			}
		}
	}
	if muxIdx != -1 && muxIdx > doneIdx {
		t.Errorf("mux stage at event index %d came after the done event at index %d, want the mux stage before done", muxIdx, doneIdx)
	}
	wantPath := media.DefaultOutputPath(film, "es")
	if got, _ := done["path"].(string); got != wantPath {
		t.Errorf("done path = %q, want %q (the muxed container, not a sidecar)", got, wantPath)
	}
}

// TestTheMuxWriteIsClassified drives a real mux failure through the compiled
// binary and asserts the error event's kind is output_unwritable. It is the
// sibling of TestTheOrdinaryRunEmitsTheContainerDoneEvent above, which already
// proves -mode mux is reachable through the compiled binary with nothing more
// exotic than subtitleSourceStubs and stubTranslateEndpoint — a comment near
// errkind_test.go once claimed a real mux failure could not be driven to
// completion in this suite; this test is the proof that it can, by failing
// only the ffmpeg invocation that performs the mux itself while leaving the
// (also ffmpeg-driven) subtitle extraction that has to succeed first alone.
//
// The two calls are told apart by how many -i flags they carry:
// ExtractSubtitle passes exactly one (the input file), while MuxSubtitle
// passes two (the input file and the translated SRT) — internal/media/media.go
// builds both argument lists, and that shape is the only thing distinguishing
// them from the stub's side.
func TestTheMuxWriteIsClassified(t *testing.T) {
	srv := stubTranslateEndpoint(t)
	bin := buildCLI(t)
	dir := t.TempDir()
	binDir := subtitleSourceStubs(t, dir, subtitleTrackSRT)

	// Overwrite subtitleSourceStubs' ffmpeg stub, which always succeeds: this
	// one still extracts a real subtitle track (one -i) but refuses the mux
	// invocation (two -i's, input and the translated SRT) exactly the way a
	// read-only output directory or a full disk would.
	writeStub(t, filepath.Join(binDir, "ffmpeg"), `#!/bin/sh
i_count=0
prev=""
out=""
for a in "$@"; do
  if [ "$prev" = "-i" ]; then i_count=$((i_count+1)); fi
  prev="$a"
  out="$a"
done
if [ "$i_count" -ge 2 ]; then
  echo "ffmpeg: mux stub refuses to write the container" >&2
  exit 1
fi
cat > "$out" <<'SRTBODY'
`+subtitleTrackSRT+`
SRTBODY
exit 0
`)

	film := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(film, []byte("not really a film"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	cmd := exec.Command(bin, "-events", "json", "-to", "es", "-source", "sub",
		"-from", "en", "-track", "0", "-mode", "mux", film)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"OPENAI_API_KEY=sk-test",
		"OPENAI_BASE_URL="+srv.URL,
		"XDG_CONFIG_HOME="+filepath.Join(dir, "cfg"),
	)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(runErr, &exit) {
		t.Fatalf("the binary did not run: %v\nstdout:\n%s\nstderr:\n%s", runErr, stdout.String(), stderr.String())
	}
	if code := exit.ExitCode(); code != 1 {
		t.Errorf("exit status %d, want 1", code)
	}

	events := parseEventStream(t, stderr.Bytes())
	var errs []map[string]any
	for _, e := range events {
		if e["type"] == "error" {
			errs = append(errs, e)
		}
	}
	if len(errs) != 1 {
		t.Fatalf("%d error events, want exactly 1:\n%s", len(errs), stderr.String())
	}
	if kind, _ := errs[0]["kind"].(string); kind != string(errOutputUnwritable) {
		t.Errorf("error event kind = %q, want %q\nstderr:\n%s", kind, errOutputUnwritable, stderr.String())
	}
	if msg, _ := errs[0]["message"].(string); !strings.HasPrefix(msg, "mux: ") {
		t.Errorf("error event message = %q, want it to start with %q", msg, "mux: ")
	}
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
			var errorEventCount int
			for _, line := range strings.Split(stderr.String(), "\n") {
				var m map[string]any
				if json.Unmarshal([]byte(line), &m) != nil {
					continue
				}
				if m["type"] == "error" {
					errorEventCount++
					found, _ = m["kind"].(string)
				}
			}
			if found != tt.kind {
				t.Errorf("error kind = %q, want %q\nstderr: %s", found, tt.kind, stderr.String())
			}
			// fatalKindf must emit its own kind exactly once, never falling
			// through to fatalf's "other" as well — that would double-emit
			// (one classified, one unclassified) for every classified fatal.
			if errorEventCount != 1 {
				t.Errorf("%d error events, want exactly 1\nstderr: %s", errorEventCount, stderr.String())
			}
		})
	}
}

// Only fatalKindf used to emit an error event; plain fatalf emitted nothing,
// so an unclassified fatal left a -events json consumer with a non-zero exit
// and total silence on the wire. fatalf now emits kind "other" itself. This
// drives a fatalf call site that carries no kind of its own — an empty
// subtitle track, reached the same way TestOrdinaryRunProbeEventCarriesDuration
// reaches the probe event, without touching whisper or the network.
func TestFatalfEmitsAnUnclassifiedErrorEvent(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, filepath.Join(binDir, "ffprobe"), "#!/bin/sh\n"+`cat <<JSON
{"streams":[
 {"index":0,"codec_type":"subtitle","codec_name":"srt","tags":{"language":"eng"}}
],"format":{"duration":"120.5"}}
JSON`)
	writeStub(t, filepath.Join(binDir, "ffmpeg"), "#!/bin/sh\nout=\"\"\nfor a in \"$@\"; do out=\"$a\"; done\n: > \"$out\"\nexit 0\n")

	film := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(film, []byte("not a film"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "-events", "json", "-to", "es", "-from", "en", "-track", "0", film)
	cmd.Env = append(os.Environ(),
		"OPENAI_API_KEY=sk-test",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "cfg"),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	var exit *exec.ExitError
	if !errors.As(runErr, &exit) {
		t.Fatalf("the binary did not run: %v", runErr)
	}
	if code := exit.ExitCode(); code != 1 {
		t.Errorf("exit status %d, want 1", code)
	}

	var errorEvents []map[string]any
	for _, line := range strings.Split(stderr.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["type"] == "error" {
			errorEvents = append(errorEvents, m)
		}
	}
	if len(errorEvents) != 1 {
		t.Fatalf("%d error events, want exactly 1: %v\nstdout: %s\nstderr: %s",
			len(errorEvents), errorEvents, stdout.String(), stderr.String())
	}
	if kind, _ := errorEvents[0]["kind"].(string); kind != "other" {
		t.Errorf("error event kind = %q, want %q", kind, "other")
	}
	if !strings.Contains(stderr.String(), "error: subtitle track #0 is empty") {
		t.Errorf("stderr's human-readable \"error: ...\" line is missing or changed:\n%s", stderr.String())
	}
}

// TestTheProbeEventStatesTheFastModeRule is what lets the GUI disable its
// checkbox without holding a copy of the rule.
func TestTheProbeEventStatesTheFastModeRule(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := "#!/bin/sh\n" + `cat <<'JSON'
{"streams":[
 {"index":0,"codec_type":"video","codec_name":"h264","tags":{"language":"rus"}},
 {"index":1,"codec_type":"audio","codec_name":"ac3","tags":{"language":"eng","title":"Original"}}
],"format":{"duration":"120.5"}}
JSON`
	writeStub(t, filepath.Join(binDir, "ffprobe"), stub)

	film := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(film, []byte("not a film"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "-events", "json", "-probe", film)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "cfg"),
	)
	cmd.Env = append(cmd.Env, "OPENAI_API_KEY=")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("probe failed: %v\nstderr: %s", err, stderr.String())
	}

	var probe map[string]any
	for _, line := range strings.Split(stderr.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["type"] == "probe" {
			probe = m
		}
	}
	if probe == nil {
		t.Fatalf("no probe event:\n%s", stderr.String())
	}

	fast, ok := probe["fast"].(map[string]any)
	if !ok {
		t.Fatalf("probe.fast = %#v, want an object", probe["fast"])
	}
	sources, _ := fast["sources"].([]any)
	if len(sources) != 1 || sources[0] != "audio" {
		t.Errorf("fast.sources = %v, want [\"audio\"]", fast["sources"])
	}
	modes, _ := fast["modes"].([]any)
	if len(modes) != 1 || modes[0] != "srt" {
		t.Errorf("fast.modes = %v, want [\"srt\"]", fast["modes"])
	}
	if fast["min_chunk_minutes"] != float64(1) {
		t.Errorf("fast.min_chunk_minutes = %v, want 1", fast["min_chunk_minutes"])
	}
}

// -probe is what a wrapper calls to build a track list before the user has
// chosen anything, so it must need nothing: no API key, no target language, and
// it must not write a file or start any work.
func TestProbeNeedsNoKeyAndDoesNoWork(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()

	// A stub ffprobe: -probe must go no further than this.
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "calls.log")
	stub := "#!/bin/sh\necho \"$@\" >> " + log + "\n" + `cat <<'JSON'
{"streams":[
 {"index":0,"codec_type":"video","codec_name":"h264","tags":{"language":"rus"}},
 {"index":1,"codec_type":"audio","codec_name":"ac3","tags":{"language":"eng","title":"Original"}}
],"format":{"duration":"120.5"}}
JSON`
	if err := os.WriteFile(filepath.Join(binDir, "ffprobe"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	film := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(film, []byte("not a film"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "-events", "json", "-probe", film)
	cmd.Env = append(os.Environ(),
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "cfg"),
	)
	cmd.Env = append(cmd.Env, "OPENAI_API_KEY=")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("probe failed: %v\nstderr: %s", err, stderr.String())
	}

	var probe map[string]any
	for _, line := range strings.Split(stderr.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["type"] == "probe" {
			probe = m
		}
	}
	if probe == nil {
		t.Fatalf("no probe event:\n%s", stderr.String())
	}
	audio, _ := probe["audio"].([]any)
	if len(audio) != 1 {
		t.Errorf("audio = %#v, want one track", probe["audio"])
	}
	if probe["duration"] != 120.5 {
		t.Errorf("duration = %v, want 120.5", probe["duration"])
	}

	// Nothing beyond the probe may have run.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".srt") {
			t.Errorf("-probe wrote %s", e.Name())
		}
	}
}

// An off feature must cost nothing. probeEvent's own work — a second ffprobe
// call for the duration — was, for one commit, unconditional: Go evaluates
// Emit's argument before Emit's own nil check runs, so every ordinary run paid
// for that subprocess whether or not -events json was passed. This counts
// actual ffprobe invocations through a logging stub, once per flag
// combination, using the same empty-subtitle-track failure
// TestOrdinaryRunProbeEventCarriesDuration uses to reach the probe event
// without whisper or the network.
func TestOrdinaryRunSpawnsFfprobeOnlyWhenEventsAreOn(t *testing.T) {
	bin := buildCLI(t)

	countFfprobeCalls := func(t *testing.T, extraArgs ...string) int {
		t.Helper()
		dir := t.TempDir()
		binDir := filepath.Join(dir, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatal(err)
		}
		log := filepath.Join(dir, "ffprobe.log")
		writeStub(t, filepath.Join(binDir, "ffprobe"), "#!/bin/sh\necho called >> \""+log+"\"\n"+`cat <<JSON
{"streams":[
 {"index":0,"codec_type":"subtitle","codec_name":"srt","tags":{"language":"eng"}}
],"format":{"duration":"120.5"}}
JSON`)
		writeStub(t, filepath.Join(binDir, "ffmpeg"), "#!/bin/sh\nout=\"\"\nfor a in \"$@\"; do out=\"$a\"; done\n: > \"$out\"\nexit 0\n")

		film := filepath.Join(dir, "movie.mkv")
		if err := os.WriteFile(film, []byte("not a film"), 0o600); err != nil {
			t.Fatal(err)
		}

		args := append([]string{"-to", "es", "-from", "en", "-track", "0"}, extraArgs...)
		args = append(args, film)
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(),
			"OPENAI_API_KEY=sk-test",
			"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			"XDG_CONFIG_HOME="+filepath.Join(dir, "cfg"),
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err == nil {
			t.Fatalf("run succeeded, want the empty-subtitle-track failure:\n%s", stdout.String())
		}

		b, err := os.ReadFile(log)
		if err != nil {
			if os.IsNotExist(err) {
				return 0
			}
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(lines) == 1 && lines[0] == "" {
			return 0
		}
		return len(lines)
	}

	t.Run("without -events json: one ffprobe call", func(t *testing.T) {
		if got := countFfprobeCalls(t); got != 1 {
			t.Errorf("ffprobe invoked %d time(s), want 1 (just media.Probe)", got)
		}
	})

	t.Run("with -events json: two ffprobe calls", func(t *testing.T) {
		if got := countFfprobeCalls(t, "-events", "json"); got != 2 {
			t.Errorf("ffprobe invoked %d time(s), want 2 (media.Probe, plus probeEvent's duration)", got)
		}
	})
}

// The spec's sample stream is a full run, not -probe, and its "probe" line
// carries duration — so the ordinary run's probe event must too, via the same
// media.ContainerDuration call -probe's emitProbe already made. The run is
// steered into failing fast right after probing (an explicit -track pointing
// at a subtitle stream that ffmpeg — stubbed to write an empty file — turns
// into zero blocks), so this never has to reach whisper or the network: only
// the probe event, already on stderr by then, is asserted on.
func TestOrdinaryRunProbeEventCarriesDuration(t *testing.T) {
	bin := buildCLI(t)

	probeEventFor := func(t *testing.T, formatDuration string) map[string]any {
		t.Helper()
		dir := t.TempDir()
		binDir := filepath.Join(dir, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Same JSON body answers both the -show_streams and -show_format calls
		// Probe and ContainerDuration each make; neither cares about the other's
		// field.
		writeStub(t, filepath.Join(binDir, "ffprobe"), "#!/bin/sh\n"+`cat <<JSON
{"streams":[
 {"index":0,"codec_type":"subtitle","codec_name":"srt","tags":{"language":"eng"}}
],"format":{"duration":"`+formatDuration+`"}}
JSON`)
		// ExtractSubtitle's only job here is to produce an empty transcript, so
		// the run fails on "nothing to translate" without ever reaching whisper
		// or the translation endpoint.
		writeStub(t, filepath.Join(binDir, "ffmpeg"), "#!/bin/sh\nout=\"\"\nfor a in \"$@\"; do out=\"$a\"; done\n: > \"$out\"\nexit 0\n")

		film := filepath.Join(dir, "movie.mkv")
		if err := os.WriteFile(film, []byte("not a film"), 0o600); err != nil {
			t.Fatal(err)
		}

		cmd := exec.Command(bin, "-events", "json", "-to", "es", "-from", "en", "-track", "0", film)
		cmd.Env = append(os.Environ(),
			"OPENAI_API_KEY=sk-test",
			"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			"XDG_CONFIG_HOME="+filepath.Join(dir, "cfg"),
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err == nil {
			t.Fatalf("run succeeded, want the empty-subtitle-track failure:\n%s", stdout.String())
		}

		var probe map[string]any
		for _, line := range strings.Split(stderr.String(), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) == nil && m["type"] == "probe" {
				probe = m
			}
		}
		if probe == nil {
			t.Fatalf("no probe event:\n%s%s", stdout.String(), stderr.String())
		}
		return probe
	}

	t.Run("a readable duration is carried", func(t *testing.T) {
		probe := probeEventFor(t, "120.5")
		if probe["duration"] != 120.5 {
			t.Errorf("duration = %v, want 120.5", probe["duration"])
		}
	})

	t.Run("an unreadable duration is omitted, never zero", func(t *testing.T) {
		probe := probeEventFor(t, "N/A")
		if d, ok := probe["duration"]; ok {
			t.Errorf("duration = %v, want the field omitted entirely (omitempty), not sent as 0", d)
		}
	})
}

// The event stream is what a wrapper renders its progress from. This pins the
// order and the presence of the events a run produces, so a refactor cannot
// quietly stop emitting one.
//
// Deviation from the brief's illustrative code: that example drove a real
// compiled binary with exec.Command and expected -events json to reach a
// working translation endpoint over the network. translate.Options.BaseURL is
// documented in internal/translate/translator.go as a test seam for exactly
// this reason ("the end-to-end tests fill in BaseURL to keep a whole run off
// the network"), but it is a package-level field filled in-process — a
// separate compiled subprocess cannot be redirected to httptest this way, and
// there is no flag, config key or environment variable that lets a real
// sub-translator binary point its OpenAI client anywhere but the real
// endpoint (by design: Task 6 "Produces: no new API"). So this test drives
// the same production call sequence a full run makes — media.Probe's stream
// conversion via probeEvent, then runProgressive — in-process, the same way
// progressive_e2e_test.go's own end-to-end tests do, with the event emitter
// pointed at a buffer instead of stderr.
func TestFastRunEmitsTheExpectedEventSequence(t *testing.T) {
	dir, binDir, tmpDir, _ := progressiveStubs(t, dialogueSRT) // three chunks: a 30-minute stub file, 10-minute chunks
	stubTranslator(t, nil)

	input := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(input, []byte("not really a film"), 0o644); err != nil {
		t.Fatal(err)
	}

	streams := []media.Stream{{Index: 0, CodecType: "video"}, {Index: 1, CodecType: "audio"}}
	streams[1].Tags.Language = "eng"
	opts := whisper.Options{
		Bin:   filepath.Join(binDir, "stub-whisper"),
		Model: filepath.Join(dir, "ggml-test.bin"),
	}

	// Swapping the package var ev like this is safe only because nothing in
	// package main calls t.Parallel(): two tests racing on ev, or this test
	// racing against itself, would need its own synchronization, which
	// neither this test nor t.Cleanup provides.
	saved := ev
	t.Cleanup(func() { ev = saved })
	var buf bytes.Buffer
	ev = events.New(&buf)

	// The two events a real run emits before it ever reaches runProgressive:
	// main() emits Version right after parsing -events json, and Probe right
	// after media.Probe succeeds — both reproduced here via the exact
	// functions main() calls (events.New/Emit and probeEvent), rather than
	// re-deriving their shape by hand.
	ev.Emit(&events.Version{Events: events.ProtocolVersion, Tool: version})
	ev.Emit(probeEvent(input, streams, media.SubtitleStreams(streams)))

	if err := runProgressive(input, tmpDir, streams, -1, "en", "es",
		10*time.Minute, opts, translate.Options{}, ""); err != nil {
		t.Fatalf("runProgressive() = %v, want success", err)
	}

	var kinds []string
	var savedCount int
	var translateStages []map[string]any
	var doneEvent map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		kinds = append(kinds, m["type"].(string))
		if m["type"] == "saved" {
			savedCount++
			if m["watchable"] != true {
				t.Errorf("saved event is not marked watchable: %v", m)
			}
		}
		if m["type"] == "stage" && m["name"] == "translate" {
			translateStages = append(translateStages, m)
		}
		if m["type"] == "done" {
			doneEvent = m
		}
	}

	// "version" and "probe" are not asserted here: this test hand-emits both
	// two lines up, so checking for them would only prove the test's own
	// Emit calls succeeded. version-first and stderr-only are pinned by
	// TestEventsFlagLeavesStdoutUnchanged (real binary); the probe event's
	// shape, including duration, by TestOrdinaryRunProbeEventCarriesDuration
	// (also real binary). What's load-bearing here is what runProgressive
	// itself produces.
	for _, want := range []string{"stage", "progress", "saved", "done"} {
		if !slices.Contains(kinds, want) {
			t.Errorf("no %q event in the stream: %v", want, kinds)
		}
	}
	if savedCount != 3 {
		t.Errorf("%d saved events, want one per chunk (3): %v", savedCount, kinds)
	}
	if kinds[len(kinds)-1] != "done" {
		t.Errorf("last event = %q, want done", kinds[len(kinds)-1])
	}
	// Unlike the ordinary run, -fast knows exactly what span the file covers
	// at every point, chunk by chunk, so its done event must carry it —
	// Major 3 (fix round 2) only made Covers omittable, never absent here.
	if doneEvent == nil {
		t.Fatal("no done event captured")
	}
	if covers, _ := doneEvent["covers"].(string); covers != "0:30:00" {
		t.Errorf("done event covers = %q, want \"0:30:00\"", doneEvent["covers"])
	}

	// One "translate" stage per chunk, each carrying its own chunk number and
	// the total — not the single bare pre-loop emit this run used to produce.
	// A single-chunk run could not tell the two shapes apart, which is why
	// this run is three chunks: a bare {"name":"translate"} with no chunk/of
	// (omitempty drops zero fields, so a stray one would come back with no
	// "chunk" key at all) would collapse to one event regardless of chunk
	// count, while the per-chunk emit produces exactly one per chunk.
	if len(translateStages) != 3 {
		t.Fatalf("%d \"translate\" stage events, want one per chunk (3): %v", len(translateStages), translateStages)
	}
	for i, s := range translateStages {
		chunk, ok := s["chunk"].(float64)
		if !ok {
			t.Fatalf("translate stage %d has no chunk field — looks like the old pre-loop bare emit: %v", i+1, s)
		}
		of, ok := s["of"].(float64)
		if !ok {
			t.Fatalf("translate stage %d has no of field: %v", i+1, s)
		}
		if int(chunk) != i+1 {
			t.Errorf("translate stage %d: chunk = %v, want %d", i+1, chunk, i+1)
		}
		if int(of) != 3 {
			t.Errorf("translate stage %d: of = %v, want 3", i+1, of)
		}
	}
}

// The event stream is an addition, not a replacement, and TestEventsFlagLeavesStdoutUnchanged
// only proves that for -version — a single stdout write. This is the same
// constraint under load: a whole -fast translation, chunked and transcribed
// and translated, run twice into separate scratch directories, once with the
// event emitter enabled and once without. Every fmt.Printf this task added an
// Emit beside must still be the only thing that reaches stdout.
func TestAFullRunLeavesStdoutUnchangedUnderEvents(t *testing.T) {
	dir, binDir, _, _ := progressiveStubs(t, dialogueSRT)
	stubTranslator(t, nil)

	// The same input path and output filename in both runs: outPath is
	// derived from the input's basename and is itself printed ("Saved SRT:
	// %s ..."), so two different input names would make the two stdout
	// captures differ for a reason that has nothing to do with -events json.
	input := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(input, []byte("not really a film"), 0o644); err != nil {
		t.Fatal(err)
	}

	streams := []media.Stream{{Index: 0, CodecType: "video"}, {Index: 1, CodecType: "audio"}}
	streams[1].Tags.Language = "eng"
	opts := whisper.Options{
		Bin:   filepath.Join(binDir, "stub-whisper"),
		Model: filepath.Join(dir, "ggml-test.bin"),
	}

	// Same contract as the swap above: safe only because nothing in package
	// main calls t.Parallel(). This one additionally swaps os.Stdout via
	// captureStdout, which carries the same restriction.
	saved := ev
	t.Cleanup(func() { ev = saved })

	run := func(scratch string, enableEvents bool) string {
		t.Helper()
		tmpDir := filepath.Join(dir, scratch)
		if err := os.MkdirAll(tmpDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if enableEvents {
			ev = events.New(io.Discard)
		} else {
			ev = nil
		}
		return captureStdout(t, func() {
			if err := runProgressive(input, tmpDir, streams, -1, "en", "es",
				10*time.Minute, opts, translate.Options{}, ""); err != nil {
				t.Fatalf("runProgressive() = %v, want success", err)
			}
		})
	}

	without := run("plain-scratch", false)
	with := run("events-scratch", true)

	if without != with {
		t.Errorf("stdout differs with -events json enabled:\nwithout (%d bytes): %q\nwith (%d bytes):    %q",
			len(without), without, len(with), with)
	}
}

// Major 4 (task-6 fix round 2, partial): six of the eleven emits this task
// added live on the default, non -fast path, and none had a test. Fully
// closing that gap needs main()'s pipeline pulled into a runOrdinary function
// — out of scope for this round, carried to the final whole-branch review.
// What is reachable without that: transcribe() already stands on its own, so
// it is called directly here with the same stubs the -fast tests use, and its
// two Stage emits are asserted for real. Its language emit — main()'s own
// reaction to what transcribe found — was factored into reportDetectedLanguage
// specifically so this test could call the real function main() calls rather
// than re-deriving its two lines by hand the way the -fast sequence test
// reproduces Version and probeEvent (see its comment); that reproduction cost
// two tautological assertions (m11 in the review) this test does not want to
// repeat.
func TestTranscribeEmitsExtractTranscribeAndLanguageStages(t *testing.T) {
	dir, binDir, tmpDir, _ := progressiveStubs(t, dialogueSRT)

	input := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(input, []byte("not really a film"), 0o644); err != nil {
		t.Fatal(err)
	}
	streams := []media.Stream{{Index: 0, CodecType: "video"}, {Index: 1, CodecType: "audio"}}
	streams[1].Tags.Language = "eng"
	opts := whisper.Options{
		Bin:   filepath.Join(binDir, "stub-whisper"),
		Model: filepath.Join(dir, "ggml-test.bin"),
	}

	// Same contract as the other ev swaps in this file: safe only because
	// nothing in package main calls t.Parallel().
	saved := ev
	t.Cleanup(func() { ev = saved })
	var buf bytes.Buffer
	ev = events.New(&buf)

	// from is empty, so whatever whisper's stub sidecar reports ("en", fixed
	// by progressiveStubs) is new information — reportDetectedLanguage has
	// something to say about it, the same way a real run with no -from does.
	const from = ""
	blocks, lang, err := transcribe(input, tmpDir, streams, -1, from, opts)
	if err != nil {
		t.Fatalf("transcribe() = %v, want success", err)
	}
	if len(blocks) == 0 {
		t.Fatal("transcribe() returned no blocks")
	}
	if lang != "en" {
		t.Fatalf("test setup problem: transcribe() detected %q, want the stub's \"en\"", lang)
	}
	reportDetectedLanguage(from, lang)

	var kinds []string
	var stageNames []string
	var languageDetected string
	for _, line := range strings.Split(buf.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		kinds = append(kinds, m["type"].(string))
		if m["type"] == "stage" {
			if name, ok := m["name"].(string); ok {
				stageNames = append(stageNames, name)
			}
		}
		if m["type"] == "language" {
			languageDetected, _ = m["detected"].(string)
		}
	}

	if !slices.Contains(stageNames, "extract") {
		t.Errorf("no stage:extract event: %v", kinds)
	}
	if !slices.Contains(stageNames, "transcribe") {
		t.Errorf("no stage:transcribe event: %v", kinds)
	}
	if !slices.Contains(kinds, "language") {
		t.Fatalf("no language event: %v", kinds)
	}
	if languageDetected != "en" {
		t.Errorf("language event detected = %q, want %q", languageDetected, "en")
	}
}

// noisyFfmpegStub is an ffmpeg that behaves like the real one: a banner and
// carriage-return-terminated progress on stderr, and the output file written as
// the last argument. It is what makes the event stream unparseable when its
// stderr is inherited rather than captured.
const noisyFfmpegStub = `#!/bin/sh
printf 'ffmpeg version 8.1.1 Copyright (c) 2000-2026 the FFmpeg developers\n' >&2
printf '  configuration: --prefix=/opt/homebrew/Cellar/ffmpeg/8.1.1\n' >&2
printf 'size=       0KiB time=00:00:00.00 bitrate=N/A speed=N/A    \r' >&2
printf 'size=    7168KiB time=00:00:57.32 bitrate=1024.0kbits/s speed=  57x    \r' >&2
out=""
for a in "$@"; do out="$a"; done
: > "$out"
exit 0
`

// The branch's central claim is that -events json writes newline-delimited JSON
// to stderr. It did not: four ffmpeg invocations inherited the same file
// descriptor, so 40 lines of ffmpeg prose sat inside one run's stream and a
// progress line with no newline at its end could swallow an event outright.
//
// This drives the real binary down the subtitle path with a deliberately noisy
// ffmpeg and holds the whole stream to the documented rule: every line is either
// a whole JSON event or one of this tool's own prose lines. Nothing else may be
// there.
func TestFfmpegProseNeverReachesTheEventStream(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, filepath.Join(binDir, "ffprobe"), "#!/bin/sh\n"+`cat <<JSON
{"streams":[
 {"index":0,"codec_type":"subtitle","codec_name":"srt","tags":{"language":"eng"}}
],"format":{"duration":"120.5"}}
JSON`)
	writeStub(t, filepath.Join(binDir, "ffmpeg"), noisyFfmpegStub)

	film := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(film, []byte("not a film"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The empty-subtitle-track failure: it reaches ExtractSubtitle — so the
	// stub's noise is produced — and ends in one error event, without needing
	// whisper or the network.
	cmd := exec.Command(bin, "-events", "json", "-to", "es", "-from", "en", "-track", "0", film)
	cmd.Env = append(os.Environ(),
		"OPENAI_API_KEY=sk-test",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "cfg"),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("run succeeded, want the empty-subtitle-track failure:\n%s", stdout.String())
	}

	var events, prose int
	for _, line := range strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			events++
			continue
		}
		// Our own prose shares the stream by design, and a consumer is told to
		// skip it. Nothing else is allowed to be here.
		if strings.HasPrefix(line, "error: ") || strings.HasPrefix(line, "warning: ") {
			prose++
			continue
		}
		t.Errorf("stderr line is neither an event nor one of our prose lines: %q", line)
	}
	if events == 0 {
		t.Errorf("no events on stderr at all:\n%s", stderr.String())
	}
	if prose != 1 {
		t.Errorf("%d prose lines, want exactly the one \"error:\" line:\n%s", prose, stderr.String())
	}
	if strings.Contains(stderr.String(), "ffmpeg version") || strings.Contains(stderr.String(), "size=") {
		t.Errorf("ffmpeg's own output reached the event stream:\n%s", stderr.String())
	}
}

// eventsOf returns the JSON events on a stream, in order, skipping the prose
// lines a consumer is told to skip.
func eventsOf(stream string) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(stream, "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["type"] != nil {
			out = append(out, m)
		}
	}
	return out
}

// eventsOfKind returns just the events of one type.
func eventsOfKind(stream, kind string) []map[string]any {
	var out []map[string]any
	for _, m := range eventsOf(stream) {
		if m["type"] == kind {
			out = append(out, m)
		}
	}
	return out
}

// A run with no input path used to dump 56 lines of flag documentation and exit
// 1 with no error event at all — the argv-construction case for a wrapper (an
// empty path, a file that vanished between the drop and the launch), and the one
// where README and errkind.go both claim a terminal event is guaranteed.
func TestAMissingInputArgumentEmitsATerminalErrorEvent(t *testing.T) {
	bin := buildCLI(t)
	cmd := exec.Command(bin, "-events", "json", "-to", "es")
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+t.TempDir())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) {
		t.Fatalf("want a non-zero exit, got %v\nstderr: %s", err, stderr.String())
	}
	if code := exit.ExitCode(); code != 1 {
		t.Errorf("exit status %d, want 1 (the code this path has always used)", code)
	}
	errs := eventsOfKind(stderr.String(), "error")
	if len(errs) != 1 {
		t.Fatalf("%d error events, want exactly 1:\n%s", len(errs), stderr.String())
	}
	if kind, _ := errs[0]["kind"].(string); kind != "other" {
		t.Errorf("error kind = %q, want %q", kind, "other")
	}
	// The prose is unchanged: the usage text is still what a person sees.
	if !strings.Contains(stderr.String(), "Usage:\n  sub-translator [flags] <input>") {
		t.Errorf("the usage text is missing from stderr:\n%s", stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
}

// The case version negotiation exists for: a wrapper newer than the CLI it found
// on PATH, passing a flag that CLI does not have. flag.Parse used to print and
// exit 2 before the emitter was built, so the run that most needed to say "I am
// version 1" said nothing at all.
func TestAnUnknownFlagStillCarriesVersionAndAnErrorEvent(t *testing.T) {
	bin := buildCLI(t)
	cmd := exec.Command(bin, "-events", "json", "-bogus", "-to", "es", "movie.mkv")
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+t.TempDir())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) {
		t.Fatalf("want a non-zero exit, got %v\nstderr: %s", err, stderr.String())
	}
	// Unchanged: 2 is what flag's own ExitOnError used for a usage error.
	if code := exit.ExitCode(); code != 2 {
		t.Errorf("exit status %d, want 2", code)
	}

	all := eventsOf(stderr.String())
	if len(all) == 0 {
		t.Fatalf("no events at all:\n%s", stderr.String())
	}
	if all[0]["type"] != "version" {
		t.Errorf("first event = %v, want version", all[0]["type"])
	}
	if all[0]["events"] != float64(events.ProtocolVersion) {
		t.Errorf("version event events = %v, want %d", all[0]["events"], events.ProtocolVersion)
	}
	errs := eventsOfKind(stderr.String(), "error")
	if len(errs) != 1 {
		t.Fatalf("%d error events, want exactly 1:\n%s", len(errs), stderr.String())
	}
	if kind, _ := errs[0]["kind"].(string); kind != "other" {
		t.Errorf("error kind = %q, want %q", kind, "other")
	}
	if msg, _ := errs[0]["message"].(string); !strings.Contains(msg, "-bogus") {
		t.Errorf("error message = %q, want it to name the flag", msg)
	}
	// flag's own prose is unchanged too.
	if !strings.Contains(stderr.String(), "flag provided but not defined: -bogus") {
		t.Errorf("flag's own message is missing:\n%s", stderr.String())
	}
}

// -events json has to compose with the subcommands, not be refused by them: the
// flag worked on `config path` and `config list` before this branch existed (it
// was ignored), and a wrapper that appends it to every invocation must not get an
// inexplicable failure on a settings read. It also must not reach the
// subcommand's own argument count, which is what made `config get k -events json`
// fail with "config get: want exactly one key".
//
// The shape is the one -probe and -version already have: a version event, no
// terminal event. So every verb is run twice, with the flag and without, and the
// two runs must agree on stdout and on the exit code.
func TestSubcommandsComposeWithTheEventStream(t *testing.T) {
	bin := buildCLI(t)

	run := func(t *testing.T, cfg string, args ...string) (string, string, int) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+cfg)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := 0
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatalf("the binary did not run: %v", err)
		}
		return stdout.String(), stderr.String(), code
	}

	for _, verb := range [][]string{
		{"config", "path"},
		{"config", "list"},
		{"config", "get", "translate.model"},
		{"config", "set", "translate.model", "gpt-6-luna"},
		{"model", "list"},
	} {
		t.Run(strings.Join(verb, " "), func(t *testing.T) {
			// One config directory for both runs: the read-only verbs do not
			// touch it and `set` writes the same value twice, so the only
			// difference between the two runs can be the flag.
			cfg := t.TempDir()

			plainOut, plainErr, plainCode := run(t, cfg, verb...)
			withOut, withErr, withCode := run(t, cfg, append(append([]string{}, verb...), "-events", "json")...)

			if plainCode != 0 {
				t.Fatalf("the plain run failed with %d:\n%s", plainCode, plainErr)
			}
			if withCode != plainCode {
				t.Errorf("exit status %d with the flag, %d without:\n%s", withCode, plainCode, withErr)
			}
			if withOut != plainOut {
				t.Errorf("stdout differs with -events json:\nwithout: %q\nwith:    %q", plainOut, withOut)
			}
			if got := eventsOf(plainErr); len(got) != 0 {
				t.Errorf("a plain subcommand emitted %d events, want none: %v", len(got), got)
			}

			// Exactly one version event, and nothing else: a subcommand is not a
			// run, so it has no terminal event to wait for.
			all := eventsOf(withErr)
			if len(all) != 1 {
				t.Fatalf("%d events, want exactly one version event:\n%s", len(all), withErr)
			}
			if all[0]["type"] != "version" {
				t.Errorf("the one event is %v, want version", all[0]["type"])
			}
			if all[0]["events"] != float64(events.ProtocolVersion) {
				t.Errorf("version event events = %v, want %d", all[0]["events"], events.ProtocolVersion)
			}
		})
	}

	// An unsupported value is refused the same way the run path refuses it: the
	// one thing that cannot be answered in a format is that the format is not
	// understood.
	t.Run("an unsupported -events value is refused", func(t *testing.T) {
		_, stderr, code := run(t, t.TempDir(), "config", "path", "-events", "yaml")
		if code != 1 {
			t.Errorf("exit status %d, want 1", code)
		}
		if !strings.Contains(stderr, "json") {
			t.Errorf("error %q does not name the supported value", stderr)
		}
	})
}

// The subtitle path's whole pre-translate phase — extracting the track and
// parsing it — emitted nothing, so a window went probe → stage translate with
// minutes of silence over one of the two headline flows. The stub ffmpeg writes
// an empty track, which fails the run right after the stage is emitted, so this
// needs neither whisper nor the network.
func TestTheSubtitlePathEmitsTheExtractSubsStage(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, filepath.Join(binDir, "ffprobe"), "#!/bin/sh\n"+`cat <<JSON
{"streams":[
 {"index":0,"codec_type":"subtitle","codec_name":"srt","tags":{"language":"eng"}}
],"format":{"duration":"120.5"}}
JSON`)
	writeStub(t, filepath.Join(binDir, "ffmpeg"), noisyFfmpegStub)

	film := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(film, []byte("not a film"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "-events", "json", "-to", "es", "-from", "en", "-track", "0", film)
	cmd.Env = append(os.Environ(),
		"OPENAI_API_KEY=sk-test",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "cfg"),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("run succeeded, want the empty-subtitle-track failure:\n%s", stdout.String())
	}

	var names []string
	for _, m := range eventsOfKind(stderr.String(), "stage") {
		name, _ := m["name"].(string)
		names = append(names, name)
	}
	if !slices.Contains(names, "extract-subs") {
		t.Errorf("stage events = %v, want one named extract-subs:\n%s", names, stderr.String())
	}
}

// refusingTranslator points every client the run builds at an endpoint that
// answers one status and nothing else, so a whole run can be driven into the
// refusal a real endpoint gives a mistyped key.
func refusingTranslator(t *testing.T, status int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)

	real := newTranslateClient
	newTranslateClient = func(o translate.Options) (translate.Translator, error) {
		o.BaseURL = srv.URL
		if o.APIKey == "" {
			o.APIKey = "sk-wrong"
		}
		return translate.NewTranslator(o)
	}
	t.Cleanup(func() { newTranslateClient = real })
}

// A mistyped API key was reported as rate_limited by both run paths — the
// ordinary one because the kind was hard-coded at the call site, the -fast one
// because it was thrown away in a fmt.Errorf and no prefix matcher could get it
// back. A wrapper routes rate_limited to a Retry button, so the single most
// likely first-run error sent the user to the one place it cannot be fixed.
//
// The 401 cases go through the real client and a real refusing endpoint. The 429
// case is asserted on the sentinel instead: internal/translate's own suite drives
// 429 end to end with its retry delays turned down to milliseconds, and this
// package cannot reach them — four attempts at a second each, per batch, is not
// worth seven seconds of suite time to prove the same mapping twice.
func TestBothRunPathsClassifyARefusedTranslation(t *testing.T) {
	t.Run("the ordinary path: a rejected key", func(t *testing.T) {
		refusingTranslator(t, http.StatusUnauthorized)
		client, err := newTranslateClient(translate.Options{From: "en", To: "es"})
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = client.TranslateAll([]string{"one", "two"}, nil)
		if err == nil {
			t.Fatal("TranslateAll() = nil, want the endpoint's 401")
		}
		if got := classifyTranslateErr(err); got != errInvalidKey {
			t.Errorf("classifyTranslateErr(%v) = %q, want %q", err, got, errInvalidKey)
		}
	})

	t.Run("the -fast path: a rejected key, through the chunk loop", func(t *testing.T) {
		dir, binDir, tmpDir, _ := progressiveStubs(t, dialogueSRT)
		refusingTranslator(t, http.StatusUnauthorized)

		input := filepath.Join(dir, "movie.mkv")
		if err := os.WriteFile(input, []byte("not really a film"), 0o644); err != nil {
			t.Fatal(err)
		}
		streams := []media.Stream{{Index: 0, CodecType: "video"}, {Index: 1, CodecType: "audio"}}
		streams[1].Tags.Language = "eng"
		opts := whisper.Options{
			Bin:   filepath.Join(binDir, "stub-whisper"),
			Model: filepath.Join(dir, "ggml-test.bin"),
		}

		err := runProgressive(input, tmpDir, streams, -1, "en", "es",
			10*time.Minute, opts, translate.Options{}, "")
		if err == nil {
			t.Fatal("runProgressive() = nil, want the endpoint's 401")
		}
		kind, ok := classifyTranscribeErr(err)
		if !ok {
			t.Fatalf("classifyTranscribeErr(%v) = (%q, false), want a classified kind", err, kind)
		}
		if kind != errInvalidKey {
			t.Errorf("classifyTranscribeErr(%v) = %q, want %q", err, kind, errInvalidKey)
		}
	})

	t.Run("a rate limit stays rate_limited on both paths", func(t *testing.T) {
		// Wrapped the way both paths wrap it: translateAll's "all N blocks
		// failed", and the progressive run's per-chunk context over that.
		err := fmt.Errorf("chunk 3: translate: %w",
			fmt.Errorf("all 40 blocks failed: %w", translate.ErrRateLimited))
		if got := classifyTranslateErr(err); got != errRateLimited {
			t.Errorf("classifyTranslateErr = %q, want %q", got, errRateLimited)
		}
		kind, ok := classifyTranscribeErr(err)
		if !ok || kind != errRateLimited {
			t.Errorf("classifyTranscribeErr = (%q, %v), want (%q, true)", kind, ok, errRateLimited)
		}
	})

	t.Run("anything else the endpoint does is not routable", func(t *testing.T) {
		err := fmt.Errorf("all 40 blocks failed: %w",
			errors.New("the model returned 3 translations for 40 blocks"))
		if got := classifyTranslateErr(err); got != errOther {
			t.Errorf("classifyTranslateErr = %q, want %q — rate_limited now means HTTP 429 only", got, errOther)
		}
	})
}

// TestTheFastPathClassifiesAnUnwritableOutputPath is the -fast counterpart of
// TestAnUnwritableOutputPathIsClassified: runProgressive's own per-chunk write
// now builds a kindErrf(errOutputUnwritable, …) error, but that error still has
// to survive the trip back through runOrdinary's o.fast branch, which used to
// flatten every unrecognised failure out of runProgressive to "other" via
// errf("%v", err) — exactly the same loss classifyTranscribeErr's own doc
// comment already describes for the sentinels above. classifyTranscribeErr now
// checks kindOf(err) before falling through to the switch, which is what this
// asserts directly, the same way the sibling subtests above assert the
// existing sentinels survive that switch.
func TestTheFastPathClassifiesAnUnwritableOutputPath(t *testing.T) {
	dir, binDir, tmpDir, _ := progressiveStubs(t, dialogueSRT)
	stubTranslator(t, nil)

	input := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(input, []byte("not really a film"), 0o644); err != nil {
		t.Fatal(err)
	}
	streams := []media.Stream{{Index: 0, CodecType: "video"}, {Index: 1, CodecType: "audio"}}
	streams[1].Tags.Language = "eng"
	opts := whisper.Options{
		Bin:   filepath.Join(binDir, "stub-whisper"),
		Model: filepath.Join(dir, "ggml-test.bin"),
	}

	// runProgressive is handed "" for out, so it writes the same default sidecar
	// path media.DefaultSRTPath computes. Pre-creating a directory there makes
	// the first chunk's write fail without needing real filesystem permissions.
	outPath := media.DefaultSRTPath(input, "es")
	if err := os.Mkdir(outPath, 0o755); err != nil {
		t.Fatal(err)
	}

	err := runProgressive(input, tmpDir, streams, -1, "en", "es",
		10*time.Minute, opts, translate.Options{}, "")
	if err == nil {
		t.Fatal("runProgressive() = nil, want an error — the output path is a directory")
	}
	kind, ok := classifyTranscribeErr(err)
	if !ok {
		t.Fatalf("classifyTranscribeErr(%v) = (%q, false), want a classified kind", err, kind)
	}
	if kind != errOutputUnwritable {
		t.Errorf("classifyTranscribeErr(%v) = %q, want %q", err, kind, errOutputUnwritable)
	}
}

// The ordinary run's translate failure is the one classification a real binary
// can be driven into with a genuine connection failure rather than a status
// refusal: OPENAI_BASE_URL (Task 1) is pointed at 127.0.0.1:1, a port nothing
// ever listens on, so every request fails to connect and translateAll returns
// "all N blocks failed" with an error that is not a status refusal at all.
// That is the assertion this wants — the call site hard-coded errRateLimited,
// so a connection failure, a malformed reply and a mistyped key all arrived as
// "the endpoint is throttling you". Now the error is asked what it was, and
// something that is neither refusal reports "other".
//
// An earlier version of this test relied on HTTPS_PROXY/HTTP_PROXY instead,
// with a comment claiming "the request never leaves the machine: no network,
// and deterministic". That claim was false: cmd.Env appends to os.Environ(),
// and most getenv implementations return the *first* match for a duplicated
// name, so an ambient HTTPS_PROXY/HTTP_PROXY already present in the test
// runner's environment silently outranked the one this test set, making the
// failure mode — and sometimes the outcome — depend on where the suite ran.
// Routing through OPENAI_BASE_URL instead removes the dependency on any
// environment variable this process does not fully control: the CLI reads
// OPENAI_BASE_URL itself and there is exactly one way to set it.
func TestTheOrdinaryRunAsksTheErrorWhichRefusalItWas(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, filepath.Join(binDir, "ffprobe"), "#!/bin/sh\n"+`cat <<JSON
{"streams":[
 {"index":0,"codec_type":"subtitle","codec_name":"srt","tags":{"language":"eng"}}
],"format":{"duration":"120.5"}}
JSON`)
	// A real subtitle track this time: the run has to reach translation.
	writeStub(t, filepath.Join(binDir, "ffmpeg"), `#!/bin/sh
out=""
for a in "$@"; do out="$a"; done
printf '1\n00:00:01,000 --> 00:00:03,000\nHello\n\n2\n00:00:05,000 --> 00:00:07,000\nGoodbye\n\n' > "$out"
exit 0
`)

	film := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(film, []byte("not a film"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "-events", "json", "-to", "es", "-from", "en", "-track", "0", film)
	cmd.Env = append(os.Environ(),
		"OPENAI_API_KEY=sk-test",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "cfg"),
		// Port 1 has nothing listening, so every request fails at connect —
		// deterministically, and without depending on any ambient proxy
		// variable this process does not fully control (see the doc comment
		// above).
		"OPENAI_BASE_URL=http://127.0.0.1:1",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	var exit *exec.ExitError
	if err := cmd.Run(); !errors.As(err, &exit) {
		t.Fatalf("want a non-zero exit, got %v\nstdout: %s\nstderr: %s", err, stdout.String(), stderr.String())
	}
	if code := exit.ExitCode(); code != 1 {
		t.Errorf("exit status %d, want 1", code)
	}

	// The run got as far as translation, or this proves nothing.
	var stageNames []string
	for _, m := range eventsOfKind(stderr.String(), "stage") {
		name, _ := m["name"].(string)
		stageNames = append(stageNames, name)
	}
	if !slices.Contains(stageNames, "translate") {
		t.Fatalf("the run never reached the translate stage (stages: %v)\nstdout: %s\nstderr: %s",
			stageNames, stdout.String(), stderr.String())
	}

	errs := eventsOfKind(stderr.String(), "error")
	if len(errs) != 1 {
		t.Fatalf("%d error events, want exactly 1:\n%s", len(errs), stderr.String())
	}
	kind, _ := errs[0]["kind"].(string)
	if kind == string(errRateLimited) {
		t.Errorf("a connection failure is reported as %q — the kind is still hard-coded at the call site:\n%s",
			kind, stderr.String())
	}
	if kind != string(errOther) {
		t.Errorf("error kind = %q, want %q", kind, errOther)
	}
}

// The printed percentage and the emitted one are computed from the same division
// but formatted in two places, and they diverged once with every test green:
// %.0f rounds halves to even, int() truncates, so 3 of 8 printed 37 while the
// event said 38. pctOf was fixed; nothing pinned the other half of the equality,
// so changing the printed line alone re-opened it. This reads the number out of
// the line a person actually sees and holds it against the number the event
// carries — never against a hand-written expectation.
func TestThePrintedAndEmittedPercentagesCannotDiverge(t *testing.T) {
	percentIn := regexp.MustCompile(`\((\d+)%\)`)
	for _, tt := range []struct{ done, total int }{
		{1, 8},  // 12.5% — exact half: to-even (12) against away-from-zero (13)
		{3, 8},  // 37.5% — the pair the review reproduced: printed 37, emitted 38
		{5, 8},  // 62.5%
		{1, 40}, // 2.5%
		{3, 40}, // 7.5%
		{0, 1},
		{1, 1},
		{1, 3},
		{2, 3},
		{7, 8},
		{99, 200},
		{100, 200},
		{101, 200},
	} {
		line := progressLine(tt.done, tt.total)
		// The line is also what a person reads, so its shape is asserted: a
		// formatter that dropped the fraction would satisfy the equality below
		// while printing something else entirely.
		if want := fmt.Sprintf("progress: %d/%d ", tt.done, tt.total); !strings.Contains(line, want) {
			t.Errorf("progressLine(%d, %d) = %q, want it to carry %q", tt.done, tt.total, line, want)
		}
		m := percentIn.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("progressLine(%d, %d) = %q carries no (N%%) percentage", tt.done, tt.total, line)
		}
		printed, err := strconv.Atoi(m[1])
		if err != nil {
			t.Fatalf("progressLine(%d, %d) = %q: %q is not an integer", tt.done, tt.total, line, m[1])
		}
		if emitted := pctOf(tt.done, tt.total); printed != emitted {
			t.Errorf("done=%d total=%d: the line prints %d%% and the event emits %d%%",
				tt.done, tt.total, printed, emitted)
		}
	}
}

// An off feature must cost nothing, and -probe was the surviving hole in that
// rule: probeEvent's duration lookup is a second ffprobe, Go evaluates Emit's
// argument before Emit's nil check runs, and the human -probe output never prints
// a duration — so every flagless -probe spawned a subprocess whose result was
// thrown away. This counts the invocations.
func TestProbeSpawnsFfprobeOnlyWhenEventsAreOn(t *testing.T) {
	bin := buildCLI(t)

	countFfprobeCalls := func(t *testing.T, extraArgs ...string) int {
		t.Helper()
		dir := t.TempDir()
		binDir := filepath.Join(dir, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatal(err)
		}
		log := filepath.Join(dir, "ffprobe.log")
		writeStub(t, filepath.Join(binDir, "ffprobe"), "#!/bin/sh\necho called >> \""+log+"\"\n"+`cat <<JSON
{"streams":[
 {"index":0,"codec_type":"audio","codec_name":"ac3","tags":{"language":"eng"}}
],"format":{"duration":"120.5"}}
JSON`)

		film := filepath.Join(dir, "movie.mkv")
		if err := os.WriteFile(film, []byte("not a film"), 0o600); err != nil {
			t.Fatal(err)
		}

		cmd := exec.Command(bin, append(append([]string{"-probe"}, extraArgs...), film)...)
		cmd.Env = append(os.Environ(),
			"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			"XDG_CONFIG_HOME="+filepath.Join(dir, "cfg"),
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("-probe failed: %v\nstderr: %s", err, stderr.String())
		}

		b, err := os.ReadFile(log)
		if err != nil {
			if os.IsNotExist(err) {
				return 0
			}
			t.Fatal(err)
		}
		// One word per line: the stub echoes "called".
		return len(strings.Fields(string(b)))
	}

	t.Run("without -events json: one ffprobe call", func(t *testing.T) {
		if got := countFfprobeCalls(t); got != 1 {
			t.Errorf("ffprobe invoked %d time(s), want 1 (just media.Probe)", got)
		}
	})

	t.Run("with -events json: two ffprobe calls", func(t *testing.T) {
		if got := countFfprobeCalls(t, "-events", "json"); got != 2 {
			t.Errorf("ffprobe invoked %d time(s), want 2 (media.Probe, plus probeEvent's duration)", got)
		}
	})
}

// waitForEvent polls a stderr file until it carries a line with the given
// substring, which is how a test knows a subprocess has reached a stage without
// guessing at a sleep.
func waitForEvent(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil && strings.Contains(string(b), want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	b, _ := os.ReadFile(path)
	t.Fatalf("timed out waiting for %q on the event stream:\n%s", want, b)
}

// waitForFile polls until path exists, which is how a stub subprocess says it
// has got far enough for the signal to land where the test wants it.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

// Cancellation had no test at all: deleting the emit at the SIGINT handler left
// the whole suite green, and nothing asserted exit 130 anywhere. It is also the
// event the stream's unparseability destroyed — a JSON object glued to the tail
// of an ffmpeg progress line is unreadable by any line-oriented consumer, so the
// assertion here is deliberately made the way a consumer would make it: parse
// every line, and the cancelled event must be one of them.
//
// stderr goes to a file rather than a pipe on purpose. SIGINT does not kill the
// ffmpeg child (a separate, deferred finding), and a pipe would keep this test
// waiting on the orphan the way it would keep a GUI waiting.
func TestCancellationExits130WithExactlyOneCancelledEvent(t *testing.T) {
	bin := buildCLI(t)

	// A loop of carriage-return progress, so a signal arriving at any moment
	// arrives while a line with no newline at its end is in flight: the exact
	// window the cancelled event used to be lost in.
	const sleepyFfmpeg = `#!/bin/sh
out=""
for a in "$@"; do out="$a"; done
printf 'size=       0KiB time=00:00:00.00 bitrate=1024.0kbits/s speed=57x    \r' >&2
# Only now: the signal must arrive with a line that has no newline at its end
# already in flight, which is the window the cancelled event was lost in.
: > "$FFMPEG_STARTED"
i=0
while [ $i -lt 40 ]; do
  printf 'size=%8dKiB time=00:00:00.00 bitrate=1024.0kbits/s speed=57x    \r' "$i" >&2
  sleep 0.1
  i=$((i+1))
done
printf 'RIFFWAVE' > "$out"
exit 0
`
	const quickFfmpeg = `#!/bin/sh
out=""
for a in "$@"; do out="$a"; done
printf 'RIFFWAVE' > "$out"
exit 0
`
	const sleepyWhisper = `#!/bin/sh
outbase=""
while [ $# -gt 0 ]; do
  case "$1" in -of) outbase="$2" ;; esac
  shift
done
echo "whisper_print_progress_callback: progress =  10%" >&2
i=0
while [ $i -lt 40 ]; do
  sleep 0.1
  i=$((i+1))
done
: > "$outbase.srt"
exit 0
`

	for _, tt := range []struct {
		name string
		// ffmpeg and whisper are the two stubs; one of them blocks, and which
		// one decides where the signal lands.
		ffmpeg, whisper string
		// waitFor is the line on the event stream that says the run has reached
		// the blocking step.
		waitFor string
		// waitForFfmpeg additionally waits until the ffmpeg stub has written a
		// carriage-return fragment, so the signal cannot win the race against
		// the child starting up.
		waitForFfmpeg bool
	}{
		{
			name:          "during audio extraction, mid ffmpeg progress line",
			ffmpeg:        sleepyFfmpeg,
			whisper:       quickFfmpeg, // never reached
			waitFor:       `"name":"extract"`,
			waitForFfmpeg: true,
		},
		{
			name:    "during transcription",
			ffmpeg:  quickFfmpeg,
			whisper: sleepyWhisper,
			// whisper's own progress reaching the stream means whisper.Run is
			// scanning its piped stderr and the run is inside transcription.
			waitFor: `"type":"progress"`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			binDir := filepath.Join(dir, "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				t.Fatal(err)
			}
			writeStub(t, filepath.Join(binDir, "ffprobe"), "#!/bin/sh\n"+`cat <<JSON
{"streams":[
 {"index":0,"codec_type":"audio","codec_name":"ac3","tags":{"language":"eng"}}
],"format":{"duration":"1800.0"}}
JSON`)
			writeStub(t, filepath.Join(binDir, "ffmpeg"), tt.ffmpeg)
			whisperBin := filepath.Join(binDir, "stub-whisper")
			writeStub(t, whisperBin, tt.whisper)
			model := touch(t, filepath.Join(dir, "ggml-test.bin"))
			film := filepath.Join(dir, "movie.mkv")
			if err := os.WriteFile(film, []byte("not a film"), 0o600); err != nil {
				t.Fatal(err)
			}

			errPath := filepath.Join(dir, "stderr.log")
			errFile, err := os.Create(errPath)
			if err != nil {
				t.Fatal(err)
			}
			defer errFile.Close()

			cmd := exec.Command(bin, "-events", "json", "-to", "es", "-source", "audio",
				"-whisper-bin", whisperBin, "-whisper-model", model, film)
			started := filepath.Join(dir, "ffmpeg-started")
			cmd.Env = append(os.Environ(),
				"OPENAI_API_KEY=sk-test",
				"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"XDG_CONFIG_HOME="+filepath.Join(dir, "cfg"),
				"FFMPEG_STARTED="+started,
			)
			cmd.Stdout = io.Discard
			cmd.Stderr = errFile
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}

			waitForEvent(t, errPath, tt.waitFor)
			if tt.waitForFfmpeg {
				waitForFile(t, started)
			}
			if err := cmd.Process.Signal(os.Interrupt); err != nil {
				t.Fatal(err)
			}
			runErr := cmd.Wait()

			var exit *exec.ExitError
			if !errors.As(runErr, &exit) {
				t.Fatalf("want a signalled exit, got %v", runErr)
			}
			// 128 + SIGINT, the shell convention, and what a wrapper's Cancel
			// button reads when the kind cannot be recovered.
			if code := exit.ExitCode(); code != 130 {
				t.Errorf("exit status %d, want 130", code)
			}

			stream, err := os.ReadFile(errPath)
			if err != nil {
				t.Fatal(err)
			}
			errs := eventsOfKind(string(stream), "error")
			if len(errs) != 1 {
				t.Fatalf("%d error events, want exactly one parseable cancelled event:\n%s",
					len(errs), stream)
			}
			if kind, _ := errs[0]["kind"].(string); kind != string(errCancelled) {
				t.Errorf("error kind = %q, want %q", kind, errCancelled)
			}
			if got := eventsOfKind(string(stream), "done"); len(got) != 0 {
				t.Errorf("%d done events on a cancelled run, want none", len(got))
			}
		})
	}
}

// The failure side of the exit-code/terminal-event pairing is well covered; the
// success side was not covered at all — both done emits could be deleted with the
// suite green. This asserts the pairing on the -fast path: the run succeeds,
// which is what main turns into exit 0 by returning, and the stream ends in
// exactly one done and no error.
//
// It is in-process for a reason worth stating rather than hiding: a compiled
// binary cannot be pointed at a test translation endpoint, so no subprocess test
// can reach either success path at all. That seam — honouring OPENAI_BASE_URL —
// is deliberately out of scope for this round, which leaves the ordinary run's
// own done emit unpinned. The gap is named in the report, not papered over here.
func TestASuccessfulRunEndsInExactlyOneDoneEventAndNoError(t *testing.T) {
	dir, binDir, tmpDir, _ := progressiveStubs(t, dialogueSRT)
	stubTranslator(t, nil)

	input := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(input, []byte("not really a film"), 0o644); err != nil {
		t.Fatal(err)
	}
	streams := []media.Stream{{Index: 0, CodecType: "video"}, {Index: 1, CodecType: "audio"}}
	streams[1].Tags.Language = "eng"
	opts := whisper.Options{
		Bin:   filepath.Join(binDir, "stub-whisper"),
		Model: filepath.Join(dir, "ggml-test.bin"),
	}

	// Same contract as the other ev swaps in this file: safe only because
	// nothing in package main calls t.Parallel().
	saved := ev
	t.Cleanup(func() { ev = saved })
	var buf bytes.Buffer
	ev = events.New(&buf)

	// A nil error is the success main() acts on: it returns, and the process
	// exits 0.
	if err := runProgressive(input, tmpDir, streams, -1, "en", "es",
		10*time.Minute, opts, translate.Options{}, ""); err != nil {
		t.Fatalf("runProgressive() = %v, want success", err)
	}

	all := eventsOf(buf.String())
	if len(all) == 0 {
		t.Fatal("no events at all")
	}
	dones := eventsOfKind(buf.String(), "done")
	if len(dones) != 1 {
		t.Fatalf("%d done events, want exactly 1:\n%s", len(dones), buf.String())
	}
	if got := eventsOfKind(buf.String(), "error"); len(got) != 0 {
		t.Errorf("%d error events on a successful run, want none:\n%s", len(got), buf.String())
	}
	if last := all[len(all)-1]; last["type"] != "done" {
		t.Errorf("last event = %v, want done", last["type"])
	}
	if path, _ := dones[0]["path"].(string); path == "" {
		t.Error("the done event carries no path")
	}
}

// The argv pre-scan that lets a parse failure be reported on the stream must read
// os.Args the way flag.Parse is about to read it, or the two disagree about the
// same command line — and a user is then told that one flag works after the input
// path while every other flag does not. These four cases are where a scan that
// was merely "look for -events anywhere" and a scan that merely "stops at the
// first word without a dash" each get it wrong.
func TestTheEventsFlagIsHonouredWhereFlagParseWouldHonourIt(t *testing.T) {
	bin := buildCLI(t)

	run := func(t *testing.T, args ...string) (string, int) {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+t.TempDir())
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("want a non-zero exit, got %v\nstderr: %s", err, stderr.String())
		}
		return stderr.String(), exit.ExitCode()
	}

	missing := filepath.Join(t.TempDir(), "nonexistent.mkv")

	t.Run("before the input path: honoured", func(t *testing.T) {
		stderr, code := run(t, "-events", "json", "-to", "es", missing)
		if code != 1 {
			t.Errorf("exit status %d, want 1", code)
		}
		all := eventsOf(stderr)
		if len(all) == 0 || all[0]["type"] != "version" {
			t.Fatalf("want a version event first:\n%s", stderr)
		}
		if got := eventsOfKind(stderr, "error"); len(got) != 1 {
			t.Errorf("%d error events, want exactly 1:\n%s", len(got), stderr)
		}
	})

	// The case a scan that stopped at the first dashless word would break: "es"
	// is -to's value, not the input path, so flag.Parse reads straight past it
	// and so must the scan. The parse then fails on a flag it does define with a
	// value it cannot use, which is what makes the scan's answer observable —
	// after a successful parse the flag itself would switch the stream on.
	t.Run("after another flag's value: honoured", func(t *testing.T) {
		stderr, code := run(t, "-to", "es", "-workers=notanumber", "-events", "json", missing)
		if code != 2 {
			t.Errorf("exit status %d, want 2 (a bad flag value is a usage error)", code)
		}
		all := eventsOf(stderr)
		if len(all) == 0 || all[0]["type"] != "version" {
			t.Fatalf("want a version event first — the scan stopped at -to's value:\n%s", stderr)
		}
		errs := eventsOfKind(stderr, "error")
		if len(errs) != 1 {
			t.Fatalf("%d error events, want exactly 1:\n%s", len(errs), stderr)
		}
		if msg, _ := errs[0]["message"].(string); !strings.Contains(msg, "workers") {
			t.Errorf("error message = %q, want it to name the flag at fault", msg)
		}
	})

	// After the input path, flag.Parse honours nothing — every flag there is a
	// positional argument — so this must not switch the stream on either. It used
	// to, which was the one place the two disagreed.
	t.Run("after the input path: ignored, like every other flag", func(t *testing.T) {
		stderr, code := run(t, "-to", "es", missing, "-events", "json")
		if code != 1 {
			t.Errorf("exit status %d, want 1", code)
		}
		if got := eventsOf(stderr); len(got) != 0 {
			t.Errorf("%d events, want none — flag.Parse ignores this flag here and so must the scan: %v\n%s",
				len(got), got, stderr)
		}
		// The failure is still reported to a person.
		if !strings.Contains(stderr, "error: input file not found") {
			t.Errorf("the prose error line is missing:\n%s", stderr)
		}
	})

	// The deliberate asymmetry: at a flag this CLI does not define, flag.Parse is
	// about to fail rather than produce a run, so the scan keeps looking. A
	// wrapper newer than the CLI it found on PATH is exactly the case the early
	// scan exists for, and silence is the worst possible answer to it.
	t.Run("at an unknown flag the scan keeps looking", func(t *testing.T) {
		stderr, code := run(t, "-bogus", "-events", "json", missing)
		if code != 2 {
			t.Errorf("exit status %d, want 2", code)
		}
		all := eventsOf(stderr)
		if len(all) == 0 || all[0]["type"] != "version" {
			t.Fatalf("want a version event first:\n%s", stderr)
		}
		if got := eventsOfKind(stderr, "error"); len(got) != 1 {
			t.Errorf("%d error events, want exactly 1:\n%s", len(got), stderr)
		}
	})
}

// Asking for help is not a usage error. flag's own ExitOnError exits 0 on -h,
// and switching to ContinueOnError to catch the unknown-flag case silently
// changed that to 2 — a regression in this branch, caught by comparing against
// the binary built before it. Like -version, -h ends with no terminal event.
func TestHelpExitsZeroAndCarriesNoTerminalEvent(t *testing.T) {
	bin := buildCLI(t)

	for _, flagName := range []string{"-h", "-help", "--help"} {
		t.Run(flagName, func(t *testing.T) {
			cmd := exec.Command(bin, "-events", "json", flagName)
			cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+t.TempDir())
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("%s exited non-zero: %v\nstderr: %s", flagName, err, stderr.String())
			}
			if !strings.Contains(stderr.String(), "Usage:\n  sub-translator [flags] <input>") {
				t.Errorf("%s printed no usage text:\n%s", flagName, stderr.String())
			}
			all := eventsOf(stderr.String())
			if len(all) != 1 || all[0]["type"] != "version" {
				t.Errorf("%d events, want exactly one version event: %v", len(all), all)
			}
			if stdout.String() != "" {
				t.Errorf("stdout = %q, want nothing", stdout.String())
			}
		})
	}
}

// The reviewer's case, on the compiled binary: a run that omits -to reaches
// promptLang, which writes its question without a newline because the answer is
// typed on the same line. A SIGINT there used to glue the cancelled event onto
// the prompt — the same terminal event, the same Cancel path and the same
// unparseable line as the ffmpeg failure, this time in our own code. promptLang
// is not gated on stdin being a terminal, so any run missing -to is exposed.
func TestCancellationAtAPromptStillEmitsAParseableEvent(t *testing.T) {
	bin := buildCLI(t)
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, filepath.Join(binDir, "ffprobe"), "#!/bin/sh\n"+`cat <<JSON
{"streams":[
 {"index":0,"codec_type":"subtitle","codec_name":"srt","tags":{"language":"eng"}}
],"format":{"duration":"120.5"}}
JSON`)
	writeStub(t, filepath.Join(binDir, "ffmpeg"), noisyFfmpegStub)

	film := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(film, []byte("not a film"), 0o600); err != nil {
		t.Fatal(err)
	}

	errPath := filepath.Join(dir, "stderr.log")
	errFile, err := os.Create(errPath)
	if err != nil {
		t.Fatal(err)
	}
	defer errFile.Close()

	// stdin stays open and silent, so the prompt blocks exactly as it does for a
	// person who has not typed anything yet.
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdinW.Close()

	// No -to: the run reaches the target-language prompt. -source sub with a
	// track present needs no confirmation and no whisper.
	cmd := exec.Command(bin, "-events", "json", "-source", "sub", "-from", "en", "-track", "0", film)
	cmd.Env = append(os.Environ(),
		"OPENAI_API_KEY=sk-test",
		"PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"),
		"XDG_CONFIG_HOME="+filepath.Join(dir, "cfg"),
	)
	cmd.Stdin = stdinR
	cmd.Stdout = io.Discard
	cmd.Stderr = errFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stdinR.Close()

	// The prompt itself is the signal that the run is waiting, and that the
	// stream is now mid-line.
	waitForEvent(t, errPath, "language code")
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	runErr := cmd.Wait()

	var exit *exec.ExitError
	if !errors.As(runErr, &exit) {
		t.Fatalf("want a signalled exit, got %v", runErr)
	}
	if code := exit.ExitCode(); code != 130 {
		t.Errorf("exit status %d, want 130", code)
	}

	stream, err := os.ReadFile(errPath)
	if err != nil {
		t.Fatal(err)
	}
	errs := eventsOfKind(string(stream), "error")
	if len(errs) != 1 {
		t.Fatalf("%d error events, want exactly one parseable cancelled event:\n%s", len(errs), stream)
	}
	if kind, _ := errs[0]["kind"].(string); kind != string(errCancelled) {
		t.Errorf("error kind = %q, want %q", kind, errCancelled)
	}
	// And the prompt a person was reading is still intact on its own line.
	if !strings.Contains(string(stream), "language code (e.g. en, ru, es, fr): \n") {
		t.Errorf("the prompt line was not broken off before the event:\n%q", stream)
	}
}

// boolFlagsInUsage are the flags that take no value. The scan reads the rest of
// them as "this word is my value, not the input path", which is the only way
// `-to es -events json` works; a boolean followed by a word means that word is
// positional, for flag.Parse and for the scan alike. If a new boolean flag is
// added and the test below starts failing for it, add it here.
var boolFlagsInUsage = map[string]bool{
	"-fast": true, "-probe": true, "-version": true,
	// -events is the flag under test, not a subject of it.
	"-events": true,
}

// flagInUsage matches a flag's line in the usage text.
var flagInUsage = regexp.MustCompile(`(?m)^  (-[a-z-]+) `)

// The pre-scan asks flag.CommandLine which flags take a value, so it is correct
// only while every flag is defined above the call. The reviewer moved one below
// it: argv that parses still worked, because the flag itself switches the stream
// on after a successful Parse — but `-<moved flag> v -bogus -events json film`
// silently stopped emitting and M2 was reopened with the suite green.
//
// This drives the real binary once per value-taking flag, taking the list from
// the usage text so a flag added later is covered without anyone remembering to
// come back here. The property asserted is the one that breaks: an argv the CLI
// cannot parse still gets its version event and its error event.
func TestEveryValueTakingFlagIsDefinedBeforeTheArgvScan(t *testing.T) {
	bin := buildCLI(t)
	missing := filepath.Join(t.TempDir(), "nonexistent.mkv")

	names := flagInUsage.FindAllStringSubmatch(usage, -1)
	if len(names) < 10 {
		t.Fatalf("found %d flags in the usage text; the scan itself is broken", len(names))
	}

	tested := 0
	for _, m := range names {
		name := m[1]
		if boolFlagsInUsage[name] {
			continue
		}
		tested++
		t.Run(name, func(t *testing.T) {
			// "1" is an acceptable word for every value-taking flag here,
			// string or int; the run never gets far enough to interpret it,
			// because -bogus fails the parse first.
			cmd := exec.Command(bin, name, "1", "-bogus", "-events", "json", missing)
			cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+t.TempDir())
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr

			var exit *exec.ExitError
			if err := cmd.Run(); !errors.As(err, &exit) {
				t.Fatalf("want a non-zero exit, got %v\nstderr: %s", err, stderr.String())
			}
			if code := exit.ExitCode(); code != 2 {
				t.Errorf("exit status %d, want 2", code)
			}
			all := eventsOf(stderr.String())
			if len(all) == 0 || all[0]["type"] != "version" {
				t.Fatalf("no version event: the scan stopped at %s's value, so %s is declared "+
					"below the eventsRequested call (or it is a boolean flag missing from "+
					"boolFlagsInUsage)\nstderr: %s", name, name, stderr.String())
			}
			if got := eventsOfKind(stderr.String(), "error"); len(got) != 1 {
				t.Errorf("%d error events, want exactly 1:\n%s", len(got), stderr.String())
			}
		})
	}
	if tested < 8 {
		t.Errorf("only %d value-taking flags were exercised, want the whole documented set", tested)
	}
}
