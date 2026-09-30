package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bundledCLIPath returns where LocateCLI looks for a copy of the CLI
// bundled beside this test binary. It is a function, not a constant,
// because os.Executable() only resolves once the test process exists.
func bundledCLIPath(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return filepath.Join(filepath.Dir(self), "sub-translator")
}

// clearBundledCLI removes any leftover bundled binary from a previous test
// in this file, so every test starts from "no bundle" regardless of test
// order or a prior failure that skipped its own cleanup.
func clearBundledCLI(t *testing.T) {
	t.Helper()
	if err := os.Remove(bundledCLIPath(t)); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clearing leftover bundled CLI: %v", err)
	}
}

// writeFakeExecutable creates a file at path that isExecutableFile accepts:
// a regular file with at least one execute bit set. It is not a real
// sub-translator binary — LocateCLI only checks that something executable
// is sitting at the expected path, it never runs it.
func writeFakeExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("Chmod %s: %v", path, err)
	}
}

// emptyPathDir returns a fresh, empty directory suitable for use as PATH,
// so exec.LookPath("sub-translator") is guaranteed to fail rather than
// picking up whatever happens to be on the machine running the test.
func emptyPathDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

// freshHome points HOME (and so statePath, via os.UserHomeDir) at a new
// temp directory with no gui.json in it, so RememberedCLI starts from
// "nothing remembered" regardless of what the real user's machine has
// saved.
func freshHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

// TestLocateCLIPrefersTheBundledBinary: the DMG ships the CLI inside the
// .app, so a copy beside the GUI executable wins over whatever is on PATH
// or remembered — a user with an old sub-translator in /usr/local/bin must
// still get the one we shipped.
func TestLocateCLIPrefersTheBundledBinary(t *testing.T) {
	clearBundledCLI(t)
	bundled := bundledCLIPath(t)
	writeFakeExecutable(t, bundled)
	defer os.Remove(bundled)

	pathDir := emptyPathDir(t)
	onPath := filepath.Join(pathDir, "sub-translator")
	writeFakeExecutable(t, onPath)
	t.Setenv("PATH", pathDir)

	freshHome(t)
	remembered := filepath.Join(t.TempDir(), "sub-translator")
	writeFakeExecutable(t, remembered)
	if err := RememberCLI(remembered); err != nil {
		t.Fatalf("RememberCLI: %v", err)
	}

	got, err := LocateCLI()
	if err != nil {
		t.Fatalf("LocateCLI() error = %v", err)
	}
	if got != bundled {
		t.Fatalf("LocateCLI() = %q, want the bundled binary %q (PATH had %q, remembered had %q)",
			got, bundled, onPath, remembered)
	}
}

// TestLocateCLIFallsBackToPath covers the developer case: no bundle, one on
// PATH.
func TestLocateCLIFallsBackToPath(t *testing.T) {
	clearBundledCLI(t)

	pathDir := emptyPathDir(t)
	onPath := filepath.Join(pathDir, "sub-translator")
	writeFakeExecutable(t, onPath)
	t.Setenv("PATH", pathDir)

	freshHome(t)

	got, err := LocateCLI()
	if err != nil {
		t.Fatalf("LocateCLI() error = %v", err)
	}
	if got != onPath {
		t.Fatalf("LocateCLI() = %q, want the PATH binary %q", got, onPath)
	}
}

// TestLocateCLIUsesTheRememberedPathLast is the file picker's result.
func TestLocateCLIUsesTheRememberedPathLast(t *testing.T) {
	clearBundledCLI(t)
	t.Setenv("PATH", emptyPathDir(t))

	freshHome(t)
	remembered := filepath.Join(t.TempDir(), "sub-translator")
	writeFakeExecutable(t, remembered)
	if err := RememberCLI(remembered); err != nil {
		t.Fatalf("RememberCLI: %v", err)
	}

	got, err := LocateCLI()
	if err != nil {
		t.Fatalf("LocateCLI() error = %v", err)
	}
	if got != remembered {
		t.Fatalf("LocateCLI() = %q, want the remembered binary %q", got, remembered)
	}
}

// TestLocateCLIReportsNotFoundRatherThanGuessing: with none of the three,
// the window opens on Settings with a picker. A wrong guess here produces
// an error the user cannot act on.
func TestLocateCLIReportsNotFoundRatherThanGuessing(t *testing.T) {
	clearBundledCLI(t)
	t.Setenv("PATH", emptyPathDir(t))
	freshHome(t)

	_, err := LocateCLI()
	if !errors.Is(err, ErrCLINotFound) {
		t.Fatalf("LocateCLI() error = %v, want errors.Is(err, ErrCLINotFound)", err)
	}
}

// TestTheRememberedPathIsNotInTheSharedConfig is the bootstrap guard. The
// GUI reads the CLI's shared settings by RUNNING the CLI (`sub-translator
// config get ...`), so the CLI's own location cannot be stored there. This
// test asserts locate.go and guistate.go never spell out that subcommand:
// it scans both files' source for the quoted Go string literal `"config"`,
// the shape a regression would take if RememberedCLI or LocateCLI were
// rewritten to shell out to the CLI to find the CLI.
//
// It names locate.go and guistate.go explicitly rather than walking the
// package directory. This file, guistate_test.go and other test files in
// this package are expected to mention "config" in prose (as this comment
// just did, unquoted) while explaining what the guard forbids; a walk that
// swept up test files would either need to exempt itself — fragile — or
// flag its own explanation and never pass. Reading exactly the two files
// the constraint applies to avoids that trap entirely.
func TestTheRememberedPathIsNotInTheSharedConfig(t *testing.T) {
	const needle = `"config"`
	files := []string{"locate.go", "guistate.go"}

	scanned := 0
	for _, name := range files {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		scanned++
		if strings.Contains(string(body), needle) {
			t.Errorf("%s contains %s: the CLI's location must never be looked up "+
				"through its own config subcommand — it is this window's own state, "+
				"kept in gui.json (see guistate.go)", name, needle)
		}
	}

	// A scan that found no files would pass silently, which is worse than no
	// guard at all.
	if scanned != len(files) {
		t.Fatalf("scanned %d of %d files, want %d — the guard must not pass silently on nothing scanned", scanned, len(files), len(files))
	}
}
