//go:build !windows

package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

func TestBrowserProcessGroupHelper(t *testing.T) {
	if os.Getenv("NYALA_TEST_PROCESS_GROUP") == "" {
		return
	}
	if os.Getenv("NYALA_TEST_PROCESS_GROUP") == "ignore-term" {
		signal.Ignore(syscall.SIGTERM)
	}
	if _, err := os.Stdout.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
}

// browserGroupProcess starts direct children in one group so tests own and reap
// every member, even when the leader exits before the other members.
func browserGroupProcess(t *testing.T, group int, ignoreTerm bool, profile string) (*exec.Cmd, <-chan error) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestBrowserProcessGroupHelper$", "--", "--user-data-dir="+profile)
	mode := "terminate"
	if ignoreTerm {
		mode = "ignore-term"
	}
	cmd.Env = append(os.Environ(), "NYALA_TEST_PROCESS_GROUP="+mode)
	if group >= 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: group}
	}
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close() })
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make([]byte, 1)
	_, readyErr := io.ReadFull(output, ready)
	done := make(chan error, 1)
	reaped := make(chan struct{})
	go func() {
		done <- cmd.Wait()
		close(reaped)
	}()
	t.Cleanup(func() {
		cancel()
		<-reaped
	})
	if readyErr != nil {
		t.Fatalf("wait for group member: %v", readyErr)
	}
	return cmd, done
}

func waitBrowserGroupProcess(t *testing.T, done <-chan error, want syscall.Signal) {
	t.Helper()
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("group member exit = %v, want signal %v", err, want)
		}
		status, ok := exitErr.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != want {
			t.Fatalf("group member exit = %v, want signal %v", err, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("group member did not exit")
	}
}

func TestBrowserSignalTarget(t *testing.T) {
	if got := browserSignalTarget(os.Getpid()); got != os.Getpid() {
		t.Fatalf("caller target = %d, want PID %d", got, os.Getpid())
	}
	inherited, _ := browserGroupProcess(t, -1, false, "")
	if got := browserSignalTarget(inherited.Process.Pid); got != inherited.Process.Pid {
		t.Fatalf("inherited group target = %d, want PID %d", got, inherited.Process.Pid)
	}
	leader, _ := browserGroupProcess(t, 0, false, "")
	pid := leader.Process.Pid
	if got := browserSignalTarget(pid); got != -pid {
		t.Fatalf("private group target = %d, want %d", got, -pid)
	}
	member, _ := browserGroupProcess(t, pid, false, "")
	if got := browserSignalTarget(member.Process.Pid); got != member.Process.Pid {
		t.Fatalf("non-leader target = %d, want PID %d", got, member.Process.Pid)
	}
}

func TestTerminateBrowserProcessGroup(t *testing.T) {
	leader, leaderDone := browserGroupProcess(t, 0, false, "")
	_, memberDone := browserGroupProcess(t, leader.Process.Pid, false, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := terminateBrowser(ctx, leader.Process.Pid); err != nil {
		t.Fatal(err)
	}
	waitBrowserGroupProcess(t, leaderDone, syscall.SIGTERM)
	waitBrowserGroupProcess(t, memberDone, syscall.SIGTERM)
}

func TestTerminateBrowserProcessGroupOutlivesLeader(t *testing.T) {
	leader, leaderDone := browserGroupProcess(t, 0, false, "")
	pid := leader.Process.Pid
	member, memberDone := browserGroupProcess(t, pid, true, "")
	signalTarget := browserSignalTarget(pid)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	if err := terminateBrowser(ctx, pid); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("terminate group = %v, want deadline while member lives", err)
	}
	waitBrowserGroupProcess(t, leaderDone, syscall.SIGTERM)
	if processAlive(pid) {
		t.Fatal("processAlive reported the departed leader as alive")
	}
	if !processAlive(member.Process.Pid) || processGone(signalTarget) {
		t.Fatal("group disappeared while its member survived")
	}
	killProcess(signalTarget)
	waitBrowserGroupProcess(t, memberDone, syscall.SIGKILL)
	if !processGone(signalTarget) {
		t.Fatal("group survived after every member was reaped")
	}
}

func TestStopBrowserProcessGroupEscalation(t *testing.T) {
	profile := t.TempDir()
	leader, leaderDone := browserGroupProcess(t, 0, false, profile)
	_, memberDone := browserGroupProcess(t, leader.Process.Pid, true, profile)
	stopBrowser(leader.Process.Pid, profile)
	waitBrowserGroupProcess(t, leaderDone, syscall.SIGTERM)
	waitBrowserGroupProcess(t, memberDone, syscall.SIGKILL)
}

func TestStopBrowserProcessGroupUnowned(t *testing.T) {
	profile := t.TempDir()
	leader, _ := browserGroupProcess(t, 0, false, profile)
	member, _ := browserGroupProcess(t, leader.Process.Pid, false, profile)
	stopBrowser(leader.Process.Pid, t.TempDir())
	if !processAlive(leader.Process.Pid) || !processAlive(member.Process.Pid) {
		t.Fatal("stopBrowser signalled a group that does not own the profile")
	}
}

func TestTerminateBrowserInheritedGroup(t *testing.T) {
	process, done := browserGroupProcess(t, -1, false, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := terminateBrowser(ctx, process.Process.Pid); err != nil {
		t.Fatal(err)
	}
	waitBrowserGroupProcess(t, done, syscall.SIGTERM)
}
