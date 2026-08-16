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
// When forcing stays active but the current cell pair no longer matches the pair
// the registered stylesheet baked (a terminal font change moves Z with cellW), it
// refreshes the stylesheet so the forced line-height can never trail the grid.
//
// It keeps the state consistent on partial CDP failure: it stores an id only after a
// successful register, and clears the id only after a successful remove, so a failed
// call never leaves a phantom or stale id. On any error it returns the current active
// state rather than the desired one. It returns whether forcing is enabled after the
// reconcile.
func (e *ChromiumEngine) syncFontForcing(ctx context.Context) (enabled bool) {
	desired, active, cellW, cellH, stale := e.forcingReconcileInputs()
	if ctx == nil {
		return active
	}
	switch reconcileForcingAction(desired, active, stale) {
	case forcingEnable:
		return e.enableFontForcing(ctx, cellW, cellH)
	case forcingDisable:
		return e.disableFontForcing(ctx)
	case forcingRefresh:
		return e.refreshFontForcing(ctx, cellW, cellH)
	default:
		return active
	}
}

// forcingReconcileInputs snapshots the reconcile inputs under mu: the desired
// state (the active backend takes the overlay), the active state (a forcing
// script is registered), the current fractional CSS-pixel cell pair, and
// whether the registered stylesheet's baked pair is stale against it. It is
// the seam the stale-line-height unit test drives without a browser.
func (e *ChromiumEngine) forcingReconcileInputs() (desired, active bool, cellW, cellH float64, stale bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	desired = e.backend.UsesTextOverlay()
	active = e.fontForcingID != ""
	cellW, cellH = e.snapshotCellPx()
	stale = active && (cellW != e.forcedCellW || cellH != e.forcedCellH)
	return desired, active, cellW, cellH, stale
}

// forcingAction is the reconcile step syncFontForcing runs for one desired,
// active, stale triple.
type forcingAction int

const (
	// forcingKeep leaves the current state untouched.
	forcingKeep forcingAction = iota
	// forcingEnable registers the stylesheet on a fresh block enable.
	forcingEnable
	// forcingDisable removes the stylesheet on a switch off the block backend.
	forcingDisable
	// forcingRefresh replaces the active stylesheet whose baked cell pair went
	// stale, so the forced line-height tracks a cell-pair change mid-session.
	forcingRefresh
)

// reconcileForcingAction is the pure font-forcing reconcile decision. Enable
// and disable follow the desired and active states; a desired, active, stale
// triple refreshes, because the registered stylesheet bakes the cell pair and
// calibration rewrites only the font-size, so without the refresh a cell-pair
// change would leave rows stacking at the old line-height pitch while the walk
// snaps at the new one. Pure so the trigger is unit-tested without a browser.
func reconcileForcingAction(desired, active, stale bool) forcingAction {
	switch {
	case desired && !active:
		return forcingEnable
	case !desired && active:
		return forcingDisable
	case desired && stale:
		return forcingRefresh
	default:
		return forcingKeep
	}
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
	e.forcedCellW, e.forcedCellH = cellW, cellH
	e.mu.Unlock()
	// Apply to the live document too: the on-new-document script only fires for later
	// documents, so evaluate the same script now to force the current page.
	_ = evaluateVoid(ctx, "live font forcing", script, false)
	e.enableLayoutShift(ctx)
	return true
}

// refreshFontForcing replaces the registered font-forcing stylesheet with one
// rebuilt for the current cell pair, keeping forcing active across the swap. It
// registers the new on-new-document script first and removes the old one only
// after that succeeds, so a failed register leaves the old sheet standing (a
// stale pitch, retried on the next sync) rather than no forcing at all. A
// failed remove after a successful register leaves an orphan script for the
// session; its duplicate live style degrades placement on a later document
// until teardown, the same degrade-not-fail contract calibration follows. It
// then drops the live style and evaluates the rebuilt script so the current
// document reflows at the new pitch without a reload; the caller's
// recalibration corrects the font-size against the fresh sheet. The
// layout-shift observer is untouched: it bakes no cell-derived value.
func (e *ChromiumEngine) refreshFontForcing(ctx context.Context, cellW, cellH float64) (enabled bool) {
	e.mu.Lock()
	oldID := e.fontForcingID
	paintText := e.diagnosticTextPaint
	e.mu.Unlock()
	script := buildStylesheetScriptWithPaint(font.FiraCodeTTF(), cellW, cellH, paintText)
	var id page.ScriptIdentifier
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(runCtx context.Context) error {
		var err error
		id, err = page.AddScriptToEvaluateOnNewDocument(script).Do(runCtx)
		return err
	})); err != nil {
		return true
	}
	_ = chromedp.Run(ctx, page.RemoveScriptToEvaluateOnNewDocument(oldID))
	e.mu.Lock()
	e.fontForcingID = id
	e.forcedCellW, e.forcedCellH = cellW, cellH
	e.mu.Unlock()
	// Swap the live style too: drop the old sheet, then inject the rebuilt one so
	// the current document reflows at the new pitch without waiting for a reload.
	_ = evaluateVoid(ctx, "live style removal", buildStyleRemovalScript(), false)
	_ = evaluateVoid(ctx, "live font forcing", script, false)
	return true
}

// layoutShiftBindingParams builds the Runtime.addBinding command that exposes the
// layout-shift binding on the layoutShiftWorld isolated world alone, so the page
// can never call or enumerate it.
func layoutShiftBindingParams() *runtime.AddBindingParams {
	return runtime.AddBinding(layoutShiftBinding).WithExecutionContextName(layoutShiftWorld)
}

// layoutShiftScriptParams builds the Page.addScriptToEvaluateOnNewDocument command
// that installs the observer script into the layoutShiftWorld isolated world and
// runs it immediately, so the already-loaded document gets an observer without a
// page-world evaluate.
//
// Both builders are named rather than inline chains so the parameter tests drive
// the production registration and a dropped world name or runImmediately cannot
// pass unseen.
func layoutShiftScriptParams(script string) *page.AddScriptToEvaluateOnNewDocumentParams {
	return page.AddScriptToEvaluateOnNewDocument(script).
		WithWorldName(layoutShiftWorld).
		WithRunImmediately(true)
}

// enableLayoutShift registers the Runtime binding the layout-shift observer calls
// and registers the observer on-new-document script, both scoped to the
// layoutShiftWorld isolated world, so the page can never enumerate the observer
// state. The binding must exist before the observer script runs, so it is
// registered first; the observer guard bails when the binding is absent, and
// runImmediately runs the script at install time. runImmediately also covers the
// already-loaded document, so no separate live evaluate is needed, and a page-world
// evaluate would put the observer state back where the page can read it. A failed
// binding registration skips the observer, since a binding-less observer never fires;
// a successful binding then stores the observer script id so disableLayoutShift can
// remove it on a backend switch. Any CDP error degrades the trigger (a layout shift
// then relies on the other re-snapshot triggers) rather than the session, so errors
// are dropped after the binding step. It rides the font-forcing enable path, so the
// observer lives only while the block backend takes the overlay.
func (e *ChromiumEngine) enableLayoutShift(ctx context.Context) {
	if err := chromedp.Run(ctx,
		runtime.Enable(),
		layoutShiftBindingParams(),
	); err != nil {
		return
	}
	script := buildLayoutShiftScript()
	var id page.ScriptIdentifier
	if err := chromedp.Run(ctx, chromedp.ActionFunc(func(runCtx context.Context) error {
		var runErr error
		id, runErr = layoutShiftScriptParams(script).Do(runCtx)
		return runErr
	})); err == nil {
		e.mu.Lock()
		e.layoutShiftID = id
		e.mu.Unlock()
	}
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
	e.forcedCellW, e.forcedCellH = 0, 0
	e.mu.Unlock()
	// Drop the style from the live document too, so the current page renders unforced
	// without waiting for a reload.
	_ = evaluateVoid(ctx, "live style removal", buildStyleRemovalScript(), false)
	e.disableLayoutShift(ctx)
	return false
}

// disableLayoutShift removes the layout-shift observer on-new-document script,
// disconnects the live observer, and unsubscribes from the binding, so a cycle from
// the block backend to a graphics backend stops the layout-shift trigger. The live
// observer lives in the layoutShiftWorld isolated world, so the removal script runs
// against the held execution context id; a zero id (no observer announced itself)
// skips that step. It clears layoutShiftID only after the remove succeeds, so a
// failed remove leaves the id set and the state stays consistent. A missing script
// id (the binding registered but the observer script did not) skips the script
// remove and still disconnects the live observer and the binding. Errors are
// dropped: an orphaned observer on a graphics-backend page is harmless (the snapshot
// goroutine publishes nil there), so a failed cleanup degrades rather than fails.
func (e *ChromiumEngine) disableLayoutShift(ctx context.Context) {
	e.mu.Lock()
	id := e.layoutShiftID
	ctxID := e.layoutShiftCtxID
	e.mu.Unlock()
	if id != "" {
		if err := chromedp.Run(ctx, page.RemoveScriptToEvaluateOnNewDocument(id)); err == nil {
			e.mu.Lock()
			e.layoutShiftID = ""
			e.mu.Unlock()
		}
	}
	if ctxID != 0 {
		_ = chromedp.Run(ctx, chromedp.ActionFunc(func(runCtx context.Context) error {
			_, _, err := runtime.Evaluate(buildLayoutShiftRemovalScript()).
				WithContextID(ctxID).
				Do(runCtx)
			return err
		}))
	}
	_ = chromedp.Run(ctx, runtime.RemoveBinding(layoutShiftBinding))
	e.mu.Lock()
	e.layoutShiftCtxID = 0
	e.mu.Unlock()
}
