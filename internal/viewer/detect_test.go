package viewer

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

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

// TestParseKittySupport asserts the kitty parser matches an APC reply only when
// it carries ";OK" for the requested image id, so the direct and shm probes
// never claim each other's acknowledgement.
func TestParseKittySupport(t *testing.T) {
	cases := []struct {
		name string
		buf  string
		id   string
		want bool
	}{
		{"ok reply", "\x1b_Gi=31;OK\x1b\\\x1b[?62;4c", kittyDirectID, true},
		{"error reply", "\x1b_Gi=31;EBADF:bad\x1b\\\x1b[?62c", kittyDirectID, false},
		{"no apc", "\x1b[?62;4c", kittyDirectID, false},
		{"apc no terminator", "\x1b_Gi=31;OK", kittyDirectID, true},
		{"shm ok after direct ok", "\x1b_Gi=31;OK\x1b\\\x1b_Gi=32;OK\x1b\\\x1b[?62;4c", kittySHMID, true},
		{"shm error reply", "\x1b_Gi=31;OK\x1b\\\x1b_Gi=32;EBADF:bad shm\x1b\\\x1b[?62;4c", kittySHMID, false},
		{"shm silence wezterm", "\x1b_Gi=31;OK\x1b\\\x1b[?62;4c", kittySHMID, false},
		{"direct ok not claimed by shm error", "\x1b_Gi=32;EBADF:bad\x1b\\\x1b_Gi=31;OK\x1b\\\x1b[?62;4c", kittyDirectID, true},
		{"id is whole element not prefix", "\x1b_Gi=320;OK\x1b\\\x1b[?62;4c", kittySHMID, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseKittySupport([]byte(c.buf), c.id); got != c.want {
				t.Fatalf("parseKittySupport(%q, %q) = %v, want %v", c.buf, c.id, got, c.want)
			}
		})
	}
}

// TestQueryBytesSHM asserts the handshake batch includes the shm transport
// query only when a probe object name is supplied, keeps the two probe ids
// distinct, and always ends with the DA1 fence.
func TestQueryBytesSHM(t *testing.T) {
	withSHM := queryBytes("/nyala-shm-probe-1")
	if !bytes.Contains(withSHM, []byte("i="+kittySHMID+",")) || !bytes.Contains(withSHM, []byte("t=s")) {
		t.Fatalf("shm query missing from batch: %q", withSHM)
	}
	if !bytes.Contains(withSHM, []byte("i="+kittyDirectID+",")) {
		t.Fatalf("direct query missing from batch: %q", withSHM)
	}
	without := queryBytes("")
	if bytes.Contains(without, []byte("t=s")) {
		t.Fatalf("shm query present without a probe object: %q", without)
	}
	for _, b := range [][]byte{withSHM, without} {
		if !bytes.HasSuffix(b, []byte("\x1b[c")) {
			t.Fatalf("batch does not end with the DA1 fence: %q", b)
		}
	}
}

// TestReducedQueryBytes pins the multiplexer handshake batch byte for byte:
// the three cell-size queries, XTVERSION, then the DA1 fence, and no Kitty
// a=q APC probe. tmux and zellij consume APC without passthrough, so any APC
// in this batch would vanish unanswered; the batch must stay APC-free.
func TestReducedQueryBytes(t *testing.T) {
	got := reducedQueryBytes()
	want := []byte("\x1b[16t\x1b[14t\x1b[18t\x1b[>0q\x1b[c")
	if !bytes.Equal(got, want) {
		t.Fatalf("reducedQueryBytes() = %q, want %q", got, want)
	}
	if bytes.Contains(got, []byte("\x1b_G")) {
		t.Fatalf("reduced batch carries a Kitty APC probe: %q", got)
	}
}

// TestParseKittyKeyboard asserts the keyboard-protocol capability parse: only a
// "CSI ? flags u" reply counts, whatever flag value it carries, and the DA1 reply
// that shares the "CSI ?" prefix never false-matches. A silent terminal (every
// one without the protocol) reports no support, so those sessions keep the legacy
// parser.
func TestParseKittyKeyboard(t *testing.T) {
	cases := []struct {
		name string
		buf  string
		want bool
	}{
		{"flags zero", "\x1b[?0u\x1b[?62;4c", true},
		{"flags one", "\x1b[?1u\x1b[?62;4c", true},
		{"flags many", "\x1b[?27u\x1b[?62;4c", true},
		{"reply after da1", "\x1b[?62;4c\x1b[?1u", true},
		{"reply among other reports", "\x1b[6;16;8t\x1b[?1u\x1b[?62;4c", true},
		{"silent terminal", "\x1b[?62;4c", false},
		{"da1 alone is not the protocol", "\x1b[?62;4;9c", false},
		{"empty buffer", "", false},
		{"no digits before the final", "\x1b[?u", false},
		{"parameter list is another report", "\x1b[?62;1u", false},
		{"kitty graphics reply is not the protocol", "\x1b_Gi=31;OK\x1b\\", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseKittyKeyboard([]byte(c.buf)); got != c.want {
				t.Fatalf("parseKittyKeyboard(%q) = %v, want %v", c.buf, got, c.want)
			}
		})
	}
}

// TestKittyKeyboardQueryRidesTheHandshake asserts the capability query travels in
// the one startup batch, ahead of the DA1 fence that bounds the read, so a
// terminal that never answers cannot hang the probe or leave the reply behind for
// the key reader. The reduced multiplexer handshake deliberately omits it.
func TestKittyKeyboardQueryRidesTheHandshake(t *testing.T) {
	full := string(queryBytes(""))
	if !strings.Contains(full, queryKittyKeyboard) {
		t.Fatalf("full handshake %q omits the keyboard query %q", full, queryKittyKeyboard)
	}
	da1 := strings.Index(full, "\x1b[c")
	if da1 < 0 {
		t.Fatalf("full handshake %q has no DA1 fence", full)
	}
	if strings.Index(full, queryKittyKeyboard) > da1 {
		t.Fatal("the keyboard query trails the DA1 fence, so its reply could arrive after the read ends")
	}
	if reduced := string(reducedQueryBytes()); strings.Contains(reduced, queryKittyKeyboard) {
		t.Fatalf("reduced handshake %q carries the keyboard query", reduced)
	}
}

// TestProbeSHMLifecycle asserts the probe object is created under /dev/shm
// with the 1x1 pixel payload and never leaks: the ignored case (the terminal
// sent no reply and left the object) is removed by unlinkProbeSHM, and the
// consumed case (the terminal already unlinked it) is a silent no-op.
func TestProbeSHMLifecycle(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("probe lifecycle test requires Linux /dev/shm")
	}
	for _, c := range []struct {
		name     string
		consumed bool
	}{
		{"ignored by terminal", false},
		{"consumed by terminal", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			name, err := createProbeSHM()
			if err != nil {
				t.Fatalf("createProbeSHM: %v", err)
			}
			path := "/dev/shm/" + strings.TrimPrefix(name, "/")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("probe object unreadable: %v", err)
			}
			if len(data) != 3 {
				t.Fatalf("probe payload = %d bytes, want 3", len(data))
			}
			if c.consumed {
				if err := os.Remove(path); err != nil {
					t.Fatalf("simulate terminal unlink: %v", err)
				}
			}
			unlinkProbeSHM(name)
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("probe object leaked at %s (stat err: %v)", path, err)
			}
		})
	}
}

// TestParseXTVersion asserts the XTVERSION parser extracts the identity text
// from real terminal replies and returns an empty string when the terminal
// ignores the query.
func TestParseXTVersion(t *testing.T) {
	cases := []struct {
		name string
		buf  string
		want string
	}{
		{"kitty", "\x1bP>|kitty 0.40.1\x1b\\\x1b[?62;4c", "kitty 0.40.1"},
		{"ghostty", "\x1bP>|ghostty 1.1.3\x1b\\\x1b[?62;4;22c", "ghostty 1.1.3"},
		{"wezterm", "\x1bP>|WezTerm 20240203-110809-5046fc22\x1b\\\x1b[?65;4;6;18;22c", "WezTerm 20240203-110809-5046fc22"},
		{"konsole", "\x1bP>|Konsole 24.12.3\x1b\\\x1b[?62;1;4c", "Konsole 24.12.3"},
		{"no reply", "\x1b[?62;4c", ""},
		{"missing st cut at da1", "\x1bP>|kitty 0.40.1\x1b[?62;4c", "kitty 0.40.1"},
		{"empty buffer", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseXTVersion([]byte(c.buf)); got != c.want {
				t.Fatalf("parseXTVersion(%q) = %q, want %q", c.buf, got, c.want)
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

// TestParseBackgroundDark pins the OSC 11 background parse across the reply
// forms terminals send: XParseColor "rgb:" and "rgba:" at every legal digit
// width, the "#" hex forms, and both terminators. A silent terminal and an
// unreadable spec both report no answer, so the bar falls back rather than
// guessing a polarity, and so does a reply that never reaches its terminator.
func TestParseBackgroundDark(t *testing.T) {
	cases := []struct {
		name     string
		buf      string
		wantDark bool
		wantOK   bool
	}{
		{"black bel", "\x1b]11;rgb:0000/0000/0000\x07\x1b[?62;4c", true, true},
		{"black st", "\x1b]11;rgb:0000/0000/0000\x1b\\\x1b[?62;4c", true, true},
		{"white bel", "\x1b]11;rgb:ffff/ffff/ffff\x07\x1b[?62;4c", false, true},
		{"solarized dark", "\x1b]11;rgb:0000/2b2b/3636\x07", true, true},
		{"solarized light", "\x1b]11;rgb:fdfd/f6f6/e3e3\x07", false, true},
		{"one digit per component", "\x1b]11;rgb:0/0/0\x07", true, true},
		{"two digits per component", "\x1b]11;rgb:ff/ff/ff\x07", false, true},
		{"three digits per component", "\x1b]11;rgb:fff/fff/fff\x07", false, true},
		{"uppercase digits", "\x1b]11;rgb:FFFF/FFFF/FFFF\x07", false, true},
		{"rgba drops the alpha", "\x1b]11;rgba:0000/0000/0000/8000\x07", true, true},
		{"hex short", "\x1b]11;#000\x07", true, true},
		{"hex six", "\x1b]11;#ffffff\x07", false, true},
		{"hex nine", "\x1b]11;#000000000\x07", true, true},
		{"hex twelve", "\x1b]11;#ffffffffffff\x07", false, true},
		// HSL lightness is the midpoint of the brightest and dimmest
		// channels, so a fully saturated hue sits exactly on the boundary
		// whatever its hue, and the boundary itself reads light.
		{"saturated blue sits on the boundary", "\x1b]11;rgb:0000/0000/ffff\x07", false, true},
		{"mid grey is light", "\x1b]11;rgb:8080/8080/8080\x07", false, true},
		{"just below mid grey is dark", "\x1b]11;rgb:7f7f/7f7f/7f7f\x07", true, true},
		{"silent terminal", "\x1b[?62;4c", false, false},
		{"empty buffer", "", false, false},
		{"colour name needs a table", "\x1b]11;grey20\x07", false, false},
		{"missing component", "\x1b]11;rgb:0000/0000\x07", false, false},
		{"non-hex digit", "\x1b]11;rgb:zzzz/0000/0000\x07", false, false},
		{"too many digits", "\x1b]11;rgb:00000/0000/0000\x07", false, false},
		{"non-hex alpha", "\x1b]11;rgba:0000/0000/0000/zz!!\x07", false, false},
		{"too many digits in the alpha", "\x1b]11;rgba:0000/0000/0000/00000\x07", false, false},
		{"empty component", "\x1b]11;rgb://\x07", false, false},
		{"empty spec", "\x1b]11;\x07", false, false},
		{"hex of no legal width", "\x1b]11;#ffff\x07", false, false},
		{"foreground reply is not the background", "\x1b]10;rgb:ffff/ffff/ffff\x07", false, false},
		// A truncated reply must not borrow the next reply's escape byte as its
		// terminator: the leading digits would otherwise parse as a colour.
		{"truncated before the DA1 reply", "\x1b]11;rgb:0000/0000/0000\x1b[?62;4c", false, false},
		{"truncated mid-spec before the DA1 reply", "\x1b]11;rgb:0000/0000/00\x1b[?62;4c", false, false},
		{"no terminator at all", "\x1b]11;rgb:0000/0000/0000", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dark, ok := parseBackgroundDark([]byte(c.buf))
			if dark != c.wantDark || ok != c.wantOK {
				t.Fatalf("parseBackgroundDark(%q) = (%v, %v), want (%v, %v)", c.buf, dark, ok, c.wantDark, c.wantOK)
			}
		})
	}
}

// TestBackgroundQueryRidesTheHandshake asserts the OSC 11 query travels in the
// one startup batch, ahead of the DA1 fence that bounds the read, so a
// terminal that never answers cannot hang the probe and the reply is parsed
// off the raw fd rather than reaching the page as typed text. The reduced
// multiplexer handshake omits it, because tmux answers from a cache read once
// at attach.
func TestBackgroundQueryRidesTheHandshake(t *testing.T) {
	full := string(queryBytes(""))
	if !strings.Contains(full, ansi.RequestBackgroundColor) {
		t.Fatalf("full handshake %q omits the background query", full)
	}
	da1 := strings.Index(full, "\x1b[c")
	if da1 < 0 {
		t.Fatalf("full handshake %q has no DA1 fence", full)
	}
	if strings.Index(full, ansi.RequestBackgroundColor) > da1 {
		t.Fatal("the background query trails the DA1 fence, so its reply could arrive after the read ends")
	}
	if reduced := string(reducedQueryBytes()); strings.Contains(reduced, osc11Prefix) {
		t.Fatalf("reduced handshake %q carries the background query", reduced)
	}
}

// TestBackgroundReplyKeepsTheOtherParsesIntact asserts an OSC 11 reply in the
// handshake buffer disturbs no other parse: it holds no "\x1b[?", so it cannot
// terminate the DA1 read early, and it is not mistaken for a cell-size report,
// an XTVERSION identity, or a Kitty reply.
func TestBackgroundReplyKeepsTheOtherParsesIntact(t *testing.T) {
	buf := []byte("\x1b_Gi=31;OK\x1b\\\x1b[6;16;8t\x1bP>|kitty 0.40.1\x1b\\\x1b]11;rgb:1c1c/1c1c/1c1c\x07\x1b[?62;4c")
	if hasDA1Terminator([]byte("\x1b]11;rgb:1c1c/1c1c/1c1c\x07")) {
		t.Fatal("an OSC 11 reply alone satisfied the DA1 fence")
	}
	if !parseKittySupport(buf, kittyDirectID) {
		t.Fatal("the Kitty reply was lost beside the background reply")
	}
	if got := parseXTVersion(buf); got != "kitty 0.40.1" {
		t.Fatalf("parseXTVersion = %q, want the identity intact", got)
	}
	px, ok := parseCellPixels(buf, 80, 24)
	if !ok || px.W != 8 || px.H != 16 {
		t.Fatalf("cell px = %+v (ok %v), want 8x16 intact", px, ok)
	}
	if !parseDA1Sixel(buf) {
		t.Fatal("the DA1 sixel attribute was lost beside the background reply")
	}
	dark, bgOK := parseBackgroundDark(buf)
	if !dark || !bgOK {
		t.Fatalf("parseBackgroundDark = (%v, %v), want a dark background", dark, bgOK)
	}
}
