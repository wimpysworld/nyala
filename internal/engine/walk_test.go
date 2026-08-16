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

// TestWalkDecodeCarriesLinkFields proves the decode copies the walk's hr and li
// fields through to GlyphRun.Href and GlyphRun.LinkID untouched, and that a run
// with no anchor decodes to the zero values, so absence means no link.
func TestWalkDecodeCarriesLinkFields(t *testing.T) {
	const raw = `{"gen":3,"sx":0,"sy":0,"runs":[` +
		`{"t":"a link","c":0,"r":0,"x":0,"y":0,"w":48,"h":16,` +
		`"fg":{"r":0,"g":0,"b":0,"a":255},"bg":{"r":0,"g":0,"b":0,"a":0},"z":0,"f":false,` +
		`"hr":"https://example.org/page","li":"g3-a1"},` +
		`{"t":"plain","c":0,"r":1,"x":0,"y":16,"w":40,"h":16,` +
		`"fg":{"r":0,"g":0,"b":0,"a":255},"bg":{"r":0,"g":0,"b":0,"a":0},"z":1,"f":false}` +
		`],"fixed":[]}`
	res, err := decodeWalkResult(raw)
	if err != nil {
		t.Fatalf("decodeWalkResult: %v", err)
	}
	runs := decodeWalkRuns(res, 8, 80)
	if len(runs) != 2 {
		t.Fatalf("decoded %d runs, want 2", len(runs))
	}
	if runs[0].Href != "https://example.org/page" || runs[0].LinkID != "g3-a1" {
		t.Errorf("linked run = Href %q LinkID %q, want the canned values untouched",
			runs[0].Href, runs[0].LinkID)
	}
	if runs[1].Href != "" || runs[1].LinkID != "" {
		t.Errorf("plain run = Href %q LinkID %q, want both empty",
			runs[1].Href, runs[1].LinkID)
	}
}

// TestTextWalkScriptCarriesLinkResolution pins the in-page walk's link plumbing:
// the anchor climb, the emission of both run fields, and the per-anchor counter
// that keys the stable id on DOM identity.
func TestTextWalkScriptCarriesLinkResolution(t *testing.T) {
	script := buildTextWalkScript(8, 16)
	for _, want := range []string{
		"a.tagName==='A'&&a.hasAttribute('href')", // the anchor climb
		"run.hr=lk.hr;run.li=lk.li;",              // both field emissions
		"linkIds.set(a,id)",                       // the per-anchor counter map
		"'g'+gen+'-a'+(++linkN)",                  // the generation-prefixed id
		"v={hr:a.href,li:id};",                    // one a.href read per anchor
	} {
		if !strings.Contains(script, want) {
			t.Errorf("walk script lacks %q", want)
		}
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
