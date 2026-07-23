//go:build linux

package viewer

import (
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/wimpysworld/nyala/internal/render/block"
)

// newSetupViewerTTY opens a Linux pseudoterminal slave with a fixed grid so
// setupViewer can exercise its terminal-only path without production hooks.
func newSetupViewerTTY(t *testing.T) *os.File {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open PTY master: %v", err)
	}
	t.Cleanup(func() { _ = master.Close() })

	ptn, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatalf("get PTY number: %v", err)
	}
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatalf("unlock PTY: %v", err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", ptn), os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open PTY slave: %v", err)
	}
	t.Cleanup(func() { _ = slave.Close() })
	if err := unix.IoctlSetWinsize(int(slave.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Col: 80, Row: 24}); err != nil {
		t.Fatalf("set PTY size: %v", err)
	}
	return slave
}

// TestSetupViewerRawModeClearsIXON proves the raw-mode termios setup clears
// IXON explicitly, not by assumed cfmakeraw semantics. With IXON set, the
// line discipline consumes Ctrl+Q (XON) and Ctrl+S (XOFF) as flow control and
// the quit key never reaches the key reader; the Ctrl+Q binding depends on
// this flag staying cleared.
func TestSetupViewerRawModeClearsIXON(t *testing.T) {
	clearMuxEnv(t)
	t.Setenv("TMUX", "/tmp/tmux-1000/default,123,0")
	tty := newSetupViewerTTY(t)

	fd, _, restore, err := setupViewer(&fakeEngine{}, tty, "auto", "auto", block.SchemeQuadrant)
	if err != nil {
		t.Fatalf("setupViewer: %v", err)
	}
	t.Cleanup(restore)

	termios, err := unix.IoctlGetTermios(int(fd), unix.TCGETS)
	if err != nil {
		t.Fatalf("read raw-mode termios: %v", err)
	}
	if termios.Iflag&unix.IXON != 0 {
		t.Fatal("raw mode left IXON set; Ctrl+Q would be eaten as XON flow control")
	}
}

// TestSetupViewerWiresResolvedScheme proves every --blocks resolution reaches
// matching viewer state through the launch setup seam.
func TestSetupViewerWiresResolvedScheme(t *testing.T) {
	clearMuxEnv(t)
	t.Setenv("TMUX", "/tmp/tmux-1000/default,123,0")
	supportedEnv := []string{"TERM=xterm-kitty"}
	unsupportedEnv := []string{"TERM=xterm-256color"}
	cases := []struct {
		name   string
		blocks string
		env    []string
		want   block.Scheme
	}{
		{"quadrant", "quadrant", supportedEnv, block.SchemeQuadrant},
		{"half", "half", supportedEnv, block.SchemeHalfBlock},
		{"auto supported", "auto", supportedEnv, block.SchemeSextant},
		{"auto unsupported", "auto", unsupportedEnv, block.SchemeQuadrant},
		{"sextant supported", "sextant", supportedEnv, block.SchemeSextant},
		{"sextant unsupported", "sextant", unsupportedEnv, block.SchemeQuadrant},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := resolveScheme(tc.blocks, tc.env)
			if scheme != tc.want {
				t.Fatalf("resolveScheme(%q, env) = %d, want %d", tc.blocks, scheme, tc.want)
			}

			eng := &fakeEngine{}
			_, state, restore, err := setupViewer(eng, newSetupViewerTTY(t), "auto", "auto", scheme)
			if err != nil {
				t.Fatalf("setupViewer: %v", err)
			}
			t.Cleanup(restore)
			if state.Scheme != scheme {
				t.Fatalf("viewer scheme = %d, want resolved scheme %d", state.Scheme, scheme)
			}
		})
	}
}
