//go:build !windows

package viewer

import (
	"os"
	"os/signal"
	"syscall"
)

// notifyResize returns a channel that receives on each terminal resize, and a
// stop function that ends the notification.
func notifyResize(_ uintptr) (<-chan os.Signal, func()) {
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	return winch, func() { signal.Stop(winch) }
}
