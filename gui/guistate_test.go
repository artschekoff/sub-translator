package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRememberedCLIReturnsEmptyWhenNothingSaved covers a machine that has
// never opened the file picker: no gui.json exists yet, and that is not an
// error condition.
func TestRememberedCLIReturnsEmptyWhenNothingSaved(t *testing.T) {
	freshHome(t)

	if got := RememberedCLI(); got != "" {
		t.Fatalf("RememberedCLI() = %q, want \"\" with no state file saved", got)
	}
}

// TestRememberCLIRoundTrips is the basic contract: what RememberCLI saves,
// RememberedCLI reads back, even across what looks like separate process
// launches (a fresh read from disk, not a cached value).
func TestRememberCLIRoundTrips(t *testing.T) {
	freshHome(t)

	want := filepath.Join(t.TempDir(), "sub-translator")
	if err := RememberCLI(want); err != nil {
		t.Fatalf("RememberCLI: %v", err)
	}

	if got := RememberedCLI(); got != want {
		t.Fatalf("RememberedCLI() = %q, want %q", got, want)
	}

	// A second save must overwrite, not append or merge — there is exactly
	// one remembered CLI at a time.
	want2 := filepath.Join(t.TempDir(), "other-sub-translator")
	if err := RememberCLI(want2); err != nil {
		t.Fatalf("RememberCLI (second save): %v", err)
	}
	if got := RememberedCLI(); got != want2 {
		t.Fatalf("RememberedCLI() after second save = %q, want %q", got, want2)
	}
}

// TestRememberCLICreatesTheStateDirectory covers a first launch, where
// ~/Library/Application Support/sub-translator doesn't exist yet.
func TestRememberCLICreatesTheStateDirectory(t *testing.T) {
	freshHome(t)

	p, err := statePath()
	if err != nil {
		t.Fatalf("statePath: %v", err)
	}
	dir := filepath.Dir(p)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("state directory %s already exists before RememberCLI ran", dir)
	}

	if err := RememberCLI("/usr/local/bin/sub-translator"); err != nil {
		t.Fatalf("RememberCLI: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("state directory was not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s exists but is not a directory", dir)
	}
}

// TestRememberCLIWritesAtomicallyAndLeavesNoTempFile guards the crash-safety
// requirement: RememberCLI must stage its write with os.CreateTemp and move
// it into place with os.Rename, the way the CLI writes its own files (see
// internal/srt.Write), rather than truncating gui.json in place. A reader
// that inspects the state directory right after a save should see exactly
// one file — the finished gui.json — never a stray .tmp left behind by a
// staged write that forgot to clean up, and the finished file should carry
// the permissions an atomically-renamed file gets (0644, not something more
// permissive inherited from a temp file default).
func TestRememberCLIWritesAtomicallyAndLeavesNoTempFile(t *testing.T) {
	freshHome(t)

	if err := RememberCLI("/opt/sub-translator/sub-translator"); err != nil {
		t.Fatalf("RememberCLI: %v", err)
	}

	p, err := statePath()
	if err != nil {
		t.Fatalf("statePath: %v", err)
	}
	dir := filepath.Dir(p)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir %s: %v", dir, err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("state directory has %d entries after RememberCLI, want exactly gui.json: %v", len(entries), names)
	}
	if entries[0].Name() != filepath.Base(p) {
		t.Fatalf("state directory contains %q, want %q — a temp file was left behind", entries[0].Name(), filepath.Base(p))
	}

	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("Stat %s: %v", p, err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Fatalf("gui.json has mode %o, want 0644", perm)
	}
}

// TestRememberCLIFailureBeforeRenameLeavesPreviousStateIntact is the other
// half of the atomicity contract, mirroring
// internal/srt.TestWriteFailureBeforeRenameLeavesPreviousFileIntact: making
// the state directory read-only blocks os.CreateTemp from creating a new
// directory entry, but would NOT block a write that truncates the existing
// gui.json in place, since that only needs write permission on the file
// itself, not the directory. So this is also the test that would catch a
// regression to a direct os.WriteFile(p, ...) — a change that could still
// pass TestRememberCLIWritesAtomicallyAndLeavesNoTempFile above, since a
// direct write also leaves no stray temp file behind.
func TestRememberCLIFailureBeforeRenameLeavesPreviousStateIntact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root ignores directory permission bits")
	}

	freshHome(t)

	const original = "/usr/local/bin/sub-translator"
	if err := RememberCLI(original); err != nil {
		t.Fatalf("first RememberCLI: %v", err)
	}

	p, err := statePath()
	if err != nil {
		t.Fatalf("statePath: %v", err)
	}
	dir := filepath.Dir(p)

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	originalMode := di.Mode().Perm()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	t.Cleanup(func() { os.Chmod(dir, originalMode) })

	if err := RememberCLI("/opt/sub-translator/sub-translator"); err == nil {
		t.Fatal("want an error when the state directory is not writable")
	}

	if got := RememberedCLI(); got != original {
		t.Fatalf("RememberedCLI() after a failed write = %q, want the untouched previous value %q", got, original)
	}
}

// TestRememberedCLIReturnsEmptyOnCorruptState covers a gui.json damaged by
// something outside this program's control (a disk error, a hand edit gone
// wrong). RememberedCLI treats that the same as "nothing remembered" rather
// than panicking or propagating a parse error the caller has no good way to
// surface mid-launch.
func TestRememberedCLIReturnsEmptyOnCorruptState(t *testing.T) {
	freshHome(t)

	p, err := statePath()
	if err != nil {
		t.Fatalf("statePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(p, []byte("{not valid json"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if got := RememberedCLI(); got != "" {
		t.Fatalf("RememberedCLI() = %q, want \"\" for a corrupt state file", got)
	}
}
