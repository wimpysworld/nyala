//go:build !windows

package viewer

import (
	"errors"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
)

// TestFatalSignalsCoverTheKillingSignals pins the set the watcher registers.
// A signal left on its default disposition runs no deferred function, so any
// one missing here leaves raw mode, mouse reporting, and bracketed paste on
// and the shell reading escape sequences as typed input.
func TestFatalSignalsCoverTheKillingSignals(t *testing.T) {
	for _, want := range []os.Signal{syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM} {
		if !slices.Contains(fatalSignals, want) {
			t.Errorf("fatalSignals does not watch %v, so it would kill the terminal", want)
		}
	}
	// SIGKILL cannot be caught and SIGWINCH has its own watcher, so neither
	// belongs in the terminating set.
	for _, unwanted := range []os.Signal{syscall.SIGKILL, syscall.SIGWINCH} {
		if slices.Contains(fatalSignals, unwanted) {
			t.Errorf("fatalSignals watches %v, which is not a terminating signal it can handle", unwanted)
		}
	}
}

// TestSignalExitStatusIsOneTwentyEightPlusSignal pins the exit statuses a shell
// reads back. SIGTERM keeps the 143 the previous watcher returned, so that
// behaviour does not change, and the three new signals report their own
// conventional statuses rather than a shared one.
func TestSignalExitStatusIsOneTwentyEightPlusSignal(t *testing.T) {
	cases := []struct {
		sig  os.Signal
		want int
	}{
		{syscall.SIGHUP, 129},
		{syscall.SIGINT, 130},
		{syscall.SIGQUIT, 131},
		{syscall.SIGTERM, 143},
	}
	for _, c := range cases {
		if got := signalExitStatus(c.sig); got != c.want {
			t.Errorf("signalExitStatus(%v) = %d, want %d", c.sig, got, c.want)
		}
	}
	// A signal value carrying no number still reads as signalled, never as a
	// clean exit.
	if got := signalExitStatus(unnumberedSignal{}); got != 128 {
		t.Errorf("signalExitStatus on a numberless signal = %d, want the 128 base", got)
	}
}

func TestHandleFatalSignalRestoresClosesAndExitsInOrder(t *testing.T) {
	var calls []string
	exitStatus := 0
	handleFatalSignal(
		syscall.SIGTERM,
		func() { calls = append(calls, "restore") },
		func() error {
			calls = append(calls, "close")
			return errors.New("close failed")
		},
		func(status int) {
			calls = append(calls, "exit")
			exitStatus = status
		},
	)

	if want := []string{"restore", "close", "exit"}; !slices.Equal(calls, want) {
		t.Fatalf("fatal signal calls = %v, want %v", calls, want)
	}
	if exitStatus != 143 {
		t.Fatalf("fatal signal exit status = %d, want 143", exitStatus)
	}
}

func TestStopFatalSignalWatcherWaitsForSignalExit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fatal := make(chan os.Signal, 1)
		fatal <- syscall.SIGTERM
		allowClose := make(chan struct{})
		exited := make(chan int, 1)
		stop := watchFatalSignal(fatal, func() {}, func() error {
			<-allowClose
			return nil
		}, func(status int) { exited <- status })
		stopped := make(chan struct{})
		go func() {
			stop()
			close(stopped)
		}()

		// The consumer can reach stop when terminal restore closes its keys.
		// It must not return while the fatal handler waits for the engine.
		synctest.Wait()
		select {
		case <-stopped:
			t.Error("watcher stop returned before the fatal handler exited")
		default:
		}
		close(allowClose)
		<-stopped
		select {
		case status := <-exited:
			if status != 143 {
				t.Errorf("fatal signal exit status = %d, want 143", status)
			}
		default:
			t.Error("watcher stop returned without a fatal exit")
		}
	})
}

func TestStopFatalSignalWatcherWithoutSignal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fatal := make(chan os.Signal, 1)
		handled := false
		stop := watchFatalSignal(fatal, func() { handled = true }, func() error { return nil }, func(int) {})
		stop()
		if handled {
			t.Error("stopping the watcher without a signal started fatal teardown")
		}
	})
}

// unnumberedSignal is an os.Signal that is not a syscall.Signal, the case the
// exit-status fallback covers.
type unnumberedSignal struct{}

func (unnumberedSignal) String() string { return "unnumbered" }

func (unnumberedSignal) Signal() {}

// TestCatchingSIGINTLeavesCtrlCOnTheKeyPath guards the decision that made
// catching SIGINT safe: raw mode clears ISIG, so an interactive Ctrl+C reaches
// the reader as byte 0x03 and quits through the key path. Only an external
// kill -INT becomes a signal. If the byte ever stopped mapping to a quit, the
// terminal-restore watcher would be the only thing left handling Ctrl+C.
func TestCatchingSIGINTLeavesCtrlCOnTheKeyPath(t *testing.T) {
	if got := byteKeyAction(keyQuitCtrlC); got != actionQuit {
		t.Fatalf("byte 0x03 maps to %v, want actionQuit", got)
	}
	if !slices.Contains(fatalSignals, os.Signal(syscall.SIGINT)) {
		t.Fatal("SIGINT is unwatched, so an external kill -INT would wreck the terminal")
	}
}

// TestSignalTeardownResetsTheModesItEnabled pins what the watcher writes before
// it exits: the same exitTerminal sequence the deferred teardown writes, so a
// signalled exit turns off every mode enterTerminalModes turned on. The watcher
// composes no teardown of its own.
func TestSignalTeardownResetsTheModesItEnabled(t *testing.T) {
	for _, kitty := range []bool{false, true} {
		teardown := exitTerminal(24, true, kitty, true, false)
		for _, off := range []string{disableMouseReporting, disableBracketedPaste} {
			if !strings.Contains(teardown, off) {
				t.Errorf("the signal teardown (kitty=%v) does not emit %q", kitty, off)
			}
		}
		if kitty && !strings.Contains(teardown, disableKittyKeyboard) {
			t.Error("the signal teardown does not pop the kitty keyboard protocol it pushed")
		}
	}
}
