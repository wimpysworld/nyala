package viewer

import (
	"testing"

	"github.com/wimpysworld/nyala/internal/cellraster"
)

// TestCellPxDefaultWhenGeometryAbsent asserts that when every geometry report is
// absent the resolved cell pixel size is the 8x16 default (8 wide, 16 tall). The
// source of truth is cellraster.CellPxW (8) and cellraster.CellPxH (16).
// NewState seeds these, and a temp file tty has no winsize and no handshake reply,
// so the chain rests on them.
func TestCellPxDefaultWhenGeometryAbsent(t *testing.T) {
	if cellraster.CellPxW != 8 || cellraster.CellPxH != 16 {
		t.Fatalf("raster default = %dx%d, want 8x16 (the pinned diagnostic raster default)", cellraster.CellPxW, cellraster.CellPxH)
	}
	st := NewState(80, 24)
	if st.W != 8 || st.H != 16 {
		t.Fatalf("default cell px = %dx%d, want 8x16", st.W, st.H)
	}
}

// TestCellPxInBounds exercises the real winsize sanity window. winsizeCellPixels
// reads a live ioctl, so the bound is split into the pure cellPx.inBounds, which
// this table drives directly with the boundary values. That puts the real
// comparison operators under test: flipping a < to <= at an edge fails a case
// here. A derived cell px is accepted only when 4..32 wide and 8..64 tall.
func TestCellPxInBounds(t *testing.T) {
	if winsizeMinCellW != 4 || winsizeMaxCellW != 32 {
		t.Fatalf("width bound = %d..%d, want 4..32", winsizeMinCellW, winsizeMaxCellW)
	}
	if winsizeMinCellH != 8 || winsizeMaxCellH != 64 {
		t.Fatalf("height bound = %d..%d, want 8..64", winsizeMinCellH, winsizeMaxCellH)
	}
	cases := []struct {
		name string
		pxX  int
		pxY  int
		want bool
	}{
		{"in range", 8, 16, true},
		{"min edge", winsizeMinCellW, winsizeMinCellH, true},
		{"max edge", winsizeMaxCellW, winsizeMaxCellH, true},
		{"width below min", winsizeMinCellW - 1, 16, false},
		{"width above max", winsizeMaxCellW + 1, 16, false},
		{"height below min", 8, winsizeMinCellH - 1, false},
		{"height above max", 8, winsizeMaxCellH + 1, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := (cellPx{W: c.pxX, H: c.pxY}).inBounds(); got != c.want {
				t.Fatalf("cellPx{%d, %d}.inBounds() = %v, want %v", c.pxX, c.pxY, got, c.want)
			}
		})
	}
}

// TestDeriveCellPixels exercises the pure core of winsizeCellPixels with
// synthetic winsize fields the live ioctl cannot supply. It pins the named
// 32000x0 bogus report: Ypixel 0 trips the zero-guard, so it resolves to ok=false
// before any division. It also pins a healthy report, a derived cell px outside
// the sanity window, and the round-to-nearest rule on both axes: a fractional
// device-pixel cell (xpixel no exact multiple of cols) rounds to the nearest
// integer cell instead of flooring, so the derived surface can overshoot the
// reported pixels by under half a cell per axis but never undershoots by up to a
// full cell (the Ghostty/WezTerm zoom-out shrink).
func TestDeriveCellPixels(t *testing.T) {
	cases := []struct {
		name                     string
		xpixel, ypixel, col, row int
		wantX, wantY             int
		wantOK                   bool
	}{
		{"healthy 80x24 at 8x16", 640, 384, 80, 24, 8, 16, true},
		{"32000x0 bogus winsize", 32000, 0, 80, 24, 0, 0, false},
		{"zero xpixel", 0, 384, 80, 24, 0, 0, false},
		{"zero col", 640, 384, 0, 24, 0, 0, false},
		{"zero row", 640, 384, 80, 0, 0, 0, false},
		{"derived cell px too wide", 32000, 384, 80, 24, 0, 0, false},
		// 688/80 = 8.6: floor lost 48 px of surface (0.6 px per column), round
		// overshoots by only 32 px. 404/24 = 16.83 rounds up the same way.
		{"fractional cell rounds up", 688, 404, 80, 24, 9, 17, true},
		// 675/80 = 8.4375 and 392/24 = 16.33 sit under the half-cell midpoint,
		// so both round down to the floor value.
		{"fractional cell rounds down", 675, 392, 80, 24, 8, 16, true},
		// Mixed axes: width rounds down (8.4375), height rounds up (16.83).
		{"fractional axes round independently", 675, 404, 80, 24, 8, 17, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			px, ok := deriveCellPixels(c.xpixel, c.ypixel, c.col, c.row)
			if ok != c.wantOK {
				t.Fatalf("deriveCellPixels(%d,%d,%d,%d) ok = %v, want %v",
					c.xpixel, c.ypixel, c.col, c.row, ok, c.wantOK)
			}
			if ok && (px.W != c.wantX || px.H != c.wantY) {
				t.Fatalf("cell px = %dx%d, want %dx%d", px.W, px.H, c.wantX, c.wantY)
			}
		})
	}
}

// TestParseCellPxOverride exercises the NYALA_CELL_PX override parser. It pins
// the diagnostic knob: a valid "WxH" resolves to that cell size and takes
// precedence, an unset value resolves to ok=false, and any malformed value fails
// loudly with an error rather than falling back.
func TestParseCellPxOverride(t *testing.T) {
	cases := []struct {
		name         string
		value        string
		set          bool
		wantW, wantH int
		wantOK       bool
		wantErr      bool
	}{
		{name: "unset", value: "", set: false, wantOK: false},
		{name: "valid 16x33", value: "16x33", set: true, wantW: 16, wantH: 33, wantOK: true},
		{name: "empty set value", value: "", set: true, wantErr: true},
		{name: "no separator", value: "1633", set: true, wantErr: true},
		{name: "non-integer width", value: "axb", set: true, wantErr: true},
		{name: "trailing junk", value: "16x33px", set: true, wantErr: true},
		{name: "zero width", value: "0x33", set: true, wantErr: true},
		{name: "zero height", value: "16x0", set: true, wantErr: true},
		{name: "negative width", value: "-16x33", set: true, wantErr: true},
		{name: "negative height", value: "16x-33", set: true, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			px, ok, err := parseCellPxOverride(c.value, c.set)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseCellPxOverride(%q, %v) err = nil, want error", c.value, c.set)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCellPxOverride(%q, %v): %v", c.value, c.set, err)
			}
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && (px.W != c.wantW || px.H != c.wantH) {
				t.Fatalf("cell px = %dx%d, want %dx%d", px.W, px.H, c.wantW, c.wantH)
			}
		})
	}
}

// TestWinsizeCellPixelsNonTTY pins the ioctl-failure fall-through the 8x16
// default relies on. A non-terminal fd has no winsize, so the ioctl fails and
// winsizeCellPixels returns ok=false before the pure core runs.
func TestWinsizeCellPixelsNonTTY(t *testing.T) {
	tty := newTempTTY(t)
	if _, ok := winsizeCellPixels(tty.Fd(), 80, 24); ok {
		t.Fatalf("winsizeCellPixels on a non-tty returned ok=true, want false")
	}
}
