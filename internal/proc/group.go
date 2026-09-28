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

// The concurrency contract, and the reason Start and Wait exist instead of a
// bare Detach the caller follows with its own cmd.Start and cmd.Wait:
//
// groups is appended to by whichever goroutine is launching a child and read by
// the signal handler's goroutine, which is always a different one. mu orders
// those, but a mutex around the slice would not be enough if the slice held
// *exec.Cmd: os/exec writes cmd.Process inside Start and cmd.ProcessState inside
// Wait, and a handler reading either would be a data race — undefined, not
// merely unlucky. So Start does the whole launch under mu and records the pid,
// an int copied while the lock is held, and Wait removes it again once the child
// has been reaped. The handler reads ints and nothing else.
//
// What that buys is the fact the handler actually needs. Membership means
// "started, and not reaped yet", which is the only thing that makes a pid safe
// to name: the kernel is free to hand a reaped pid to somebody else, and killing
// a recycled process group would kill a stranger. Membership is authoritative
// because every child this program starts is waited for on every path — see the
// call sites in internal/media and internal/whisper — so no entry can outlive
// its child.
//
// The one window left is the width of a function return: the kernel reaps the
// child inside cmd.Wait, and the entry goes a few instructions later, when Wait
// takes mu. Closing it would mean holding mu across cmd.Wait, which is the whole
// length of an ffmpeg pass — the handler would block behind it and cancel
// nothing. A pid registry that kept reaped entries for the length of a run, as
// this one did before, left that window open for hours instead.
var (
	mu     sync.Mutex
	groups []int
)

// Start puts cmd's child in a process group of its own, starts it, and records
// the group so KillAllGroups can find it. It is the only way this program starts
// a subprocess it may later have to kill, and it replaces cmd.Start at the call
// site.
//
// The group attributes have to be set before the fork and the pid only exists
// after it, which is why this is one call rather than two: holding mu across the
// whole launch is what keeps the registry free of half-started children.
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

// Wait waits for a child started by Start and drops it from the registry, and it
// replaces cmd.Wait at the call site for the same reason Start replaces
// cmd.Start: forgetting it would leave a reaped pid on the list of groups a
// cancel kills. It returns cmd.Wait's error unchanged.
//
// Call it from the goroutine that called Start, which is what os/exec requires
// of Start and Wait anyway; that program order is what makes reading
// cmd.Process here safe without the lock.
func Wait(cmd *exec.Cmd) error {
	err := cmd.Wait()
	if cmd.Process != nil {
		forget(cmd.Process.Pid)
	}
	return err
}

// forget drops one pid from the registry. The order within the slice is not
// meaningful, so the last entry fills the hole.
func forget(pid int) {
	mu.Lock()
	defer mu.Unlock()
	for i, p := range groups {
		if p == pid {
			groups[i] = groups[len(groups)-1]
			groups = groups[:len(groups)-1]
			return
		}
	}
}

// Running reports how many process groups a cancel would kill right now. It is
// zero whenever this program is between children, which is most of a run — and
// it is how a caller's own tests check that it waited for everything it started,
// since a child that is started but never waited for leaves a pid on this list
// for the rest of the run and the kernel is free to hand that pid to somebody
// else.
func Running() int {
	mu.Lock()
	defer mu.Unlock()
	return len(groups)
}

// KillAllGroups kills the process group of every child that is running right
// now. It is what the signal handler calls, and it has to run before the scratch
// directory is removed: a surviving ffmpeg would go on writing into a directory
// being deleted underneath it.
func KillAllGroups() {
	mu.Lock()
	live := make([]int, len(groups))
	copy(live, groups)
	mu.Unlock()

	for _, pid := range live {
		_ = KillGroup(pid)
	}
}
