//go:build windows

package proc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Detach gives cmd's child a console process group of its own and records it
// for KillAllGroups. The constant comes from the standard library's own
// syscall package on purpose: this module has no third-party dependencies, and
// golang.org/x/sys would be the first.
//
// It must be called before cmd.Start, which is when CreationFlags is read.
func Detach(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP
	register(cmd)
}

// KillGroup ends the child. Windows has no kill(-pgid): TerminateProcess, which
// os.Process.Kill calls, ends one process and leaves anything it spawned
// behind. ffmpeg and whisper-cli spawn nothing of their own, so this reaches
// the process that matters; the new process group still earns its keep by
// keeping a console Ctrl-C from reaching the child twice.
//
// A child that has already finished is the outcome being asked for, so that
// comes back as success.
func KillGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil {
		if errors.Is(err, os.ErrProcessDone) {
			return nil
		}
		return err
	}
	return nil
}
