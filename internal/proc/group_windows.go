//go:build windows

package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// detach asks for a console process group of its own for the child cmd is about
// to create. The constant comes from the standard library's own syscall package
// on purpose: this module has no third-party dependencies, and golang.org/x/sys
// would be the first. CreationFlags is ORed rather than assigned so a caller's
// own flags survive.
//
// It has to happen before the process is created, which is when CreationFlags
// is read.
func detach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
}

// KillGroup ends the child. Windows has no kill(-pgid): TerminateProcess, which
// os.Process.Kill calls, ends one process and leaves anything it spawned
// behind. ffmpeg and whisper-cli spawn nothing of their own, so this reaches the
// process that matters; the new process group still earns its keep by keeping a
// console Ctrl-C from reaching the child twice.
//
// A child that has already finished is the outcome being asked for, so that
// comes back as success.
func KillGroup(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		// Nothing by that pid: already gone.
		return nil
	}
	if err := p.Kill(); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	return nil
}
