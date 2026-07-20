package engine

import (
	"context"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/wimpysworld/nyala/internal/render/font"
)

// syncFontForcing reconciles the font-forcing state to the active backend. Only the
// block backend folds the crisp text overlay onto its cell grid, so it forces the
// page onto the monospace grid; the graphics backends draw the bitmap alone and must
// render the page unforced. It reads the desired state (textOverlayBackend) and the
// active state (fontForcingID != "") under mu, then runs the CDP outside the lock,
// mirroring calibrateFonts.
//
// On a fresh enable it registers the on-new-document script (for later documents)
// and evaluates the same script against the live document, whose documentElement
// exists post-paint so the immediate-append branch runs. On a fresh disable it
// removes the on-new-document script by its id and evaluates the removal script to
// drop the live style. Runtime.evaluate ExceptionDetails are treated as errors, like
// evaluateVoid.
//
// It keeps the state consistent on partial CDP failure: it stores an id only after a
// successful register, and clears the id only after a successful remove, so a failed
// call never leaves a phantom or stale id. On any error it returns the current active
// state rather than the desired one. It returns whether forcing is enabled after the
// reconcile.
func (e *ChromiumEngine) syncFontForcing(ctx context.Context) (enabled bool) {
	e.mu.Lock()
	desired := e.backend.UsesTextOverlay()
	active := e.fontForcingID != ""
	cellW, cellH := e.snapshotCellPx()
	e.mu.Unlock()
	if ctx == nil || desired == active {
		return active
	}
	if desired {
		return e.enableFontForcing(ctx, cellW, cellH)
	}
	return e.disableFontForcing(ctx)
}

// fontForcingActive reports whether a font-forcing script is currently registered.
// The snapshot goroutine reads it before syncFontForcing to tell a fresh enable
// (disabled to enabled) from an already-active state, so it resets the calibration
// guard only on the transition. It takes mu because the field is written under mu.
func (e *ChromiumEngine) fontForcingActive() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.fontForcingID != ""
}

// enableFontForcing registers the font-forcing on-new-document script and applies it
// to the live document, then stores the returned id. It stores the id only after the
// register succeeds, so a failed register leaves fontForcingID empty and the state
// stays consistent. A failed live evaluate after a successful register keeps the id,
// since the script is registered for later documents; the live style just lands on
// the next document. It returns the active state after the attempt.
func (e *ChromiumEngine) enableFontForcing(ctx context.Context, cellW, cellH float64) (enabled bool) {
	e.mu.Lock()
	paintText := e.diagnosticTextPaint
	e.mu.Unlock()
	script := buildStylesheetScriptWithPaint(font.FiraCodeTTF(), cellW, cellH, paintText)
	var id page.ScriptIdentifier
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(runCtx context.Context) error {
		var err error
		id, err = page.AddScriptToEvaluateOnNewDocument(script).Do(runCtx)
		return err
	})); err != nil {
		return false
	}
	e.mu.Lock()
	e.fontForcingID = id
	e.mu.Unlock()
	// Apply to the live document too: the on-new-document script only fires for later
	// documents, so evaluate the same script now to force the current page.
	_ = evaluateVoid(ctx, "live font forcing", script, false)
	e.enableLayoutShift(ctx)
	return true
}

// enableLayoutShift registers the Runtime binding the layout-shift observer calls,
// registers the observer on-new-document script for later documents, and evaluates
// it against the live document so the current page is observed at once. The binding
// must exist before the observer script runs, so it is registered first. A failed
// binding registration skips the observer, since a binding-less observer never fires;
// a successful binding then stores the observer script id so disableLayoutShift can
// remove it on a backend switch. Any CDP error degrades the trigger (a layout shift
// then relies on the other re-snapshot triggers) rather than the session, so errors
// are dropped after the binding step. It rides the font-forcing enable path, so the
// observer lives only while the block backend takes the overlay.
func (e *ChromiumEngine) enableLayoutShift(ctx context.Context) {
	if err := chromedp.Run(ctx,
		runtime.Enable(),
		runtime.AddBinding(layoutShiftBinding),
	); err != nil {
		return
	}
	script := buildLayoutShiftScript()
	var id page.ScriptIdentifier
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(runCtx context.Context) error {
		var runErr error
		id, runErr = page.AddScriptToEvaluateOnNewDocument(script).Do(runCtx)
		return runErr
	})); err == nil {
		e.mu.Lock()
		e.layoutShiftID = id
		e.mu.Unlock()
	}
	// Observe the live document too: the on-new-document script only fires for later
	// documents, so evaluate the same script now to observe the current page.
	_ = evaluateVoid(ctx, "live layout-shift observer", script, false)
}

// disableFontForcing removes the font-forcing on-new-document script by its id and
// drops the live style. It clears fontForcingID only after the remove succeeds, so a
// failed remove leaves the id set and the state stays consistent. It returns the
// active state after the attempt.
func (e *ChromiumEngine) disableFontForcing(ctx context.Context) (enabled bool) {
	e.mu.Lock()
	id := e.fontForcingID
	e.mu.Unlock()
	if err := chromedp.Run(ctx, page.RemoveScriptToEvaluateOnNewDocument(id)); err != nil {
		return true
	}
	e.mu.Lock()
	e.fontForcingID = ""
	e.mu.Unlock()
	// Drop the style from the live document too, so the current page renders unforced
	// without waiting for a reload.
	_ = evaluateVoid(ctx, "live style removal", buildStyleRemovalScript(), false)
	e.disableLayoutShift(ctx)
	return false
}

// disableLayoutShift removes the layout-shift observer on-new-document script,
// disconnects the live observer, and unsubscribes from the binding, so a Tab from
// the block backend to a graphics backend stops the layout-shift trigger. It clears
// layoutShiftID only after the remove succeeds, so a failed remove leaves the id set
// and the state stays consistent. A missing id (the binding registered but the
// observer script did not) skips the script remove and still disconnects the live
// observer and the binding. Errors are dropped: an orphaned observer on a
// graphics-backend page is harmless (the snapshot goroutine publishes nil there), so
// a failed cleanup degrades rather than fails.
func (e *ChromiumEngine) disableLayoutShift(ctx context.Context) {
	e.mu.Lock()
	id := e.layoutShiftID
	e.mu.Unlock()
	if id != "" {
		if err := chromedp.Run(ctx, page.RemoveScriptToEvaluateOnNewDocument(id)); err == nil {
			e.mu.Lock()
			e.layoutShiftID = ""
			e.mu.Unlock()
		}
	}
	_ = evaluateVoid(ctx, "live layout-shift removal", buildLayoutShiftRemovalScript(), false)
	_ = chromedp.Run(ctx, runtime.RemoveBinding(layoutShiftBinding))
}
