//go:build unix

package proc

import (
	"errors"
	"os/exec"
	"syscall"
)

// detach asks for a process group of its own for the child cmd is about to
// fork. Setpgid with no Pgid makes the child the leader of a brand-new group,
// so the group id is the child's own pid — which is what Start records and
// KillGroup kills.
//
// It has to happen before the fork: SysProcAttr is read while the process is
// being created, and setting it afterwards does nothing at all.
func detach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// KillGroup kills every process in the group pid leads — the child and anything
// it spawned — rather than the child alone, which is the whole point of
// detaching it. A group that is already gone is the outcome being asked for,
// not a failure.
func KillGroup(pid int) error {
	// A negative pid is kill(2)'s way of naming a process group. pid is a group
	// leader because Start made it one, and it has not been reaped because Wait
	// removes a reaped child from the registry before KillAllGroups can read it
	// — the registry is what makes -pid safe to name here.
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	return nil
}
