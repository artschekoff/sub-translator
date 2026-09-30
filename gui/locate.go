// Command gui — locate.go finds the sub-translator CLI binary this window
// drives as a subprocess.
package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

// ErrCLINotFound is returned by LocateCLI when none of its three lookup
// strategies produced a usable binary. The window responds by opening on
// Settings with a file picker rather than guessing further — a wrong guess
// here would produce an error the user cannot act on.
var ErrCLINotFound = errors.New("sub-translator not found")

// LocateCLI finds the sub-translator binary, in the order the spec fixes:
// a copy beside this executable (how the DMG ships it), then $PATH, then a
// path the user picked earlier through RememberCLI.
//
// The remembered path deliberately does not live in the CLI's own settings
// file: the GUI reads that file by running the CLI (see cmd_config.go at
// the repository root), so storing the CLI's location there would mean
// needing the CLI to find the CLI. It is the one piece of state this window
// owns — see guistate.go, which keeps it in a file that belongs to the GUI
// alone.
func LocateCLI() (string, error) {
	if self, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(self), "sub-translator")
		if isExecutableFile(candidate) {
			return candidate, nil
		}
	}

	if p, err := exec.LookPath("sub-translator"); err == nil {
		return p, nil
	}

	if p := RememberedCLI(); p != "" && isExecutableFile(p) {
		return p, nil
	}

	return "", ErrCLINotFound
}

// isExecutableFile reports whether path exists, is a regular file, and has
// at least one execute bit set. A directory or a file with no execute bit
// is not a candidate, even if something happens to be sitting at that path.
func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode()&0o111 != 0
}
