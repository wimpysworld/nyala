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

// browserSignalTarget selects the browser's private process group when pid is
// its leader. Capture it before signalling: a wrapper can exit before its
// browser children, but the group identifier remains while members exist.
func browserSignalTarget(pid int) int {
	if pid > 1 && pid != unix.Getpgrp() {
		if group, err := unix.Getpgid(pid); err == nil && group == pid {
			return -pid
		}
	}
	return pid
}

// signalTerminate sends SIGTERM to a PID or a captured negative process group.
// gone is true when ESRCH shows that the target has already exited.
func signalTerminate(target int) (gone bool, err error) {
	err = unix.Kill(target, unix.SIGTERM)
	if errors.Is(err, unix.ESRCH) {
		return true, nil
	}
	return false, err
}

// processGone reports whether a null signal to the target fails with ESRCH.
// Unreaped zombies still belong to their group and keep this check false.
func processGone(target int) bool {
	return errors.Is(unix.Kill(target, 0), unix.ESRCH)
}

// processAlive reports whether a null signal to pid succeeds.
func processAlive(pid int) bool {
	return unix.Kill(pid, 0) == nil
}

// killProcess sends SIGKILL to a PID or a captured negative process group.
func killProcess(target int) {
	_ = unix.Kill(target, unix.SIGKILL)
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
