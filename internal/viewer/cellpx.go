package viewer

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// cellPx is a terminal cell's pixel size: the width and height of one character
// cell. The two values always travel together (resolve, validate, scale), so the
// pair carries one type rather than a loose (pxX, pxY) int pair through the
// resolution chain.
type cellPx struct {
	W int // cell width in pixels
	H int // cell height in pixels
}

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

// deriveCellPixels turns a winsize report into a per-cell pixel size. It is the
// pure core of winsizeCellPixels, split out so the zero-guard and the sanity
// bound are testable without a live ioctl. It returns ok=false when any field
// is zero, so a bogus report (for example a 32000x0 winsize, Ypixel 0) falls
// through, and ok=false when the derived cell px is outside the sane window.
//
// Each axis rounds to the nearest integer cell rather than flooring. A terminal
// with fractional device-pixel cells (Ghostty, WezTerm, most visibly after a
// zoom) reports an xpixel that is no exact multiple of cols, and flooring lost
// up to one pixel per column of surface, a visible shrink of the rendered image
// inside the window. Rounding bounds the error to half a pixel per cell in
// either direction, so cols*cellW (or rows*cellH) can overshoot the reported
// pixel size by under half the grid dimension. The overshoot is safe: every
// live Sixel frame travels inside DECSDM fixed placement
// (beginSixelDisplayMode), which clips at the screen edge instead of scrolling
// the viewport, and Kitty scales its placement to the cell area.
func deriveCellPixels(xpixel, ypixel, col, row int) (cellPx, bool) {
	if xpixel == 0 || ypixel == 0 || col == 0 || row == 0 {
		return cellPx{}, false
	}
	c := cellPx{
		W: int(math.Round(float64(xpixel) / float64(col))),
		H: int(math.Round(float64(ypixel) / float64(row))),
	}
	if !c.inBounds() {
		return cellPx{}, false
	}
	return c, true
}

// resizeCellPx is the pure resize-time cell-pair decision: it returns the pair
// the viewer and engine should use after a terminal resize and whether it
// changed. A forced NYALA_CELL_PX pair never moves, a failed winsize
// derivation keeps the current pair (fail closed, the same policy as startup),
// and an unchanged derivation reports no change so the engine restart skips
// SetCellPx. The ioctl stays at the seam edge in winsizeCellPixels.
func resizeCellPx(forced bool, current, derived cellPx, derivedOK bool) (cellPx, bool) {
	if forced || !derivedOK || derived == current {
		return current, false
	}
	return derived, true
}

// inBounds reports whether a derived cell pixel size sits inside the winsize
// sanity window: 4..32 wide and 8..64 tall. A size outside it is a bogus winsize
// report, so the caller falls through to the handshake or the 8x16 default.
func (c cellPx) inBounds() bool {
	return c.W >= winsizeMinCellW && c.W <= winsizeMaxCellW &&
		c.H >= winsizeMinCellH && c.H <= winsizeMaxCellH
}

// winsize cell-px sanity bounds. A derived cell width or height outside this
// window is treated as a bogus winsize report and rejected to the safe default.
const (
	winsizeMinCellW = 4
	winsizeMaxCellW = 32
	winsizeMinCellH = 8
	winsizeMaxCellH = 64
)

// cellPxEnv is the environment variable name for the cell-pixel-size override.
const cellPxEnv = "NYALA_CELL_PX"

// parseCellPxOverride parses a NYALA_CELL_PX override value of the form "WxH"
// (two positive integers, for example "16x33"). It is a diagnostic and harness
// knob: it lets a headless run behind a synthetic pty, where no winsize pixel
// report exists, pin the cell pixel size and reproduce a real terminal's device
// quantisation. When set (set=true) it takes precedence over the ioctl-derived
// and handshake-probed cell; production without the variable never calls this.
// It returns ok=false when unset. A set value that is empty, not "WxH", or holds
// a non-integer, zero, or negative width or height returns an error naming
// NYALA_CELL_PX, so a bad override fails loudly rather than falling back.
func parseCellPxOverride(value string, set bool) (cellPx, bool, error) {
	if !set {
		return cellPx{}, false, nil
	}
	w, h, found := strings.Cut(value, "x")
	if !found {
		return cellPx{}, false, fmt.Errorf("%s=%q: want WxH, two positive integers", cellPxEnv, value)
	}
	pxW, errW := strconv.Atoi(w)
	pxH, errH := strconv.Atoi(h)
	if errW != nil || errH != nil || pxW <= 0 || pxH <= 0 {
		return cellPx{}, false, fmt.Errorf("%s=%q: want WxH, two positive integers", cellPxEnv, value)
	}
	return cellPx{W: pxW, H: pxH}, true, nil
}
