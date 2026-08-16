package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/runtime"
)

// TestLayoutShiftWalkRequiresContextID proves the text walk refuses to evaluate
// without a held isolated-world execution context id. The layout generation lives
// on that world alone, so a page-world fallback would pair every run set to an
// undefined generation and quietly break the compose pairing rule. The guard must
// therefore return an error before any evaluate: the test passes a background
// context that carries no CDP executor, so a call that slipped past the guard
// would surface the transport's own failure instead of this message.
func TestLayoutShiftWalkRequiresContextID(t *testing.T) {
	const want = "engine: text walk has no layout-shift execution context"
	raw, err := evaluateWalkJSON(context.Background(), "1", 0)
	if err == nil {
		t.Fatalf("evaluateWalkJSON with a zero context id returned %q, want an error", raw)
	}
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	if raw != "" {
		t.Errorf("raw = %q, want empty on the guard path", raw)
	}

	// The same guard fronts the decoding wrapper, so a zero id can never reach a
	// decode either.
	res, err := evaluateWalk(context.Background(), "1", 0)
	if err == nil || err.Error() != want {
		t.Errorf("evaluateWalk error = %v, want %q", err, want)
	}
	if len(res.Runs) != 0 || res.Gen != 0 {
		t.Errorf("evaluateWalk returned %+v, want the zero result on the guard path", res)
	}
}

// TestLayoutShiftWalkNonZeroContextIDPassesGuard proves the guard is gated on the
// id alone: a non-zero id gets past it and fails later, in the transport, rather
// than on the missing-context message. Without this pairing the guard test above
// would still pass if the guard rejected every call.
func TestLayoutShiftWalkNonZeroContextIDPassesGuard(t *testing.T) {
	_, err := evaluateWalkJSON(context.Background(), "1", 7)
	if err == nil {
		t.Fatal("evaluateWalkJSON without a browser returned no error")
	}
	if strings.Contains(err.Error(), "no layout-shift execution context") {
		t.Errorf("a non-zero context id hit the zero-id guard: %v", err)
	}
}

// TestLayoutShiftWalkEvaluateCarriesContextID proves the evaluate the walk builds
// puts the held isolated-world id on the wire as contextId, so Chromium runs the
// walk in the world that holds the generation counter. It runs the production
// builder, walkEvaluateParams, against a recording executor, because chromedp.Run
// needs a real browser context; a dropped WithContextID therefore fails here.
// contextId is an omitzero field, so this also pins that a non-zero id survives
// marshalling.
func TestLayoutShiftWalkEvaluateCarriesContextID(t *testing.T) {
	const id runtime.ExecutionContextID = 7
	rec := &cdpCapture{}
	ctx := cdp.WithExecutor(context.Background(), rec)
	expr := buildTextWalkScript(8, 16)
	if _, _, err := walkEvaluateParams(expr, id).Do(ctx); err != nil {
		t.Fatalf("Runtime.evaluate: %v", err)
	}
	if rec.method != "Runtime.evaluate" {
		t.Errorf("method = %q, want Runtime.evaluate", rec.method)
	}
	if got, ok := rec.params["contextId"].(float64); !ok || runtime.ExecutionContextID(got) != id {
		t.Errorf("contextId = %v, want %d", rec.params["contextId"], id)
	}
	if got := rec.params["returnByValue"]; got != true {
		t.Errorf("returnByValue = %v, want true", got)
	}
	if got := rec.params["expression"]; got != expr {
		t.Error("expression is not the built walk script")
	}
}

// TestLayoutShiftWalkReadsHeldContextID proves textWalk reads the engine's held
// isolated-world id rather than a zero default, by driving it with the id unset:
// the call must fail on the walk's own guard, which is only reachable when the
// field is the value passed to the evaluate.
func TestLayoutShiftWalkReadsHeldContextID(t *testing.T) {
	e := &ChromiumEngine{}
	if _, err := e.textWalk(context.Background(), 8, 16); err == nil ||
		!strings.Contains(err.Error(), "no layout-shift execution context") {
		t.Errorf("textWalk with no held context id: err = %v, want the missing-context guard", err)
	}
}
