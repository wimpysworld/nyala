package viewer

import (
	"bytes"
	"os"
	"testing"
)

// TestConfigureBackendsSkipsHandshakeUnderScreenAndHerdr asserts that under
// GNU screen and herdr configureBackends enables block only and runs no
// handshake at all. Both probes write their query batch to the tty as a first
// act, so a tty file left empty after the call proves neither ran. A non-mux
// baseline proves the same file path does carry a write when the handshake
// runs.
func TestConfigureBackendsSkipsHandshakeUnderScreenAndHerdr(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"screen", map[string]string{"STY": "1234.pts-0.host"}},
		{"herdr", map[string]string{"HERDR_ENV": "1", "HERDR_PANE_ID": "p1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearMuxEnv(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}

			tty := newTempTTY(t)
			st := NewState(80, 24)
			if err := configureBackends(&st, tty, 80, 24, "auto", "auto"); err != nil {
				t.Fatalf("configureBackends: %v", err)
			}

			if len(st.Enabled) != 1 || st.Enabled[0] != BackendBlock {
				t.Fatalf("enabled = %v, want [BackendBlock]", st.Enabled)
			}
			if n := ttyByteCount(t, tty); n != 0 {
				t.Fatalf("handshake wrote %d bytes; no probe must run", n)
			}
		})
	}
}

// TestConfigureBackendsReducedHandshakeUnderTmuxAndZellij asserts that under
// tmux and zellij configureBackends writes exactly the reduced query batch
// (no Kitty APC probe) and, with no replies on the temp-file tty, falls to
// block only: an unanswered DA1 must never enable Sixel.
func TestConfigureBackendsReducedHandshakeUnderTmuxAndZellij(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"tmux", map[string]string{"TMUX": "/tmp/tmux-1000/default,123,0"}},
		{"zellij", map[string]string{"ZELLIJ": "1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clearMuxEnv(t)
			for k, v := range c.env {
				t.Setenv(k, v)
			}

			tty := newTempTTY(t)
			st := NewState(80, 24)
			if err := configureBackends(&st, tty, 80, 24, "auto", "auto"); err != nil {
				t.Fatalf("configureBackends: %v", err)
			}

			if len(st.Enabled) != 1 || st.Enabled[0] != BackendBlock {
				t.Fatalf("enabled = %v, want [BackendBlock] with no replies", st.Enabled)
			}
			wrote := readTTYBytes(t, tty)
			if !bytes.Equal(wrote, reducedQueryBytes()) {
				t.Fatalf("handshake wrote %q, want the reduced batch %q", wrote, reducedQueryBytes())
			}
			if bytes.Contains(wrote, []byte("\x1b_G")) {
				t.Fatalf("reduced handshake carried a Kitty APC probe: %q", wrote)
			}
		})
	}
}

// TestConfigureBackendsRunsHandshakeWithoutMux is the skip-handshake baseline:
// with no mux env var set, configureBackends calls probeTerminal, which writes
// the query batch to the tty. A non-empty tty file proves the handshake ran, so
// the empty-file assertion under mux is meaningful, not vacuous.
func TestConfigureBackendsRunsHandshakeWithoutMux(t *testing.T) {
	clearMuxEnv(t)

	tty := newTempTTY(t)
	st := NewState(80, 24)
	if err := configureBackends(&st, tty, 80, 24, "auto", "auto"); err != nil {
		t.Fatalf("configureBackends: %v", err)
	}

	if n := ttyByteCount(t, tty); n == 0 {
		t.Fatalf("handshake wrote no bytes without mux; probeTerminal should run")
	}
}

// ttyByteCount returns the number of bytes written to the temp tty file.
func ttyByteCount(t *testing.T, f *os.File) int64 {
	t.Helper()
	info, err := f.Stat()
	if err != nil {
		t.Fatalf("stat tty: %v", err)
	}
	return info.Size()
}

// readTTYBytes returns everything written to the temp tty file.
func readTTYBytes(t *testing.T, f *os.File) []byte {
	t.Helper()
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read tty: %v", err)
	}
	return data
}
