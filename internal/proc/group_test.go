//go:build unix

package proc

import (
	"fmt"
	"os/exec"
	"sync"
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
	if err := Start(cmd); err != nil {
		t.Fatal(err)
	}
	if err := KillGroup(cmd.Process.Pid); err != nil {
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
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := Start(cmd); err != nil {
		t.Fatal(err)
	}
	var grandchild int
	if _, err := fmt.Fscan(out, &grandchild); err != nil {
		t.Fatalf("reading the grandchild's pid: %v", err)
	}
	if err := KillGroup(cmd.Process.Pid); err != nil {
		t.Fatalf("KillGroup: %v", err)
	}
	waitFor(t, cmd, 5*time.Second, "KillGroup")
	if !gone(grandchild, 5*time.Second) {
		t.Errorf("the grandchild (pid %d) survived the group kill", grandchild)
	}
}

// An already-exited child is the success case, not an error: the signal handler
// races every child's own exit, and a run whose ffmpeg finished a millisecond
// before the user pressed Cancel must not be reported as a failed kill. It also
// must not be killed a second time — the registry holds nothing but pids, and a
// reaped pid can belong to somebody else by now.
func TestKillGroupOnAnAlreadyExitedChildIsNotAnError(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := Start(cmd); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := KillGroup(pid); err != nil {
		t.Errorf("KillGroup on a child that had already exited: %v, want nil", err)
	}
}

// A pid that is alive but leads no group of its own is not this package's to
// kill: -pid would name whatever group the process merely belongs to, which in
// a terminal is the user's own foreground job. The test binary itself is such a
// process, and killing its group would take the test run with it.
func TestKillGroupRefusesAPidThatLeadsNoGroup(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 0.3")
	// Deliberately not Start: this child inherits the test binary's group.
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := KillGroup(cmd.Process.Pid); err != nil {
		t.Errorf("KillGroup on a non-leader: %v, want nil", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Errorf("a child that leads no group did not exit cleanly: %v", err)
	}
}

// KillAllGroups is all the signal handler has: it never sees the individual
// commands, only the registry Start filled in. Every child started this run has
// to be gone when it returns.
func TestKillAllGroupsKillsEveryStartedChild(t *testing.T) {
	var cmds []*exec.Cmd
	for i := 0; i < 3; i++ {
		cmd := exec.Command("sh", "-c", "trap '' INT; sleep 30")
		if err := Start(cmd); err != nil {
			t.Fatal(err)
		}
		cmds = append(cmds, cmd)
	}

	KillAllGroups()

	for _, cmd := range cmds {
		waitFor(t, cmd, 5*time.Second, "KillAllGroups")
	}
}

// A command nobody started through this package is not in the registry and must
// survive KillAllGroups untouched: the registry is the whole of what this
// package claims authority over. A clean exit status is the assertion, because a
// killed child is still in the process table as a zombie until it is waited for.
func TestKillAllGroupsLeavesAnUnregisteredChildAlone(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 0.3")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	KillAllGroups()

	if err := cmd.Wait(); err != nil {
		t.Errorf("a child that was never started through proc did not exit cleanly: %v", err)
	}
}

// The signal handler runs on its own goroutine and can call KillAllGroups at any
// instant, including while another goroutine is starting a child. This is the
// only test that drives that concurrency in-process, which is the only place the
// race detector can see it: the CLI-level cancellation test drives a subprocess,
// where -race is not looking. An earlier registry of *exec.Cmd passed every
// other test in this file and failed here, because reading cmd.Process and
// cmd.ProcessState from the handler races os/exec's own writes in Start and
// Wait.
//
// Run it with -race or it proves very little.
func TestKillAllGroupsRacesStartAndWait(t *testing.T) {
	stop := make(chan struct{})
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			KillAllGroups()
		}
	}()

	for i := 0; i < 20; i++ {
		cmd := exec.Command("sh", "-c", "exit 0")
		if err := Start(cmd); err != nil {
			t.Fatal(err)
		}
		// The kill and this Wait are deliberately unordered: whichever gets
		// there first, neither may touch memory the other is writing.
		_ = cmd.Wait()
	}

	close(stop)
	wg.Wait()
}
