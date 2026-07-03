package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
)

// fontCalibrationTimeout bounds one calibration pass. document.fonts.ready plus a
// probe measurement and a single re-inject must complete inside this deadline; on
// expiry the seed size stands and the session proceeds, so a wedged browser
// degrades placement rather than the session. It mirrors inputDispatchTimeout.
const fontCalibrationTimeout = 2 * time.Second

// calibrationProbeN is the grapheme count of the ASCII probe string. A run of N
// identical narrow glyphs measures the forced-face advance without depending on
// page content; N is large enough that the box-width residue per glyph averages
// out below sub-pixel noise.
const calibrationProbeN = 20

// buildCalibrationProbeScript returns the JS that appends an offscreen element of
// N repeated ASCII glyphs styled by the injected rules, reads its rendered box
// width in CSS pixels, removes it, and returns the width. The element inherits the
// forced font-family and seed font-size from the injected !important rules, so the
// width reflects the embedded face after document.fonts.ready. white-space:pre and
// the offscreen placement keep the probe unwrapped and invisible. The probe runs
// post-paint, so a root normally exists; if none does it returns 0, which the
// caller already treats as a failed measurement.
func buildCalibrationProbeScript(n int) string {
	probe := strings.Repeat("M", n)
	var b strings.Builder
	b.WriteString("(function(){var r=document.documentElement||document.body||document.head;")
	b.WriteString("if(!r)return 0;var p=document.createElement('span');")
	b.WriteString("p.style.cssText='position:absolute;left:-99999px;top:-99999px;white-space:pre;visibility:hidden';")
	b.WriteString("p.textContent=")
	b.WriteString(jsStringLiteral(probe))
	b.WriteString(";r.appendChild(p);")
	b.WriteString("var w=p.getBoundingClientRect().width;")
	b.WriteString("p.remove();return w;})();")
	return b.String()
}

// buildReinjectScript returns the JS that overwrites the injected <style>'s
// font-size rule with the corrected size. It rewrites the whole rule text so the
// corrected size wins with the same !important weight the seed had, leaving every
// other forced rule intact. A missing style element is a no-op, so a calibration
// race never throws.
func buildReinjectScript(sizePx float64) string {
	var b strings.Builder
	b.WriteString("(function(){var s=document.getElementById(")
	b.WriteString(jsStringLiteral(styleElementID))
	b.WriteString(");if(!s)return;s.textContent=s.textContent.replace(")
	b.WriteString("/font-size:[^;]*!important/,")
	b.WriteString(jsStringLiteral(fmt.Sprintf("font-size: %gpx !important", sizePx)))
	b.WriteString(");})();")
	return b.String()
}

// calibrateFonts corrects the seed font-size on the current document so N forced
// glyphs land on N cells. It awaits document.fonts.ready, measures an offscreen
// probe box through Runtime.evaluate, computes the corrected size with the pure
// calibratedFontSize, and re-injects it by rewriting the injected <style>. The
// whole pass runs under fontCalibrationTimeout; on timeout or any error the seed
// stands, a capture event records the miss when the recorder is active, and it
// returns so the caller proceeds to publish. Calibration failure degrades
// placement, never the session. It runs on the snapshot goroutine, so program
// order is the ordering contract; it holds no lock across the chromedp.Run.
func (e *ChromiumEngine) calibrateFonts(ctx context.Context) {
	if ctx == nil {
		return
	}
	e.mu.Lock()
	cellW, _ := e.snapshotCellPx()
	e.mu.Unlock()
	seed := cellW / advanceRatio

	runCtx, cancel := context.WithTimeout(ctx, fontCalibrationTimeout)
	defer cancel()

	if err := evaluateVoid(runCtx, "font readiness await", "document.fonts.ready", true); err != nil {
		e.recordCalibration()
		return
	}

	measured, err := evaluateFloat(runCtx, buildCalibrationProbeScript(calibrationProbeN))
	if err != nil || measured <= 0 {
		e.recordCalibration()
		return
	}

	corrected := calibratedFontSize(seed, calibrationProbeN, cellW, measured)
	if err := evaluateVoid(runCtx, "corrected-size reinject", buildReinjectScript(corrected), false); err != nil {
		e.recordCalibration()
		return
	}
}

// evaluateVoid runs a JS expression for its side effect and discards the result.
// A non-nil ExceptionDetails is an error even when the transport call succeeds, so
// a thrown expression or rejected awaited promise records the calibration miss and
// keeps the seed rather than reporting a false success. label names the step in the
// error. awaitPromise mirrors WithAwaitPromise for expressions that resolve a
// promise, so a rejection surfaces as an exception.
func evaluateVoid(ctx context.Context, label, expr string, awaitPromise bool) error {
	return chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		_, exc, evalErr := runtime.Evaluate(expr).WithAwaitPromise(awaitPromise).Do(c)
		if evalErr != nil {
			return evalErr
		}
		if exc != nil {
			return fmt.Errorf("engine: calibration %s threw: %s", label, exc.Text)
		}
		return nil
	}))
}

// evaluateFloat runs a JS expression that returns a number and decodes the
// ReturnByValue result to a float64. A non-numeric or empty result is an error, so
// the caller keeps the seed rather than trusting a malformed measurement.
func evaluateFloat(ctx context.Context, expr string) (float64, error) {
	var value float64
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		res, exc, evalErr := runtime.Evaluate(expr).WithReturnByValue(true).Do(c)
		if evalErr != nil {
			return evalErr
		}
		if exc != nil {
			return fmt.Errorf("engine: calibration probe threw: %s", exc.Text)
		}
		if res == nil || len(res.Value) == 0 {
			return fmt.Errorf("engine: calibration probe returned no value")
		}
		v, parseErr := strconv.ParseFloat(strings.TrimSpace(string(res.Value)), 64)
		if parseErr != nil {
			return fmt.Errorf("engine: calibration probe value %q: %w", string(res.Value), parseErr)
		}
		value = v
		return nil
	}))
	return value, err
}

// recordCalibration records one font-calibration capture event when the recorder
// is active, so a calibration miss is observable in the bundle. Production leaves
// the recorder nil, so the call pays only a nil check.
func (e *ChromiumEngine) recordCalibration() {
	if e.recorder != nil {
		e.recorder.Record(captureEvent{Type: captureEventFontCalibration})
	}
}
