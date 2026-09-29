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
	go func() { done <- Wait(cmd) }()
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
	// cmd.Wait directly, not proc.Wait, so the registry is not told this pid
	// was reaped — deliberately, since the whole point of this test is to call
	// KillGroup on a pid the registry still believes is live. That leaves a
	// stale entry behind, and this package's tests share one process-lifetime
	// registry with no reset between cases: left alone, a later
	// KillAllGroups — in this test binary, KillAllGroups is exercised by
	// several others — would send an unguarded kill(-pid) at a pid the kernel
	// is free to have handed to somebody else by then. So this test cleans up
	// its own deliberately-stale entry rather than leaving it for the next one
	// to trip over.
	t.Cleanup(func() { forget(pid) })
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := KillGroup(pid); err != nil {
		t.Errorf("KillGroup on a child that had already exited: %v, want nil", err)
	}
}

// registered reports whether pid is one of the groups a cancel would kill right
// now. The registry is the whole of KillAllGroups' target list, so this is the
// direct question — in-package on purpose, because the answer is not otherwise
// observable without provoking pid reuse.
func registered(pid int) bool {
	mu.Lock()
	defer mu.Unlock()
	for _, p := range groups {
		if p == pid {
			return true
		}
	}
	return false
}

// The registry's whole claim is that membership means "started and not reaped
// yet". A pid that stayed on the list after its child was reaped would be a pid
// the kernel is free to hand to somebody else — and the next cancel would
// SIGKILL that stranger's process group, not ours. Nothing else in this package
// re-checks that, so Wait dropping the entry is the claim.
func TestWaitDropsTheReapedChildFromTheRegistry(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := Start(cmd); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if !registered(pid) {
		t.Fatalf("Start did not register pid %d", pid)
	}
	if err := Wait(cmd); err != nil {
		t.Fatal(err)
	}
	if registered(pid) {
		t.Errorf("pid %d is still on the kill list after Wait reaped it: a cancel "+
			"would name a process group the kernel has been free to reuse since", pid)
	}
}

// The same claim made about the walk rather than the slice: once every child has
// been waited for, a cancel has nothing left to kill. This is the state a run
// spends most of its life in — between the extraction and the transcription, or
// between two -fast chunks.
func TestKillAllGroupsTargetsNothingOnceEveryChildIsWaitedFor(t *testing.T) {
	var pids []int
	for i := 0; i < 3; i++ {
		cmd := exec.Command("sh", "-c", "exit 0")
		if err := Start(cmd); err != nil {
			t.Fatal(err)
		}
		pids = append(pids, cmd.Process.Pid)
		if err := Wait(cmd); err != nil {
			t.Fatal(err)
		}
	}
	for _, pid := range pids {
		if registered(pid) {
			t.Errorf("reaped pid %d is still on the kill list", pid)
		}
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
		// there first, neither may touch memory the other is writing, and the
		// unregister inside Wait runs against the handler's read of the same
		// registry.
		_ = Wait(cmd)
	}

	close(stop)
	wg.Wait()
}

// Start has to leave every child leading a group of its own, whatever the caller
// did to cmd.SysProcAttr first. Setpgid with a non-zero Pgid makes the child
// *join* that group instead: the pid Start then records is not a group leader, so
// KillGroup's kill(-pid) either finds nothing — tolerated as ESRCH, meaning the
// child is never killed — or SIGKILLs whatever unrelated group happens to own
// that id.
//
// The Pgid planted here is this test binary's own, which is the nastiest version
// of the mistake and the cheapest to arrange: a real group, in the same session,
// that the child is allowed to join, and one whose destruction would take the
// test runner with it. Nothing kills it here — the assertion is that the child
// never joined it.
func TestStartLeavesTheChildLeadingItsOwnGroupEvenIfAPgidWasSet(t *testing.T) {
	ours, err := syscall.Getpgid(syscall.Getpid())
	if err != nil {
		t.Fatalf("Getpgid: %v", err)
	}

	cmd := exec.Command("sh", "-c", "sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Pgid: ours}
	if err := Start(cmd); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = KillGroup(pid)
		_ = Wait(cmd)
	})

	group, err := syscall.Getpgid(pid)
	if err != nil {
		t.Fatalf("Getpgid(child): %v", err)
	}
	if group != pid {
		what := fmt.Sprintf("group %d", group)
		if group == ours {
			what = fmt.Sprintf("this test binary's own group (%d)", ours)
		}
		t.Errorf("the child leads %s, want its own group (%d): KillGroup would aim "+
			"kill(-%d) at a group this child does not lead", what, pid, pid)
	}
}
