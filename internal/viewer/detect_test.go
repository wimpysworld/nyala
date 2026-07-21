package viewer

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"testing"
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

// TestProbeSHMLifecycle asserts the probe object is created under /dev/shm
// with the 1x1 pixel payload and never leaks: the ignored case (the terminal
// sent no reply and left the object) is removed by unlinkProbeSHM, and the
// consumed case (the terminal already unlinked it) is a silent no-op.
func TestProbeSHMLifecycle(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("POSIX shm probe is Linux-only")
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
