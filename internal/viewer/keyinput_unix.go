//go:build !windows

package viewer

import (
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// terminalKeyInput polls the borrowed terminal and a wake pipe. The pipe lets
// shutdown interrupt an idle read without changing the terminal's file flags.
type terminalKeyInput struct {
	fds      [2]unix.PollFd
	wake     [2]*os.File
	deadline time.Time
}

func newTerminalKeyInput(tty *os.File) (*terminalKeyInput, error) {
	fd := tty.Fd()
	if fd > math.MaxInt32 {
		return nil, fmt.Errorf("invalid terminal file descriptor: %d", fd)
	}
	wakeRead, wakeWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	wakeFD := wakeRead.Fd()
	if wakeFD > math.MaxInt32 {
		_ = wakeRead.Close()
		_ = wakeWrite.Close()
		return nil, fmt.Errorf("invalid wake pipe file descriptor: %d", wakeFD)
	}
	return &terminalKeyInput{
		fds: [2]unix.PollFd{
			{Fd: int32(wakeFD), Events: unix.POLLIN},
			{Fd: int32(fd), Events: unix.POLLIN},
		},
		wake: [2]*os.File{wakeRead, wakeWrite},
	}, nil
}

func (r *terminalKeyInput) setDeadline(deadline time.Time) {
	r.deadline = deadline
}

func (r *terminalKeyInput) interrupt() { _ = r.wake[1].Close() }
func (r *terminalKeyInput) close()     { _ = r.wake[0].Close() }

func (r *terminalKeyInput) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		wait := -1
		if !r.deadline.IsZero() {
			remaining := time.Until(r.deadline)
			if remaining <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			// Round up because Poll takes whole milliseconds.
			wait = int((remaining + time.Millisecond - 1) / time.Millisecond)
		}
		n, err := unix.Poll(r.fds[:], wait)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		switch {
		case n == 0:
			return 0, os.ErrDeadlineExceeded
		case r.fds[0].Revents != 0:
			return 0, io.EOF
		case r.fds[1].Revents&unix.POLLNVAL != 0:
			return 0, unix.EBADF
		}
		n, err = unix.Read(int(r.fds[1].Fd), p)
		if err == unix.EINTR || err == unix.EAGAIN {
			continue
		}
		if n == 0 && err == nil {
			return 0, io.EOF
		}
		return max(n, 0), err
	}
}
