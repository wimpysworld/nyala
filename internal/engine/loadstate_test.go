package engine

import (
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
)

// TestTransitionLoadState drives the pure load-state machine through the
// milestone order, out-of-order events, the error case, and the teardown
// clear, without a browser.
func TestTransitionLoadState(t *testing.T) {
	t.Parallel()
	indeterminate := render.LoadState{State: render.LoadIndeterminate}
	fifty := render.LoadState{State: render.LoadPercentage, Percent: 50}
	seventyFive := render.LoadState{State: render.LoadPercentage, Percent: 75}
	errState := render.LoadState{State: render.LoadError}
	cases := []struct {
		name   string
		events []loadEvent
		want   render.LoadState
	}{
		{"commit resets to indeterminate", []loadEvent{loadEventCommit}, indeterminate},
		{"fcp sets fifty", []loadEvent{loadEventCommit, loadEventFCP}, fifty},
		{"dom ready sets seventy-five", []loadEvent{loadEventCommit, loadEventFCP, loadEventDOMReady}, seventyFive},
		{"load clears", []loadEvent{loadEventCommit, loadEventFCP, loadEventDOMReady, loadEventLoad}, render.LoadState{}},
		{"fcp after dom ready keeps seventy-five", []loadEvent{loadEventCommit, loadEventDOMReady, loadEventFCP}, seventyFive},
		{"fcp after load keeps clear", []loadEvent{loadEventCommit, loadEventDOMReady, loadEventLoad, loadEventFCP}, render.LoadState{}},
		{"milestone before commit keeps clear", []loadEvent{loadEventFCP}, render.LoadState{}},
		{"nav error sets error", []loadEvent{loadEventCommit, loadEventNavError}, errState},
		{"milestone after error keeps error", []loadEvent{loadEventNavError, loadEventFCP, loadEventDOMReady, loadEventLoad}, errState},
		{"commit after error resets to indeterminate", []loadEvent{loadEventNavError, loadEventCommit}, indeterminate},
		{"teardown clears error", []loadEvent{loadEventNavError, loadEventTeardown}, render.LoadState{}},
		{"teardown clears progress", []loadEvent{loadEventCommit, loadEventFCP, loadEventTeardown}, render.LoadState{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var s render.LoadState
			for _, ev := range tc.events {
				s = transitionLoadState(s, ev)
			}
			if s != tc.want {
				t.Fatalf("got %+v, want %+v", s, tc.want)
			}
		})
	}
}

// TestEngineLoadStateSnapshot verifies that applyLoadEvent updates the held
// state and that the LoadState getter returns it without waiting.
func TestEngineLoadStateSnapshot(t *testing.T) {
	t.Parallel()
	e := &ChromiumEngine{sessionActive: true}
	if got := e.LoadState(); got != (render.LoadState{}) {
		t.Fatalf("initial state: got %+v, want clear", got)
	}
	e.applyLoadEvent(loadEventCommit)
	e.applyLoadEvent(loadEventFCP)
	want := render.LoadState{State: render.LoadPercentage, Percent: 50}
	if got := e.LoadState(); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// TestApplyLoadEventSessionGate verifies that a lifecycle event queued past
// teardown leaves the cleared state alone, that a live session still applies
// events, and that the startup navigation error records after teardown.
func TestApplyLoadEventSessionGate(t *testing.T) {
	t.Parallel()
	fifty := render.LoadState{State: render.LoadPercentage, Percent: 50}
	cases := []struct {
		name   string
		active bool
		events []loadEvent
		want   render.LoadState
	}{
		{"late lifecycle event after teardown keeps clear", false, []loadEvent{loadEventCommit, loadEventFCP}, render.LoadState{}},
		{"live session applies events", true, []loadEvent{loadEventCommit, loadEventFCP}, fifty},
		{"nav error applies after teardown", false, []loadEvent{loadEventNavError}, render.LoadState{State: render.LoadError}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &ChromiumEngine{sessionActive: tc.active}
			for _, ev := range tc.events {
				e.applyLoadEvent(ev)
			}
			if got := e.LoadState(); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
