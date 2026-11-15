//go:build !windows

package viewer

import "golang.org/x/sys/unix"

// winsizeCellPixels resolves the cell pixel size from the TIOCGWINSZ ioctl. It
// returns ok=false when the kernel reports no pixel size, so the caller falls
// back to the handshake replies. cols and rows are unused here but kept for a
// symmetric resolution chain with parseCellPixels.
func winsizeCellPixels(fd uintptr, _, _ int) (cellPx, bool) {
	ws, err := unix.IoctlGetWinsize(int(fd), unix.TIOCGWINSZ)
	if err != nil || ws == nil {
		return cellPx{}, false
	}
	return deriveCellPixels(int(ws.Xpixel), int(ws.Ypixel), int(ws.Col), int(ws.Row))
}
