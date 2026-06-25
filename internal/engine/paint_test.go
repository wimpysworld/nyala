package engine

import "testing"

// isClosed reports whether ch is closed without blocking.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// TestMarkPaintedClosesLatchOnce asserts markPainted closes the capture paint
// latch and stays idempotent: a second call (the navigate waiter completing after
// a crash rebuild re-registers it) does not double-close. The navigate waiter's
// matched load signal calls markPainted; this is the capture-side latch the driver
// waits on before counting usable frames.
func TestMarkPaintedClosesLatchOnce(t *testing.T) {
	e := &ChromiumEngine{painted: make(chan struct{})}

	e.markPainted()
	if !isClosed(e.painted) {
		t.Fatal("markPainted did not close the latch")
	}
	// A repeat must not panic on a double close.
	e.markPainted()
	if !isClosed(e.painted) {
		t.Fatal("latch reopened after a second markPainted")
	}
}

// TestMarkPaintedNoOpOffCapture asserts markPainted is a no-op when no latch is
// set, so the production path (no EnableCapture) pays nothing and does not panic.
func TestMarkPaintedNoOpOffCapture(t *testing.T) {
	e := &ChromiumEngine{}
	// Must not panic with a nil painted latch.
	e.markPainted()
}
