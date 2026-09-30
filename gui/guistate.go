package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// guiState is the one piece of state this window owns between launches:
// where the user last pointed us at a sub-translator binary. It is kept out
// of the CLI's own settings file on purpose — see the package doc in
// locate.go for why that would be circular.
type guiState struct {
	CLI string `json:"cli,omitempty"`
}

// statePath returns the location of this window's own state file. It is
// deliberately a file the GUI reads and writes directly with encoding/json,
// not one it goes through the CLI to reach, because the CLI's location is
// exactly the thing being looked up.
//
// Platform policy: this path is macOS's, on every platform, with no GOOS
// branch — and that is a decision rather than an oversight. The spec names
// ~/Library/Application Support/sub-translator/gui.json outright, `make gui`
// and `make dmg` are macOS-only targets, and the DMG is the only way the CLI
// ships beside the GUI executable, so macOS is the only platform this window
// is delivered on today. Task 7's window options name Windows fields too
// (Wails puts the translucency flags on a per-platform struct and there is no
// top-level place to set them), which makes the question live but does not
// make the app shipped there.
//
// What it means concretely on Windows: os.UserHomeDir returns %USERPROFILE%
// and filepath.Join builds C:\Users\name\Library\Application
// Support\sub-translator\gui.json. That is a valid, writable, round-trippable
// path — RememberCLI's MkdirAll creates it and RememberedCLI reads it back —
// so nothing breaks; it simply ignores the platform's convention, which is
// %AppData%. On Linux it likewise ignores $XDG_CONFIG_HOME. Whoever ships
// this window on a second platform should branch here first; until then a
// GOOS branch would be untested code for a platform with no build target.
func statePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "sub-translator", "gui.json"), nil
}

// RememberCLI persists path as the CLI location for future launches.
//
// It writes atomically, the way the CLI writes its own files (see
// internal/srt.Write and internal/models's downloader): the new content is
// staged with os.CreateTemp in the target directory and moved into place
// with os.Rename, which POSIX guarantees is atomic. That way a crash or a
// full disk mid-write leaves either the previous complete gui.json or the
// next one, never a half-written file that would send the next launch to
// the picker for no reason.
func RememberCLI(path string) error {
	p, err := statePath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	data, err := json.Marshal(guiState{CLI: path})
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dir, ".gui-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// Whatever happens below, a stray temp file must not survive this call.
	// Remove is a no-op once Rename has already moved the file away.
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, p)
}

// RememberedCLI returns the CLI path RememberCLI last saved, or "" if
// nothing was ever saved, or the state file is missing, unreadable, or not
// valid JSON. Callers treat "" as "nothing remembered" rather than an
// error: a missing gui.json is simply a machine that has never needed the
// picker, not a fault.
func RememberedCLI() string {
	p, err := statePath()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	var s guiState
	if err := json.Unmarshal(data, &s); err != nil {
		return ""
	}
	return s.CLI
}
