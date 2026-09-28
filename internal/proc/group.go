// Package proc keeps this program's children killable as a unit.
//
// A desktop wrapper cancels a run by sending SIGINT to the CLI alone. The
// signal never reaches ffmpeg or whisper-cli: they are reparented to PID 1 and
// keep working — a full ffmpeg pass over a feature film, continuing after the
// user pressed Cancel. Capturing their stderr means an orphan dies of EPIPE on
// its next write, but a child that never writes again is not reached by that at
// all. So each child is started in a process group of its own, and the signal
// handler kills those groups outright.
//
// Start and KillAllGroups are here; the two operations that have no
// cross-platform spelling — putting a child in its own group, and killing that
// group — are in group_unix.go and group_windows.go.
package proc

import (
	"os/exec"
	"sync"
)

// The concurrency contract, and the reason Start exists instead of a bare
// Detach the caller follows with its own cmd.Start:
//
// groups is appended to by whichever goroutine is launching a child and read by
// the signal handler's goroutine, which is always a different one. mu orders
// those, but a mutex around the slice would not be enough if the slice held
// *exec.Cmd: os/exec writes cmd.Process inside Start and cmd.ProcessState
// inside Wait, and a handler reading either would be a data race — undefined,
// not merely unlucky. So Start does the whole launch under mu and records the
// pid, an int copied while the lock is held. The handler reads ints and nothing
// else, and no child can be half-started as far as the registry is concerned:
// either it is in and killable, or Start has not returned and there is no
// process yet.
var (
	mu     sync.Mutex
	groups []int
)

// Start puts cmd's child in a process group of its own, starts it, and records
// the group so KillAllGroups can find it. It is the only way this program
// starts a subprocess it may later have to kill, and it replaces cmd.Start at
// the call site.
//
// The group attributes have to be set before the fork and the pid only exists
// after it, which is why this is one call rather than two: holding mu across
// the whole launch is what keeps the registry free of half-started children.
func Start(cmd *exec.Cmd) error {
	mu.Lock()
	defer mu.Unlock()

	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	groups = append(groups, cmd.Process.Pid)
	return nil
}

// KillAllGroups kills the process group of every child started this run. It is
// what the signal handler calls, and it has to run before the scratch directory
// is removed: a surviving ffmpeg would go on writing into a directory being
// deleted underneath it.
func KillAllGroups() {
	mu.Lock()
	live := make([]int, len(groups))
	copy(live, groups)
	mu.Unlock()

	for _, pid := range live {
		_ = KillGroup(pid)
	}
}
