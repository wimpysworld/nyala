package engine

import (
	"context"

	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// NavAction identifies one page navigation request forwarded from the viewer:
// a reload, a history step, or a normal navigation to the launch URL.
type NavAction int

const (
	// NavReload reloads the current document, like Ctrl+R or F5 in a desktop
	// browser.
	NavReload NavAction = iota
	// NavBack steps one entry back in the session history.
	NavBack
	// NavForward steps one entry forward in the session history.
	NavForward
	// NavHome navigates to the launch URL as a normal navigation, so it pushes
	// a history entry and NavBack returns to the prior page.
	NavHome
)

// navHistoryDelta maps a history-stepping NavAction to its entry offset. Any
// other action returns 0, meaning no history step.
func navHistoryDelta(a NavAction) int {
	switch a {
	case NavBack:
		return -1
	case NavForward:
		return 1
	default:
		return 0
	}
}

// historyTarget returns the navigation-history entry index a step of delta
// from current lands on, and ok=false when the step runs off either end of
// the count entries, so a Back on the first entry or a Forward on the last is
// a quiet no-op rather than a CDP error.
func historyTarget(current int64, count, delta int) (int, bool) {
	target := int(current) + delta
	if target < 0 || target >= count {
		return 0, false
	}
	return target, true
}

// SendNav forwards one navigation request to the page through the same bounded
// dispatch seam as keys and mouse events, so a wedged browser drops the
// request instead of parking the caller. NavHome re-navigates to the engine's
// held launch URL (recorded once by Navigate and never rewritten, so it is the
// page Nyala was launched with) through the same raw Page.navigate path
// startSession uses, and resets the held scroll offset because the new
// document starts at the top. Reload and the history steps keep the held
// offset: Chromium restores the scroll position for those itself, and the
// held offset only feeds crash recovery, which tolerates the same edge
// clamping a scroll at a document boundary already causes.
func (e *ChromiumEngine) SendNav(a NavAction) {
	e.mu.Lock()
	ctx := e.ctx
	url := e.url
	if a == NavHome {
		e.scrollX, e.scrollY, e.scrollYTarget = 0, 0, 0
	}
	e.mu.Unlock()
	if ctx == nil {
		return
	}
	if a == NavHome {
		runCtx, cancel := context.WithTimeout(ctx, inputDispatchTimeout)
		defer cancel()
		_, _, _, _ = e.runNavigate(runCtx, url)
		return
	}
	e.dispatchInput(ctx, navDispatchAction(a))
}

// navDispatchAction builds the chromedp action one non-home NavAction runs: a
// raw Page.reload, or a history step through Page.getNavigationHistory and
// Page.navigateToHistoryEntry. Neither waits for the page to load, matching
// the raw-navigate startup path, so the bounded dispatch returns as soon as
// the command is acknowledged.
func navDispatchAction(a NavAction) chromedp.Action {
	if a == NavReload {
		return chromedp.ActionFunc(func(ctx context.Context) error {
			return page.Reload().Do(ctx)
		})
	}
	delta := navHistoryDelta(a)
	return chromedp.ActionFunc(func(ctx context.Context) error {
		current, entries, err := page.GetNavigationHistory().Do(ctx)
		if err != nil {
			return err
		}
		target, ok := historyTarget(current, len(entries), delta)
		if !ok {
			return nil
		}
		return page.NavigateToHistoryEntry(entries[target].ID).Do(ctx)
	})
}
