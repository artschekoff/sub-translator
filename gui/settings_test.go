package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shQuote wraps s in single quotes for embedding as a literal in a POSIX
// shell script, escaping any embedded single quote with the standard
// close-quote/escaped-quote/reopen-quote trick. None of the fixtures below
// need the escaping (dotted config keys, plain values, a secret with no
// quote characters in it) but every stub built here gets it for free rather
// than needing every caller to remember it is unsafe otherwise.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// writeArgvLoggingStub creates an executable POSIX shell script at
// dir/sub-translator that plays the part of the real CLI's `config`
// subcommand for these tests. Every invocation appends its argv to
// dir/argv.log, one call per line, with arguments separated by \x1f (a
// byte no test fixture here uses) so readArgvLog can split a line back into
// exact arguments even if one contained a space.
//
//   - `config get <key>` (exactly one key argument) answers from
//     getResponses, or fails loudly if the key has no entry — a stub that
//     silently returned "" for an unrecognized key could mask a bug where
//     ReadSettings asks for the wrong key.
//   - `config set <key> <value>` (exactly one key and one value) always
//     answers with setExitCode and setStderr, regardless of which key or
//     value it was called with — the tests that use this care about what
//     WriteSetting does with the CLI's answer, not about varying the
//     answer by key.
//   - anything else — including `config list`, which is never wired to
//     succeed here — fails loudly. A ReadSettings that regressed to
//     parsing `list` instead of calling `get` would see this stub reject
//     it, not quietly hand back parseable output.
func writeArgvLoggingStub(t *testing.T, dir string, getResponses map[string]string, setExitCode int, setStderr string) (cli, log string) {
	t.Helper()

	log = filepath.Join(dir, "argv.log")

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("{ for a in \"$@\"; do printf '%s\\037' \"$a\"; done; printf '\\n'; } >> " + shQuote(log) + "\n")

	b.WriteString("if [ \"$1\" = config ] && [ \"$2\" = get ] && [ $# -eq 3 ]; then\n")
	b.WriteString("  case \"$3\" in\n")
	for key, value := range getResponses {
		b.WriteString("    " + key + ") printf '%s\\n' " + shQuote(value) + "; exit 0 ;;\n")
	}
	b.WriteString("  esac\n")
	b.WriteString("  echo \"stub: no response configured for config get $3\" 1>&2\n")
	b.WriteString("  exit 9\n")
	b.WriteString("fi\n")

	b.WriteString("if [ \"$1\" = config ] && [ \"$2\" = set ] && [ $# -eq 4 ]; then\n")
	fmt.Fprintf(&b, "  printf '%%s\\n' %s 1>&2\n", shQuote(setStderr))
	fmt.Fprintf(&b, "  exit %d\n", setExitCode)
	b.WriteString("fi\n")

	b.WriteString("echo \"stub: unexpected invocation: $*\" 1>&2\n")
	b.WriteString("exit 9\n")

	path := filepath.Join(dir, "sub-translator")
	if err := os.WriteFile(path, []byte(b.String()), 0o755); err != nil {
		t.Fatalf("writing stub CLI: %v", err)
	}
	return path, log
}

// readArgvLog reads back the calls writeArgvLoggingStub's script recorded,
// each as the slice of arguments (argv[1:], the script's own path is not
// included) it was invoked with.
func readArgvLog(t *testing.T, path string) [][]string {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading argv log: %v", err)
	}

	var calls [][]string
	for _, line := range strings.Split(strings.TrimRight(string(body), "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\x1f")
		// The trailing \x1f before the newline leaves one empty element at
		// the end of the split; drop it.
		if len(parts) > 0 && parts[len(parts)-1] == "" {
			parts = parts[:len(parts)-1]
		}
		calls = append(calls, parts)
	}
	return calls
}

// TestReadSettingsUsesGetNotList is the point of the task: `config list`
// prints aligned columns for a human, and parsing them would turn a prose
// layout into this window's API. This is asserted against the argv the
// stub actually recorded, not against ReadSettings' return value alone —
// a rewrite to `config list` that happened to still parse correctly would
// pass a value-only check silently.
func TestReadSettingsUsesGetNotList(t *testing.T) {
	dir := t.TempDir()
	responses := map[string]string{
		"translate.api-key":     "(unset)",
		"translate.model":       "gpt-4o-mini",
		"translate.workers":     "4",
		"whisper.bin":           "/usr/local/bin/whisper",
		"whisper.model":         "/models/ggml-base.bin",
		"whisper.vad-model":     "(unset)",
		"whisper.chunk-minutes": "10",
		"models.dir":            "/models",
	}
	cli, log := writeArgvLoggingStub(t, dir, responses, 9, "unexpected config set call")

	got, err := ReadSettings(context.Background(), cli)
	if err != nil {
		t.Fatalf("ReadSettings() error = %v, want nil", err)
	}

	want := map[string]string{
		"translate.api-key":     "",
		"translate.model":       "gpt-4o-mini",
		"translate.workers":     "4",
		"whisper.bin":           "/usr/local/bin/whisper",
		"whisper.model":         "/models/ggml-base.bin",
		"whisper.vad-model":     "",
		"whisper.chunk-minutes": "10",
		"models.dir":            "/models",
	}
	if len(got) != len(want) {
		t.Fatalf("ReadSettings() returned %d keys, want %d: %+v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("ReadSettings()[%q] = %q, want %q", k, got[k], v)
		}
	}

	calls := readArgvLog(t, log)

	usedList := false
	gotKeys := map[string]int{}
	for _, call := range calls {
		if len(call) >= 2 && call[0] == "config" && call[1] == "list" {
			usedList = true
		}
		if len(call) == 3 && call[0] == "config" && call[1] == "get" {
			gotKeys[call[2]]++
		}
	}
	if usedList {
		t.Fatalf("stub recorded a `config list` call: %v — config list prints aligned columns "+
			"for a human; ReadSettings must read each key with `config get` instead", calls)
	}
	if len(calls) != len(SettingKeys) {
		t.Fatalf("stub was invoked %d times, want exactly %d (one `config get` per key): %v",
			len(calls), len(SettingKeys), calls)
	}
	for _, key := range SettingKeys {
		if gotKeys[key] != 1 {
			t.Errorf("`config get %s` was called %d times, want exactly 1", key, gotKeys[key])
		}
	}
}

// TestReadSettingsTreatsUnsetAsEmpty covers the defensive half of unset
// handling, not the real contract: `config get` itself has never been
// observed to print the literal string `(unset)` — that's `config list`'s
// own substitution for its aligned columns (see ReadSettings' doc comment).
// This test exists in case `get` and `list` ever converge; the contract
// ReadSettings actually depends on today — an unset key answering with a
// bare empty line — is pinned separately by
// TestReadSettingsTrimsAnUnsetKeysEmptyLine below.
//
// It asserts the `(unset)` -> "" mapping alongside one key the stub answers
// with a real value: a ReadSettings that mapped every key to ""
// unconditionally (rather than only the ones answered `(unset)`) would
// still pass a test that checked only the unset keys, so this one would
// catch that too.
func TestReadSettingsTreatsUnsetAsEmpty(t *testing.T) {
	dir := t.TempDir()
	responses := map[string]string{}
	for _, key := range SettingKeys {
		responses[key] = "(unset)"
	}
	responses["translate.model"] = "gpt-4o-mini"

	cli, _ := writeArgvLoggingStub(t, dir, responses, 9, "unexpected config set call")

	got, err := ReadSettings(context.Background(), cli)
	if err != nil {
		t.Fatalf("ReadSettings() error = %v, want nil", err)
	}

	if got["translate.model"] != "gpt-4o-mini" {
		t.Errorf(`ReadSettings()["translate.model"] = %q, want "gpt-4o-mini" (a set value must not be touched)`,
			got["translate.model"])
	}
	for _, key := range SettingKeys {
		if key == "translate.model" {
			continue
		}
		if got[key] != "" {
			t.Errorf(`ReadSettings()[%q] = %q, want "" (the stub answered "(unset)")`, key, got[key])
		}
	}
}

// TestReadSettingsTrimsAnUnsetKeysEmptyLine pins the real contract, as
// confirmed against the actual built CLI with `od -c`: `config get` on an
// unset key prints nothing but a trailing newline, never the literal string
// `(unset)` — that substitution belongs to `config list` alone (see
// TestReadSettingsTreatsUnsetAsEmpty above for the defensive branch this
// test does not exercise). An empty line becomes "" once ReadSettings trims
// the trailing newline, with no substitution needed; mixing in one key with
// a real value keeps this from passing on an implementation that returns ""
// for everything unconditionally.
func TestReadSettingsTrimsAnUnsetKeysEmptyLine(t *testing.T) {
	dir := t.TempDir()
	responses := map[string]string{}
	for _, key := range SettingKeys {
		responses[key] = ""
	}
	responses["translate.model"] = "gpt-4o-mini"

	cli, _ := writeArgvLoggingStub(t, dir, responses, 9, "unexpected config set call")

	got, err := ReadSettings(context.Background(), cli)
	if err != nil {
		t.Fatalf("ReadSettings() error = %v, want nil", err)
	}

	if got["translate.model"] != "gpt-4o-mini" {
		t.Errorf(`ReadSettings()["translate.model"] = %q, want "gpt-4o-mini" (a set value must not be touched)`,
			got["translate.model"])
	}
	for _, key := range SettingKeys {
		if key == "translate.model" {
			continue
		}
		if got[key] != "" {
			t.Errorf(`ReadSettings()[%q] = %q, want "" (the stub answered a bare empty line, `+
				`the CLI's real form of unset)`, key, got[key])
		}
	}
}

// TestWriteSettingSurfacesTheCLIsRejection: the CLI validates values — a
// negative worker count, a whisper binary that is not executable. The
// window shows what it said instead of validating in parallel, so
// WriteSetting's error must carry the CLI's own message.
func TestWriteSettingSurfacesTheCLIsRejection(t *testing.T) {
	dir := t.TempDir()
	const rejectionMsg = `whisper.bin must be an executable file, got "/not/a/binary"`
	cli, log := writeArgvLoggingStub(t, dir, nil, 1, rejectionMsg)

	err := WriteSetting(context.Background(), cli, "whisper.bin", "/not/a/binary")
	if err == nil {
		t.Fatal("WriteSetting() error = nil, want the CLI's rejection")
	}
	if !strings.Contains(err.Error(), rejectionMsg) {
		t.Errorf("WriteSetting() error = %q, want it to contain the CLI's own message %q", err.Error(), rejectionMsg)
	}

	calls := readArgvLog(t, log)
	if len(calls) != 1 {
		t.Fatalf("stub was invoked %d times, want exactly 1: %v", len(calls), calls)
	}
	want := []string{"config", "set", "whisper.bin", "/not/a/binary"}
	if strings.Join(calls[0], "\x1f") != strings.Join(want, "\x1f") {
		t.Errorf("stub was invoked with %v, want %v", calls[0], want)
	}
}

// TestTheAPIKeyIsNeverLogged guards the one value here that must not leak:
// WriteSetting's error text for translate.api-key must not contain the
// value it was given, even though the call failed and produced an error to
// report. The stub's own rejection message never mentions the value
// either, so this is checking WriteSetting's own error construction, not
// the CLI's.
func TestTheAPIKeyIsNeverLogged(t *testing.T) {
	dir := t.TempDir()
	const secret = "sk-do-not-print-this-9f8e7d6c5b4a"
	cli, _ := writeArgvLoggingStub(t, dir, nil, 1, "translate.api-key was rejected")

	err := WriteSetting(context.Background(), cli, "translate.api-key", secret)
	if err == nil {
		t.Fatal("WriteSetting() error = nil, want a rejection to check the text of")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("WriteSetting() error = %q, contains the API key value it was given", err.Error())
	}
}
