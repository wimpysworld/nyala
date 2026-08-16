package engine

import (
	"testing"

	"github.com/chromedp/cdproto/runtime"
)

// TestParseLayoutShiftPayload proves the binding payload decode separates the
// install-time announce from a layout shift, and rejects a payload it does not own,
// including a malformed shift. The announce must be distinguishable, because it
// records the isolated execution context id and must never trigger a re-snapshot.
func TestParseLayoutShiftPayload(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		kind    string
		ok      bool
	}{
		{name: "announce", payload: "announce", kind: layoutShiftPayloadAnnounce, ok: true},
		{name: "shift", payload: "shift:0.25:7", kind: layoutShiftPayloadShift, ok: true},
		{name: "shift zero", payload: "shift:0:0", kind: layoutShiftPayloadShift, ok: true},
		{name: "empty", payload: ""},
		{name: "unknown prefix", payload: "other:1:2"},
		{name: "too few fields", payload: "shift:0.25"},
		{name: "too many fields", payload: "shift:0.25:7:9"},
		{name: "bad cumulative", payload: "shift:abc:7"},
		{name: "bad generation", payload: "shift:0.25:abc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, ok := parseLayoutShiftPayload(tt.payload)
			if ok != tt.ok || kind != tt.kind {
				t.Errorf("parseLayoutShiftPayload(%q) = (%q, %v), want (%q, %v)",
					tt.payload, kind, ok, tt.kind, tt.ok)
			}
		})
	}
}

// dispatchLayoutShiftPayload runs the real listener path for one payload: decode,
// then the engine's handler. It keeps the tests off a second copy of the switch.
func dispatchLayoutShiftPayload(e *ChromiumEngine, payload string, ctxID runtime.ExecutionContextID) {
	if kind, ok := parseLayoutShiftPayload(payload); ok {
		e.handleLayoutShiftPayload(kind, ctxID)
	}
}

// drainReflow reports whether a reflow poke is pending and clears it.
func drainReflow(e *ChromiumEngine) bool {
	select {
	case <-e.reflow:
		return true
	default:
		return false
	}
}

// TestLayoutShiftPayloadReflowPokes proves a shift payload reaches the reflow
// channel and an unknown payload does not. A payload the engine does not own that
// poked reflow would re-snapshot on another page's binding traffic.
func TestLayoutShiftPayloadReflowPokes(t *testing.T) {
	for _, tt := range []struct {
		payload string
		want    bool
	}{
		{payload: "nonsense", want: false},
		{payload: "shift:0.25:7", want: true},
	} {
		e := &ChromiumEngine{reflow: make(chan struct{}, 1)}
		dispatchLayoutShiftPayload(e, tt.payload, 11)
		if poked := drainReflow(e); poked != tt.want {
			t.Errorf("payload %q poked reflow = %v, want %v", tt.payload, poked, tt.want)
		}
	}
}

// TestAnnounceRecordsContextID proves the first announce records the isolated
// execution context id and pokes reflow once, so a cycle to Block on a quiet page
// draws text without waiting for an unrelated trigger, and proves a later announce
// replaces the stale id without a second poke, because it is not a layout shift.
func TestAnnounceRecordsContextID(t *testing.T) {
	e := &ChromiumEngine{reflow: make(chan struct{}, 1)}
	dispatchLayoutShiftPayload(e, "announce", 7)
	if e.layoutShiftCtxID != 7 {
		t.Errorf("layoutShiftCtxID = %d after first announce, want 7", e.layoutShiftCtxID)
	}
	if !drainReflow(e) {
		t.Error("first announce did not poke reflow, want one poke")
	}
	dispatchLayoutShiftPayload(e, "announce", 9)
	if e.layoutShiftCtxID != 9 {
		t.Errorf("layoutShiftCtxID = %d after second announce, want 9", e.layoutShiftCtxID)
	}
	if drainReflow(e) {
		t.Error("second announce poked reflow, want no poke")
	}
}

// TestTeardownSessionClearsLayoutShiftContext proves teardown drops the recorded
// isolated execution context id, so a rebuilt session never reads a dead world's id.
func TestTeardownSessionClearsLayoutShiftContext(t *testing.T) {
	e := &ChromiumEngine{layoutShiftCtxID: 42}
	e.teardownSession()
	if e.layoutShiftCtxID != 0 {
		t.Errorf("layoutShiftCtxID = %d after teardown, want 0", e.layoutShiftCtxID)
	}
}
