//go:build windows

package viewer

// winsizeCellPixels reports no pixel size on Windows. The console API gives the
// size in cells only, so the caller falls back to the handshake replies.
func winsizeCellPixels(_ uintptr, _, _ int) (cellPx, bool) {
	return cellPx{}, false
}
