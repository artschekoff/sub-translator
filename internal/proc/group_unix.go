//go:build unix

package proc

import (
	"errors"
	"os/exec"
	"syscall"
)

// Detach puts cmd's child in a process group of its own and records it for
// KillAllGroups. It must be called before cmd.Start: SysProcAttr is read while
// the process is being forked, and setting it afterwards does nothing at all.
func Detach(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// Setpgid with no Pgid makes the child the leader of a brand-new group, so
	// the group id is the child's own pid.
	cmd.SysProcAttr.Setpgid = true
	register(cmd)
}

// KillGroup kills every process in cmd's group — the child and anything it
// spawned — rather than the child alone, which is the whole point of detaching
// it. A group that is already gone is the outcome being asked for, not a
// failure, so ESRCH comes back as success.
func KillGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// A negative pid is kill(2)'s way of naming a process group.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	return nil
}
