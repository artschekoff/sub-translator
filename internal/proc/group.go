// Package proc keeps this program's children killable as a unit.
//
// A desktop wrapper cancels a run by sending SIGINT to the CLI alone. The
// signal never reaches ffmpeg or whisper-cli: they are reparented to PID 1 and
// keep working — a full ffmpeg pass over a feature film, continuing after the
// user pressed Cancel. Capturing their stderr means an orphan dies of EPIPE on
// its next write, but a child that never writes again is not reached by that at
// all. So each child is put in a process group of its own before it starts, and
// the signal handler kills those groups outright.
//
// Detach and KillGroup are per-platform (group_unix.go, group_windows.go); the
// registry they both feed is here, because there is nothing platform-specific
// about a slice.
package proc

import (
	"os/exec"
	"sync"
)

// The concurrency contract: started is appended to by whichever goroutine is
// launching a child and read by the signal handler's goroutine, which is
// always a different one, so every touch of the slice goes through mu.
//
// What mu cannot order is cmd.Process. os/exec sets that field inside Start,
// after Detach has handed the command back, and nothing here can synchronise
// with it. A child whose Start has not finished when the signal lands is
// therefore skipped. The window is the width of a fork and what it costs is the
// orphan this package exists to prevent — the behaviour without this package at
// all, never anything worse.
var (
	mu      sync.Mutex
	started []*exec.Cmd
)

// register records a detached command so KillAllGroups can find it later.
func register(cmd *exec.Cmd) {
	mu.Lock()
	defer mu.Unlock()
	started = append(started, cmd)
}

// KillAllGroups kills the process group of every child detached this run that
// is still running. It is what the signal handler calls, and it has to run
// before the scratch directory is removed: a surviving ffmpeg would go on
// writing into a directory being deleted underneath it.
func KillAllGroups() {
	mu.Lock()
	live := make([]*exec.Cmd, len(started))
	copy(live, started)
	mu.Unlock()

	for _, cmd := range live {
		// ProcessState is non-nil only once Wait has reaped the child. Skipping
		// those is more than tidiness: the kernel is free to hand a reaped pid
		// to somebody else, and killing a recycled process group would kill a
		// stranger.
		if cmd.Process == nil || cmd.ProcessState != nil {
			continue
		}
		_ = KillGroup(cmd)
	}
}
