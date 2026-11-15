//go:build !windows

package viewer

import (
	"math"
	"time"

	"golang.org/x/sys/unix"
)

// readUntilDA1 reads bytes from the tty fd with unix.Poll until the buffer holds
// a DA1 terminator or the cumulative deadline passes, then returns the buffer. It
// uses Poll rather than SetReadDeadline because SetReadDeadline returns
// ErrNoDeadline on stdin.
func readUntilDA1(fd int) []byte {
	if fd < 0 || fd > math.MaxInt32 {
		return nil
	}
	pollFD := int32(fd)
	deadline := time.Now().Add(probeTimeout)
	var buf []byte
	chunk := make([]byte, 256)
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		wait := probePollTimeout
		if remaining < wait {
			wait = remaining
		}
		fds := []unix.PollFd{{Fd: pollFD, Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(wait.Milliseconds()))
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return buf
		}
		if n == 0 {
			continue
		}
		rn, rerr := unix.Read(fd, chunk)
		if rn > 0 {
			buf = append(buf, chunk[:rn]...)
			if hasDA1Terminator(buf) {
				return buf
			}
		}
		if rerr != nil {
			return buf
		}
	}
	return buf
}
