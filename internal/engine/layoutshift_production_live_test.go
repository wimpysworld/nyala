//go:build live

// This file proves the WW-223 production path end to end against a live
// Chromium. The WW-200 spike file next to it proves the design in an adapted
// copy; this one drives the shipped code: New plus Navigate on the block
// backend runs launchSession, which calls enableLayoutShift, which registers
// the world-scoped binding and installs the real buildLayoutShiftScript into
// the named isolated world. The test then asserts what the change is for: the
// page main world reports undefined for the binding, the observer handle, and
// the generation counter, both before and after a real layout shift, while the
// observer itself is demonstrably live in the isolated world and the engine
// holds the announced execution context id.
package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// productionLayoutShiftDeadline bounds every wait in this file. The passing
// path settles within a second or two, so a longer bound only decides how fast
// a regression reports itself.
const productionLayoutShiftDeadline = 30 * time.Second

// productionLayoutShiftPoll is the gap between retries of a bounded poll. It
// keeps the loops off a busy spin without adding a fixed sleep to the passing
// path.
const productionLayoutShiftPoll = 50 * time.Millisecond

// productionLayoutGenExpr reads the isolated world's generation counter, which
// only the observer script maintains. A number proves the real production
// script ran in the world; the value proves the observer saw shifts.
const productionLayoutGenExpr = "window." + layoutGenProperty

// startProductionLayoutShiftEngine launches the engine on the block backend
// against a local probe page and returns it with its chromedp context, ready
// for reads. It waits for a synced frame carrying crisp text, which is the
// readiness the WW-200 spike established: on-new-document scripts need the Page
// domain enabled, and a shift triggered before first paint records no
// layout-shift entry, so nothing may be triggered before this returns.
func startProductionLayoutShiftEngine(t *testing.T) (*ChromiumEngine, context.Context, *httptest.Server) {
	t.Helper()

	if _, err := resolveExecutable(""); err != nil {
		t.Skipf("no Chromium for the live test: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(layoutShiftProbePage))
	}))
	t.Cleanup(server.Close)

	e := New("")
	e.SetCellPx(8, 16)
	e.Resize(80, 24)
	if err := e.Navigate(server.URL); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	deadline := time.NewTimer(productionLayoutShiftDeadline)
	defer deadline.Stop()
	for {
		select {
		case frame, ok := <-e.Frames():
			if !ok {
				t.Fatal("frames channel closed before a text-bearing frame arrived")
			}
			if len(frame.Text) == 0 {
				continue
			}
		case <-deadline.C:
			t.Fatal("no synced frame with crisp text within the deadline")
		}
		break
	}

	e.mu.Lock()
	ctx := e.ctx
	e.mu.Unlock()
	if ctx == nil {
		t.Fatal("engine context is nil after navigate")
	}
	return e, ctx, server
}

// awaitProductionLayoutShiftContext waits for the engine to hold a non-zero
// layout-shift execution context id. That id can only come from the production
// bindingCalled handler decoding an announce payload, so it proves the
// world-scoped binding reached the isolated world and the observer script
// called it.
func awaitProductionLayoutShiftContext(t *testing.T, e *ChromiumEngine) runtime.ExecutionContextID {
	t.Helper()
	deadline := time.Now().Add(productionLayoutShiftDeadline)
	for {
		e.mu.Lock()
		ctxID := e.layoutShiftCtxID
		e.mu.Unlock()
		if ctxID != 0 {
			return ctxID
		}
		if time.Now().After(deadline) {
			t.Fatalf("engine held no layout-shift execution context id within %v; the announce never arrived", productionLayoutShiftDeadline)
		}
		time.Sleep(productionLayoutShiftPoll)
	}
}

// assertProductionPageWorldHidden evaluates the enumeration expression in the
// page main world and fails when any of the three observer names is reachable
// there, by 'in window', by getOwnPropertyNames, or by typeof.
func assertProductionPageWorldHidden(t *testing.T, ctx context.Context, phase string) {
	t.Helper()
	var raw string
	if err := chromedp.Run(ctx, chromedp.Evaluate(layoutShiftEnumerationExpr(), &raw)); err != nil {
		t.Fatalf("%s: page-world enumeration evaluate: %v", phase, err)
	}
	var enumeration layoutShiftEnumeration
	if err := json.Unmarshal([]byte(raw), &enumeration); err != nil {
		t.Fatalf("%s: decode enumeration result %q: %v", phase, raw, err)
	}
	if !enumeration.hidden() {
		t.Fatalf("%s: page world can reach the layout-shift observer state: %s", phase, raw)
	}
}

// readProductionLayoutGen reads the generation counter out of the isolated
// world through the execution context id the engine recorded, the same id the
// text walk and the removal script use.
func readProductionLayoutGen(t *testing.T, ctx context.Context, ctxID runtime.ExecutionContextID) float64 {
	t.Helper()
	var generation float64
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(runCtx context.Context) error {
		value, exception, err := runtime.Evaluate(productionLayoutGenExpr).
			WithContextID(ctxID).
			WithReturnByValue(true).
			Do(runCtx)
		if err != nil {
			return err
		}
		if exception != nil {
			t.Fatalf("isolated-world generation read threw: %s", exception.Text)
		}
		return json.Unmarshal(value.Value, &generation)
	}))
	if err != nil {
		t.Fatalf("read the generation counter in isolated context %d: %v", ctxID, err)
	}
	return generation
}

// triggerProductionLayoutShift inserts a tall block before the first body child
// in the page main world, which moves every following block down and raises a
// real layout-shift entry. It retries under the shared deadline because the
// insertion reports false until the document is complete.
func triggerProductionLayoutShift(t *testing.T, ctx context.Context) {
	t.Helper()
	deadline := time.Now().Add(productionLayoutShiftDeadline)
	for {
		var inserted bool
		err := chromedp.Run(ctx, chromedp.Evaluate(layoutShiftTriggerExpr, &inserted))
		if err == nil && inserted {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("could not insert the late block element within %v (last error: %v)", productionLayoutShiftDeadline, err)
		}
		time.Sleep(productionLayoutShiftPoll)
	}
}

// TestLiveProductionLayoutShiftHiddenFromPage drives the shipped enable path
// end to end and proves the WW-223 outcome. Navigating on the block backend
// runs the production enableLayoutShift, so the world-scoped binding and the
// real observer script install in the named isolated world. The test then
// proves three things in order: the engine records the announced isolated
// execution context id, so the binding really reached that world; the page main
// world reports undefined for window.__nyalaLayoutShift,
// window.__nyalaLayoutShiftObserver, and window.__nyalaLayoutGen, and cannot
// enumerate them; and after a real layout shift the isolated generation counter
// advances, which proves the observer is live rather than merely absent from
// the page, while the page world stays blind to all three names.
func TestLiveProductionLayoutShiftHiddenFromPage(t *testing.T) {
	e, ctx, _ := startProductionLayoutShiftEngine(t)

	ctxID := awaitProductionLayoutShiftContext(t, e)
	t.Logf("engine recorded layout-shift execution context id %d", ctxID)

	assertProductionPageWorldHidden(t, ctx, "after first paint")

	before := readProductionLayoutGen(t, ctx, ctxID)
	triggerProductionLayoutShift(t, ctx)

	deadline := time.Now().Add(productionLayoutShiftDeadline)
	var after float64
	for {
		after = readProductionLayoutGen(t, ctx, ctxID)
		if after > before {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("isolated generation counter stayed at %g within %v; the observer saw no layout shift", after, productionLayoutShiftDeadline)
		}
		time.Sleep(productionLayoutShiftPoll)
	}
	t.Logf("isolated generation counter advanced %g -> %g after a real layout shift", before, after)

	assertProductionPageWorldHidden(t, ctx, "after a real layout shift")
}
