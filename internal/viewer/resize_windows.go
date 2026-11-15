//go:build windows

package viewer

import (
	"os"
	"syscall"
	"time"

	cterm "github.com/charmbracelet/x/term"
)

// resizePollInterval is how often the console size is read. Windows has no
// SIGWINCH, so a change is found by polling.
const resizePollInterval = 250 * time.Millisecond

// notifyResize returns a channel that receives when the console size in cells
// changes, and a stop function that ends the poll. A missed send is dropped,
// since one pending notice already makes the loop read the new size.
func notifyResize(fd uintptr) (<-chan os.Signal, func()) {
	winch := make(chan os.Signal, 1)
	done := make(chan struct{})
	go func() {
		cols, rows, _ := cterm.GetSize(fd)
		ticker := time.NewTicker(resizePollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
			}
			c, r, err := cterm.GetSize(fd)
			if err != nil || (c == cols && r == rows) {
				continue
			}
			cols, rows = c, r
			select {
			case winch <- syscall.Signal(0):
			default:
			}
		}
	}()
	return winch, func() { close(done) }
}
