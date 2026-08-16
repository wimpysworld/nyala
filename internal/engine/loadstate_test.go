package engine

import "testing"

// TestTransitionLoadState drives the pure load-state machine through the
// milestone order, out-of-order events, the error case, and the teardown
// clear, without a browser.
func TestTransitionLoadState(t *testing.T) {
	t.Parallel()
	indeterminate := LoadState{State: LoadIndeterminate}
	fifty := LoadState{State: LoadPercentage, Percent: 50}
	seventyFive := LoadState{State: LoadPercentage, Percent: 75}
	errState := LoadState{State: LoadError}
	cases := []struct {
		name   string
		events []loadEvent
		want   LoadState
	}{
		{"commit resets to indeterminate", []loadEvent{loadEventCommit}, indeterminate},
		{"fcp sets fifty", []loadEvent{loadEventCommit, loadEventFCP}, fifty},
		{"dom ready sets seventy-five", []loadEvent{loadEventCommit, loadEventFCP, loadEventDOMReady}, seventyFive},
		{"load clears", []loadEvent{loadEventCommit, loadEventFCP, loadEventDOMReady, loadEventLoad}, LoadState{}},
		{"fcp after dom ready keeps seventy-five", []loadEvent{loadEventCommit, loadEventDOMReady, loadEventFCP}, seventyFive},
		{"fcp after load keeps clear", []loadEvent{loadEventCommit, loadEventDOMReady, loadEventLoad, loadEventFCP}, LoadState{}},
		{"milestone before commit keeps clear", []loadEvent{loadEventFCP}, LoadState{}},
		{"nav error sets error", []loadEvent{loadEventCommit, loadEventNavError}, errState},
		{"milestone after error keeps error", []loadEvent{loadEventNavError, loadEventFCP, loadEventDOMReady, loadEventLoad}, errState},
		{"commit after error resets to indeterminate", []loadEvent{loadEventNavError, loadEventCommit}, indeterminate},
		{"teardown clears error", []loadEvent{loadEventNavError, loadEventTeardown}, LoadState{}},
		{"teardown clears progress", []loadEvent{loadEventCommit, loadEventFCP, loadEventTeardown}, LoadState{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var s LoadState
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
	e := &ChromiumEngine{}
	if got := e.LoadState(); got != (LoadState{}) {
		t.Fatalf("initial state: got %+v, want clear", got)
	}
	e.applyLoadEvent(loadEventCommit)
	e.applyLoadEvent(loadEventFCP)
	want := LoadState{State: LoadPercentage, Percent: 50}
	if got := e.LoadState(); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}
