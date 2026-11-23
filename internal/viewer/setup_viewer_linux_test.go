//go:build linux

package viewer

import (
	"fmt"
	"io"
	"os"
	"slices"
	"testing"
	"time"

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

func TestConfigureBackendsHerdr(t *testing.T) {
	clearMuxEnv(t)
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "p1")
	t.Setenv(cellPxEnv, "")
	if err := os.Unsetenv(cellPxEnv); err != nil {
		t.Fatal(err)
	}

	const (
		okReply  = "\x1b_Gi=31;OK\x1b\\"
		badReply = "\x1b_Gi=31;ENOTSUP:graphics disabled\x1b\\"
		geometry = "\x1b[6;24;12t"
		fence    = "\x1b[?62;4c"
		query    = "\x1b_Gi=31,s=1,v=1,a=q,t=d,f=24;AAAA\x1b\\\x1b[16t\x1b[14t\x1b[18t\x1b[>0q\x1b[c"
	)
	cases := []struct {
		name      string
		reply     string
		override  string
		transport KittyTransport
		wantKitty bool
		wantPx    cellPx
	}{
		{"direct support and geometry", okReply + geometry + fence, "", KittyTransportAuto, true, cellPx{W: 12, H: 24}},
		{"direct support and geometry override", okReply + fence, "11x22", KittyTransportDirect, true, cellPx{W: 11, H: 22}},
		{"direct support without geometry", okReply + fence, "", KittyTransportAuto, false, cellPx{W: 8, H: 16}},
		{"unusable geometry", okReply + "\x1b[4;23;79t" + fence, "", KittyTransportAuto, false, cellPx{W: 8, H: 16}},
		{"explicit shared memory", okReply + geometry + fence, "", KittyTransportSHM, false, cellPx{W: 12, H: 24}},
		{"unsupported graphics", badReply + geometry + fence, "", KittyTransportAuto, false, cellPx{W: 12, H: 24}},
		{"no Kitty reply", geometry + fence, "", KittyTransportAuto, false, cellPx{W: 12, H: 24}},
		{"no replies", "", "", KittyTransportAuto, false, cellPx{W: 8, H: 16}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.override != "" {
				t.Setenv(cellPxEnv, c.override)
			}
			fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_NONBLOCK, 0)
			if err != nil {
				t.Fatal(err)
			}
			tty := os.NewFile(uintptr(fds[0]), "pane")
			terminal := os.NewFile(uintptr(fds[1]), "terminal")
			t.Cleanup(func() { _ = tty.Close() })
			t.Cleanup(func() { _ = terminal.Close() })
			if _, err := terminal.WriteString(c.reply); err != nil {
				t.Fatal(err)
			}

			st := NewState(80, 24)
			if err := configureBackends(&st, tty, 80, 24, "auto", string(c.transport)); err != nil {
				t.Fatalf("configureBackends: %v", err)
			}

			want := []Backend{BackendBlock}
			if c.wantKitty {
				want = []Backend{BackendKitty, BackendBlock}
			}
			if !slices.Equal(st.Enabled, want) || st.Backend != want[0] {
				t.Fatalf("enabled = %v, active = %v, want %v with active %v", st.Enabled, st.Backend, want, want[0])
			}
			if got := ValidateForcedBackend(ModeKitty, st.Enabled) == nil; got != c.wantKitty {
				t.Fatalf("forced Kitty accepted = %v, want %v", got, c.wantKitty)
			}
			if st.cellPx != c.wantPx || st.kittyTransport != KittyTransportDirect {
				t.Fatalf("cell geometry = %+v, transport = %q, want %+v and direct", st.cellPx, st.kittyTransport, c.wantPx)
			}
			if st.kittyKeyboard || st.pointerShapeOK || st.paletteQueryOK {
				t.Fatal("graphics probe enabled an unrelated capability")
			}

			if err := terminal.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := unix.Shutdown(fds[0], unix.SHUT_WR); err != nil {
				t.Fatal(err)
			}
			got, err := io.ReadAll(terminal)
			if err != nil || string(got) != query {
				t.Fatalf("handshake = %q, err = %v, want %q", got, err, query)
			}
		})
	}
}
