//go:build windows

package engine

import (
	"context"
	"errors"
	"math"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// detachedSysProcAttr starts the browser in a new process group, so a console
// Ctrl+C does not reach it.
func detachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
}

// errNoProcess stands in for ESRCH: no process has the pid.
var errNoProcess = errors.New("no such process")

// openProcess opens pid with access. It returns errNoProcess when no process
// has the pid.
func openProcess(pid int, access uint32) (windows.Handle, error) {
	if pid <= 0 || pid > math.MaxUint32 {
		return 0, errNoProcess
	}
	h, err := windows.OpenProcess(access, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return 0, errNoProcess
	}
	return h, err
}

// exited reports whether the process behind h has ended.
func exited(h windows.Handle) bool {
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == windows.WAIT_OBJECT_0
}

// signalTerminate ends pid with TerminateProcess, since Windows has no SIGTERM.
// Chromium's child processes exit with the browser process. gone is true when
// the process has already exited.
func signalTerminate(pid int) (gone bool, err error) {
	h, err := openProcess(pid, windows.PROCESS_TERMINATE|windows.SYNCHRONIZE)
	if errors.Is(err, errNoProcess) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	if exited(h) {
		return true, nil
	}
	return false, windows.TerminateProcess(h, 1)
}

// processGone reports whether pid has exited or no process has it.
func processGone(pid int) bool {
	h, err := openProcess(pid, windows.SYNCHRONIZE)
	if err != nil {
		return errors.Is(err, errNoProcess)
	}
	defer func() { _ = windows.CloseHandle(h) }()
	return exited(h)
}

// processAlive reports whether pid is a running process.
func processAlive(pid int) bool {
	h, err := openProcess(pid, windows.SYNCHRONIZE)
	if err != nil {
		return false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	return !exited(h)
}

// killProcess ends pid.
func killProcess(pid int) {
	_, _ = signalTerminate(pid)
}

// processCommandLine returns the command line of pid from the process
// information that NtQueryInformationProcess reports. Windows keeps one
// quoted string, so split it into arguments and join them with NUL, which
// commandLineHasArg treats as a bound. A profile path with a space then
// matches.
func processCommandLine(_ context.Context, pid int) (string, bool) {
	h, err := openProcess(pid, windows.PROCESS_QUERY_LIMITED_INFORMATION)
	if err != nil {
		return "", false
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var size uint32
	_ = windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation, nil, 0, &size)
	if size < uint32(unsafe.Sizeof(windows.NTUnicodeString{})) {
		return "", false
	}
	buf := make([]byte, size)
	if err := windows.NtQueryInformationProcess(h, windows.ProcessCommandLineInformation, unsafe.Pointer(&buf[0]), size, &size); err != nil {
		return "", false
	}
	line := (*windows.NTUnicodeString)(unsafe.Pointer(&buf[0])).String()
	args, err := windows.DecomposeCommandLine(line)
	if err != nil {
		return line, true
	}
	return strings.Join(args, "\x00"), true
}
