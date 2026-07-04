package engine

import (
	"strings"
	"testing"
)

// TestLayoutShiftScriptShape proves the injected observer script names the binding,
// observes the layout-shift entry type with buffered replay, thresholds on the
// epsilon, ignores input-driven shifts, and stores the observer for removal. These
// are the pieces the trigger relies on, so a drift in any one is caught without a
// browser.
func TestLayoutShiftScriptShape(t *testing.T) {
	script := buildLayoutShiftScript()
	wants := []string{
		layoutShiftBinding,
		layoutShiftObserverHandle,
		"PerformanceObserver",
		"'layout-shift'",
		"buffered:true",
		"hadRecentInput",
		"pending>0.01",
	}
	for _, w := range wants {
		if !strings.Contains(script, w) {
			t.Errorf("layout-shift script missing %q\nscript: %s", w, script)
		}
	}
}

// TestLayoutShiftRemovalScriptDisconnects proves the removal script reads the stored
// observer, disconnects it, and clears the handle, so a Tab from block to a graphics
// backend stops the observer.
func TestLayoutShiftRemovalScriptDisconnects(t *testing.T) {
	script := buildLayoutShiftRemovalScript()
	for _, w := range []string{layoutShiftObserverHandle, "disconnect()", "=undefined"} {
		if !strings.Contains(script, w) {
			t.Errorf("layout-shift removal script missing %q\nscript: %s", w, script)
		}
	}
}

// TestIsLayoutShiftBinding proves the listener predicate matches only the engine's
// own binding name, so a bindingCalled from any other page-registered binding does
// not poke a re-snapshot.
func TestIsLayoutShiftBinding(t *testing.T) {
	if !isLayoutShiftBinding(layoutShiftBinding) {
		t.Errorf("isLayoutShiftBinding(%q) = false, want true", layoutShiftBinding)
	}
	for _, other := range []string{"", "someOtherBinding", "layoutShift"} {
		if isLayoutShiftBinding(other) {
			t.Errorf("isLayoutShiftBinding(%q) = true, want false", other)
		}
	}
}

// TestPokeReflowFromLayoutShiftWakesChannel proves the layout-shift arm's poke lands
// on the reflow channel the snapshot goroutine debounces, so a matched bindingCalled
// drives one re-snapshot. It exercises pokeReflow, the same poke the arm calls, with
// the engine's session channel wired, mirroring how the networkAlmostIdle arm reaches
// the snapshot goroutine.
func TestPokeReflowFromLayoutShiftWakesChannel(t *testing.T) {
	e := &ChromiumEngine{reflow: make(chan struct{}, 1)}
	e.pokeReflow()
	select {
	case <-e.reflow:
	default:
		t.Fatal("layout-shift poke did not wake the reflow channel")
	}
}

// TestPokeReflowLatestWinsCoalesces proves a burst of layout-shift pokes collapses to
// one pending reflow wake, so a chatty page cannot overrun the capacity-1 channel and
// the event goroutine never blocks.
func TestPokeReflowLatestWinsCoalesces(t *testing.T) {
	e := &ChromiumEngine{reflow: make(chan struct{}, 1)}
	for range 5 {
		e.pokeReflow()
	}
	select {
	case <-e.reflow:
	default:
		t.Fatal("expected one pending reflow wake after a burst")
	}
	select {
	case <-e.reflow:
		t.Fatal("burst of pokes left more than one pending wake")
	default:
	}
}
