//go:build !windows

package viewer

import (
	"io"
	"os"
	"testing"
	"time"
)

func TestReadTerminalKeysEscapeTimeout(t *testing.T) {
	for _, prefix := range []string{"\x1b", "\x1b]", "\x1bP", "\x1b]1", "\x1bP1", "\x1bP>"} {
		t.Run(prefix, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			defer w.Close()
			keys, stop, err := readTerminalKeys(r)
			if err != nil {
				t.Fatal(err)
			}
			defer stop()

			writeTerminalTest(t, w, prefix)
			for _, want := range []byte(prefix) {
				if event := receiveTerminalTest(t, keys); event != (keyEvent{b: want}) {
					t.Fatalf("%q event = %+v, want %q", prefix, event, want)
				}
			}
			// The source stays open, and the peek timeout must not end the reader.
			writeTerminalTest(t, w, "r")
			if event := receiveTerminalTest(t, keys); event != (keyEvent{b: 'r'}) {
				t.Fatalf("event after Esc = %+v, want plain 'r'", event)
			}
		})
	}
}

func TestReadTerminalKeysStopsBeforeLaterInput(t *testing.T) {
	for _, prefix := range []string{"", "\x1b", "\x1b[200~partial", "\x1b]11;partial"} {
		t.Run(prefix, func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			defer w.Close()
			keys, stop, err := readTerminalKeys(r)
			if err != nil {
				t.Fatal(err)
			}
			// This byte confirms the reader has accepted the write before stop.
			writeTerminalTest(t, w, "q"+prefix)
			if event := receiveTerminalTest(t, keys); event != (keyEvent{b: 'q'}) {
				t.Fatalf("first event = %+v, want plain 'q'", event)
			}
			stopped := make(chan struct{})
			go func() {
				stop()
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("terminal reader did not stop while the source stayed open")
			}

			writeTerminalTest(t, w, "x")
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			left, err := io.ReadAll(r)
			if err != nil || string(left) != "x" {
				t.Fatalf("input after stop = %q, %v; want 'x'", left, err)
			}
		})
	}
}

func writeTerminalTest(t *testing.T, w *os.File, text string) {
	t.Helper()
	if _, err := io.WriteString(w, text); err != nil {
		t.Fatal(err)
	}
}

func receiveTerminalTest(t *testing.T, keys <-chan keyEvent) keyEvent {
	t.Helper()
	select {
	case event, ok := <-keys:
		if !ok {
			t.Fatal("terminal reader closed before delivering a key")
		}
		return event
	case <-time.After(time.Second):
		t.Fatal("terminal reader did not deliver a key")
		return keyEvent{}
	}
}
