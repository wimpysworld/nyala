package viewer

import "testing"

// TestParseDA1Sixel asserts the DA1 parser matches the sixel attribute as a whole
// element "4" and never as a substring, so "64" and "14" do not false-match.
func TestParseDA1Sixel(t *testing.T) {
	cases := []struct {
		name string
		buf  string
		want bool
	}{
		{"sixel present mid-list", "\x1b[?62;4;6;9;15;22c", true},
		{"sixel first element", "\x1b[?4;6c", true},
		{"sixel last element", "\x1b[?62;4c", true},
		{"sixel only element", "\x1b[?4c", true},
		{"no sixel", "\x1b[?62;6;9;15;22c", false},
		{"sixty-four not four", "\x1b[?62;64c", false},
		{"fourteen not four", "\x1b[?62;14c", false},
		{"no da1 reply", "garbage", false},
		{"unterminated da1", "\x1b[?62;4", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseDA1Sixel([]byte(c.buf)); got != c.want {
				t.Fatalf("parseDA1Sixel(%q) = %v, want %v", c.buf, got, c.want)
			}
		})
	}
}

// TestParseKittySupport asserts the kitty parser matches the APC reply only when
// it carries ";OK".
func TestParseKittySupport(t *testing.T) {
	cases := []struct {
		name string
		buf  string
		want bool
	}{
		{"ok reply", "\x1b_Gi=31;OK\x1b\\\x1b[?62;4c", true},
		{"error reply", "\x1b_Gi=31;EBADF:bad\x1b\\\x1b[?62c", false},
		{"no apc", "\x1b[?62;4c", false},
		{"apc no terminator", "\x1b_Gi=31;OK", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseKittySupport([]byte(c.buf)); got != c.want {
				t.Fatalf("parseKittySupport(%q) = %v, want %v", c.buf, got, c.want)
			}
		})
	}
}

// TestParseCellPixels asserts the cell-pixel resolution chain: the 16t reply wins
// directly, the 14t reply divides by the 18t cell counts, the 14t reply falls
// back to the passed-in counts, and an empty buffer yields ok=false.
func TestParseCellPixels(t *testing.T) {
	cases := []struct {
		name         string
		buf          string
		cols, rows   int
		wantX, wantY int
		wantOK       bool
	}{
		{
			name: "16t direct",
			buf:  "\x1b[6;16;8t",
			cols: 80, rows: 24,
			wantX: 8, wantY: 16, wantOK: true,
		},
		{
			name: "14t with 18t counts",
			buf:  "\x1b[4;384;640t\x1b[8;24;80t",
			cols: 10, rows: 10,
			wantX: 8, wantY: 16, wantOK: true,
		},
		{
			name: "14t with passed-in counts",
			buf:  "\x1b[4;384;640t",
			cols: 80, rows: 24,
			wantX: 8, wantY: 16, wantOK: true,
		},
		{
			name: "none",
			buf:  "\x1b[?62;4c",
			cols: 80, rows: 24,
			wantOK: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			px, ok := parseCellPixels([]byte(c.buf), c.cols, c.rows)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if ok && (px.W != c.wantX || px.H != c.wantY) {
				t.Fatalf("cell px = %dx%d, want %dx%d", px.W, px.H, c.wantX, c.wantY)
			}
		})
	}
}
