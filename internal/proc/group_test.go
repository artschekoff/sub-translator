//go:build unix

package proc

import (
	"fmt"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// waitFor fails the test unless cmd finishes inside d. A kill that did not
// land shows up as this timeout rather than as a hung test binary.
func waitFor(t *testing.T, cmd *exec.Cmd, d time.Duration, what string) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(d):
		_ = cmd.Process.Kill()
		t.Fatalf("%s: the child outlived the kill by %s", what, d)
	}
}

// gone reports whether pid has left the process table, polling because a
// grandchild is reparented and reaped by init rather than by this test, which
// does not happen in the same instant as the kill.
func gone(pid int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestKillGroupKillsAChildThatIgnoresSignals is the case that matters: a child
// that never writes again cannot be reached by EPIPE, so only the group kill
// gets it.
func TestKillGroupKillsAChildThatIgnoresSignals(t *testing.T) {
	cmd := exec.Command("sh", "-c", "trap '' INT; sleep 30")
	Detach(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := KillGroup(cmd); err != nil {
		t.Fatalf("KillGroup: %v", err)
	}
	waitFor(t, cmd, 5*time.Second, "KillGroup")
}

// A group kill has to reach the whole group and not only its leader. Killing
// the leader alone would leave whatever it forked still running, which is the
// same bug one level down.
func TestKillGroupReachesAGrandchild(t *testing.T) {
	// The grandchild's pid is printed so the test can ask after it; the leader
	// then waits, so it is still alive when the group kill arrives.
	cmd := exec.Command("sh", "-c", "sleep 30 & echo $!; wait")
	Detach(cmd)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var grandchild int
	if _, err := fmt.Fscan(out, &grandchild); err != nil {
		t.Fatalf("reading the grandchild's pid: %v", err)
	}
	if err := KillGroup(cmd); err != nil {
		t.Fatalf("KillGroup: %v", err)
	}
	waitFor(t, cmd, 5*time.Second, "KillGroup")
	if !gone(grandchild, 5*time.Second) {
		t.Errorf("the grandchild (pid %d) survived the group kill", grandchild)
	}
}

// An already-exited child is the success case, not an error: the signal handler
// races every child's own exit, and a run whose ffmpeg finished a millisecond
// before the user pressed Cancel must not be reported as a failed kill.
func TestKillGroupOnAnAlreadyExitedChildIsNotAnError(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	Detach(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := KillGroup(cmd); err != nil {
		t.Errorf("KillGroup on a child that had already exited: %v, want nil", err)
	}
}

// KillAllGroups is all the signal handler has: it never sees the individual
// commands, only the registry Detach filled in. Every child detached this run
// has to be gone when it returns.
func TestKillAllGroupsKillsEveryDetachedChild(t *testing.T) {
	var cmds []*exec.Cmd
	for i := 0; i < 3; i++ {
		cmd := exec.Command("sh", "-c", "trap '' INT; sleep 30")
		Detach(cmd)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}

	KillAllGroups()

	for _, cmd := range cmds {
		waitFor(t, cmd, 5*time.Second, "KillAllGroups")
	}
}

// A command nobody detached is not in the registry and must survive
// KillAllGroups untouched: the registry is the whole of what this package
// claims authority over. A clean exit status is the assertion, because a killed
// child is still in the process table as a zombie until it is waited for.
func TestKillAllGroupsLeavesAnUndetachedChildAlone(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 0.3")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	KillAllGroups()

	if err := cmd.Wait(); err != nil {
		t.Errorf("a child that was never detached did not exit cleanly: %v", err)
	}
}
