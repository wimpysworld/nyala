//go:build !windows

package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// detachedSysProcAttr starts the browser in a new session.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// signalTerminate sends SIGTERM to pid. gone is true when ESRCH shows the
// process has already exited.
func signalTerminate(pid int) (gone bool, err error) {
	err = unix.Kill(pid, unix.SIGTERM)
	if errors.Is(err, unix.ESRCH) {
		return true, nil
	}
	return false, err
}

// processGone reports whether a null signal to pid fails with ESRCH.
func processGone(pid int) bool {
	return errors.Is(unix.Kill(pid, 0), unix.ESRCH)
}

// processAlive reports whether a null signal to pid succeeds.
func processAlive(pid int) bool {
	return unix.Kill(pid, 0) == nil
}

// killProcess sends SIGKILL to pid.
func killProcess(pid int) {
	_ = unix.Kill(pid, unix.SIGKILL)
}

// processCommandLine returns the command line of pid. Linux exposes it in
// /proc; elsewhere ps prints it.
func processCommandLine(ctx context.Context, pid int) (string, bool) {
	if data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline")); err == nil {
		return string(data), true
	}
	//nolint:gosec // ps receives a pid that Nyala formatted from an integer.
	out, err := exec.CommandContext(ctx, "ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimRight(string(out), "\n"), true
}
