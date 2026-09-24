package whisper

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
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

// TestRunHandlesStderrReadError verifies that a scanner error on stderr (e.g.,
// a line longer than the buffer) doesn't cause cmd.Wait to deadlock on a full
// pipe. The Run function must drain the pipe even after a read error, otherwise
// Wait() blocks forever waiting for the process to write its remaining stderr.
func TestRunHandlesStderrReadError(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "bad-whisper")

	// Create a shell script that writes a line longer than 1 MB to stderr.
	// The scanner buffer is 1 MB, so this will trigger a read error.
	scriptContent := `#!/bin/sh
awk 'BEGIN{for(i=1;i<=2097152;i++) printf "x"}' >&2
exit 1
`
	if err := os.WriteFile(script, []byte(scriptContent), 0o755); err != nil {
		t.Fatal(err)
	}

	// Run with a timeout to detect deadlocks. If the stderr drain is missing,
	// this will hang indefinitely on cmd.Wait().
	done := make(chan bool)
	var err error

	go func() {
		_, err = Run(Options{
			Bin:     script,
			Model:   "dummy",
			Audio:   "dummy",
			OutBase: filepath.Join(dir, "out"),
		}, nil)
		done <- true
	}()

	select {
	case <-done:
		// Good, it returned. A scanner error should have been captured.
		if err == nil {
			t.Error("expected error from script with long stderr")
		}
		if !strings.Contains(err.Error(), "whisper") {
			t.Errorf("error should mention whisper: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run() hung for 30 seconds; scanner error was not handled and deadlocked on stderr pipe")
	}
}
