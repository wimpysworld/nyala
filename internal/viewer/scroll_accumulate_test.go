package viewer

import (
	"testing"
	"time"
)

// recordingEngine embeds the capture-test fake and records the scroll-key codes
// forwarded through SendKey, so a keySender test can assert a burst accumulates
// instead of collapsing to the last key.
type recordingEngine struct {
	*fakeEngine
	got chan string
}

func (e *recordingEngine) SendKey(code string) { e.got <- code }

// scrollStep maps a scroll-key code to a signed step: down is +1, up is -1, so a
// test can sum a burst and check up and down net (they sum, not max).
func scrollStep(code string) int {
	switch code {
	case "ArrowDown", "PageDown":
		return 1
	case "ArrowUp", "PageUp":
		return -1
	default:
		return 0
	}
}

func newRecordingSender(t *testing.T, buf int) (*keySender, *recordingEngine) {
	t.Helper()
	eng := &recordingEngine{fakeEngine: &fakeEngine{}, got: make(chan string, buf)}
	ks := newKeySender(eng)
	t.Cleanup(ks.close)
	return ks, eng
}

// collectSteps reads n forwarded codes with a deadline and returns their signed
// sum. A short read means the coalescer dropped part of the burst.
func collectSteps(t *testing.T, got <-chan string, n int) int {
	t.Helper()
	sum := 0
	deadline := time.After(2 * time.Second)
	for i := range n {
		select {
		case c := <-got:
			sum += scrollStep(c)
		case <-deadline:
			t.Fatalf("keySender forwarded %d codes, want %d (burst dropped)", i, n)
		}
	}
	return sum
}

// TestKeySenderAccumulatesBurst feeds a burst of scroll keys through the sender
// and asserts every one reaches the engine, so a rapid run of ArrowDown moves the
// page by the whole burst rather than a single step.
func TestKeySenderAccumulatesBurst(t *testing.T) {
	const n = 20
	ks, eng := newRecordingSender(t, n)
	for range n {
		ks.send("ArrowDown")
	}
	if sum := collectSteps(t, eng.got, n); sum != n {
		t.Fatalf("summed scroll = %d, want %d", sum, n)
	}
}

// TestKeySenderMixedBurstNets feeds an interleaved up/down burst and asserts the
// forwarded codes net to zero, so opposing scrolls in one burst sum, not collapse
// to the last key.
func TestKeySenderMixedBurstNets(t *testing.T) {
	const k = 8
	ks, eng := newRecordingSender(t, 2*k)
	for range k {
		ks.send("ArrowDown")
		ks.send("ArrowUp")
	}
	if sum := collectSteps(t, eng.got, 2*k); sum != 0 {
		t.Fatalf("mixed burst summed to %d, want 0", sum)
	}
}
