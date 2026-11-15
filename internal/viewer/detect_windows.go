//go:build windows

package viewer

import (
	"time"

	"golang.org/x/sys/windows"
)

// readUntilDA1 reads bytes from the console input handle until the buffer holds
// a DA1 terminator or the cumulative deadline passes, then returns the buffer.
// WaitForSingleObject bounds each wait, so a terminal that never answers cannot
// hold the read past the deadline.
func readUntilDA1(fd int) []byte {
	if fd < 0 {
		return nil
	}
	h := windows.Handle(fd)
	deadline := time.Now().Add(probeTimeout)
	var buf []byte
	chunk := make([]byte, 256)
	for time.Now().Before(deadline) {
		wait := min(time.Until(deadline), probePollTimeout).Milliseconds()
		if wait < 0 {
			wait = 0
		}
		ev, err := windows.WaitForSingleObject(h, uint32(wait))
		if err != nil {
			return buf
		}
		if ev != windows.WAIT_OBJECT_0 {
			continue
		}
		var rn uint32
		rerr := windows.ReadFile(h, chunk, &rn, nil)
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
