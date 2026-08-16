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
		// The generation counter is initialised to 0 and incremented on every
		// qualifying shift, so the snapshot goroutine reads a monotonic generation.
		layoutGenProperty + "=0",
		layoutGenProperty + "++",
	}
	for _, w := range wants {
		if !strings.Contains(script, w) {
			t.Errorf("layout-shift script missing %q\nscript: %s", w, script)
		}
	}
	// The counter bump must sit inside the per-entry loop (before the epsilon-gated
	// binding poke), so a sub-epsilon reflow that never pokes the binding still
	// advances the generation. The bump text must appear before the "pending>" gate.
	bump := strings.Index(script, layoutGenProperty+"++")
	gate := strings.Index(script, "pending>")
	if bump < 0 || gate < 0 || bump > gate {
		t.Errorf("generation bump must precede the epsilon gate; bump=%d gate=%d", bump, gate)
	}
}

// TestLayoutShiftScriptAnnouncesContext proves the observer script calls the
// binding once at install with the literal 'announce' payload, that the call is
// guarded to the top frame, and that the call sits after the binding guard. The
// announce call is the only route by which the engine learns the isolated world's
// execution context id, so a drift that drops it, renames the payload, or hoists it
// above the typeof guard would leave the held context id at zero and wedge every
// later text walk. The top-frame test matters too: the script installs in every
// frame, so an unguarded announce would let an iframe's world overwrite the id and
// send the text walk into a subframe document. The try/catch matters as well: a
// throwing binding at install time must not abort the observer setup that follows.
func TestLayoutShiftScriptAnnouncesContext(t *testing.T) {
	script := buildLayoutShiftScript()
	const announce = "try{if(window.top===window)fn('announce');}catch(err){}"
	if !strings.Contains(script, announce) {
		t.Errorf("layout-shift script missing the guarded announce call %q\nscript: %s", announce, script)
	}
	guard := strings.Index(script, "if(typeof fn!=='function')return;")
	call := strings.Index(script, announce)
	if guard < 0 {
		t.Fatalf("layout-shift script missing the binding guard\nscript: %s", script)
	}
	if call < guard {
		t.Errorf("announce must follow the binding guard; guard=%d announce=%d", guard, call)
	}
	// The observer install must come after the announce, so the context id is
	// known before any shift can be reported against it.
	if observe := strings.Index(script, "o.observe("); observe < 0 || observe < call {
		t.Errorf("announce must precede the observer install; announce=%d observe=%d", call, observe)
	}
}

// TestLayoutShiftScriptEncodesShiftPayload proves a qualifying shift is encoded as
// shift:<cumulative>:<generation>, the three-field form parseLayoutShiftPayload
// splits on. The generation is read from the isolated world's own window property
// in the same expression as the cumulative score, so the pair the engine receives
// describes one layout.
func TestLayoutShiftScriptEncodesShiftPayload(t *testing.T) {
	script := buildLayoutShiftScript()
	want := "fn('shift:'+String(cumulative)+':'+String(window." + layoutGenProperty + "))"
	if !strings.Contains(script, want) {
		t.Errorf("layout-shift script missing the shift payload %q\nscript: %s", want, script)
	}
	// The announce payload carries no colon, so the parser can never read it as a
	// shift; pin that the two payload forms stay distinguishable.
	if strings.Contains(script, "'announce:") {
		t.Errorf("announce payload must not take the shift colon form\nscript: %s", script)
	}
}

// TestLayoutShiftWorldName pins the isolated world name shared by the binding
// registration, the on-new-document script, and Page.createIsolatedWorld. The
// three routes address one world per frame only while they agree on this string,
// so it is asserted once rather than repeated as a literal in each caller's test.
func TestLayoutShiftWorldName(t *testing.T) {
	if layoutShiftWorld != "__nyala_world" {
		t.Errorf("layoutShiftWorld = %q, want %q", layoutShiftWorld, "__nyala_world")
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
