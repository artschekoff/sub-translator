// settings.go reads and writes the shared configuration the CLI already
// owns, through `sub-translator config` and only through it. The spec
// forbids the window holding a second copy of the CLI's validation rules —
// the same principle by which it reads the fast-mode rule off the probe
// event (see runner.go) instead of reimplementing it — so every setting
// here is read with `config get <key>` and written with `config set <key>
// <value>`.
//
// `config list` is deliberately never used to read settings: it prints
// aligned columns for a human (`  translate.model        (unset)`, the
// actual spacing `cmd_config.go`'s `%-22s` produces), a prose layout nobody
// promised to keep stable and whose alignment shifts as key names grow.
// Parsing it would turn that prose format into this window's API. `config
// get <key>` is the stable, one-value-per-call interface instead — one
// exec.CommandContext per key.
package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// SettingKeys are the config keys the spec's Settings screen shows, in the
// order it shows them.
//
// The CLI's own key set (`sub-translator config list`) has three more:
// whisper.language, whisper.threads and whisper.vad-threshold. The spec's
// Settings screen does not surface them, so they are deliberately left out
// here rather than added as controls nobody asked for.
var SettingKeys = []string{
	"translate.api-key", "translate.model", "translate.workers",
	"whisper.bin", "whisper.model", "whisper.vad-model",
	"whisper.chunk-minutes", "models.dir",
}

// ReadSettings reads every key in SettingKeys with its own `config get
// <key>` call — never `config list` (see the package doc comment for why).
// Each call's stdout, with its trailing newline trimmed, is the value.
//
// An unset key's real contract, from `config get`, is an empty line — that
// trims to "" on its own, no substitution needed. `(unset)` is what `config
// list` prints instead, for its aligned human-readable columns; `config
// get` has never been observed to print it. The `value == "(unset)"`
// mapping below is defensive, not load-bearing: it costs nothing today and
// only matters if `get`'s output ever converges with `list`'s, so a plain
// alignment-column string can't end up mistaken for someone's literal
// model name or file path.
func ReadSettings(ctx context.Context, cli string) (map[string]string, error) {
	settings := make(map[string]string, len(SettingKeys))
	for _, key := range SettingKeys {
		cmd := exec.CommandContext(ctx, cli, "config", "get", key)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("settings: reading %s: %s", key, rejection(err, stderr.Bytes()))
		}

		value := strings.TrimSuffix(stdout.String(), "\n")
		if value == "(unset)" {
			value = ""
		}
		settings[key] = value
	}
	return settings, nil
}

// WriteSetting writes one key with `config set <key> <value>` and surfaces
// the CLI's own rejection verbatim on failure. The CLI validates some of
// these keys — translate.workers, for one, rejects anything but a positive
// integer: run against the real built binary, `sub-translator config set
// translate.workers -1` answers `error: translate.workers must be a
// positive integer, got "-1"` — and this window shows what it said instead
// of validating the value itself in parallel.
//
// Not every key here validates: whisper.bin, whisper.model,
// whisper.vad-model and models.dir only run expandPath on the value
// (internal/config/config.go), and translate.api-key only trims it —
// neither checks the path exists, is a file, or is executable. WriteSetting
// makes no distinction; it surfaces whatever the CLI said, or nothing, for
// every key alike.
//
// The returned error is built only from the CLI's stderr, or the process
// error's own message when the CLI never got to say anything — never from
// value, and never by formatting the *exec.Cmd or its Args directly. See
// rejection for why that distinction matters: it is the one thing here that
// must not leak translate.api-key back out through an error string.
func WriteSetting(ctx context.Context, cli, key, value string) error {
	cmd := exec.CommandContext(ctx, cli, "config", "set", key, value)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("settings: writing %s: %s", key, rejection(err, stderr.Bytes()))
	}
	return nil
}

// rejection turns a failed `config` call into the text an error should
// carry: the CLI's own stderr when it said something, or the process
// error's own message otherwise (an *exec.ExitError's Error() is just "exit
// status N"; an *exec.Error from a missing or non-executable binary names
// only the binary's path). Both of those are safe to surface as-is.
//
// What rejection deliberately never does is format err's underlying
// *exec.Cmd, or that Cmd's Args, into the message — cmd.String() and a bare
// "%v" on a *exec.Cmd both render the full argv, which for
// WriteSetting("translate.api-key", ...) would print the key's value right
// back out into an error a caller might log or display. Building the
// message only from stderr/err.Error(), as both callers above do, avoids
// that path entirely rather than needing every caller to remember to strip
// it.
func rejection(err error, stderr []byte) string {
	if msg := strings.TrimSpace(string(stderr)); msg != "" {
		return msg
	}
	return err.Error()
}
