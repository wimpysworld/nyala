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
// from current lands on, skipping about:blank entries (the initial blank
// document Chromium seeds the session with), and ok=false when the step runs
// off either end of the entry URLs, so a Back on the launch page or a Forward
// on the last entry is a quiet no-op rather than a blank screen or a CDP
// error. A zero delta is not a history step and reports not-ok.
func historyTarget(current int64, urls []string, delta int) (int, bool) {
	if delta == 0 {
		return 0, false
	}
	for target := int(current) + delta; target >= 0 && target < len(urls); target += delta {
		if urls[target] != "about:blank" {
			return target, true
		}
	}
	return 0, false
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

// OpenURL navigates the page to url as a normal navigation, pushing a history
// entry, through the same raw Page.navigate path and bounded dispatch timeout
// NavHome uses, so a wedged browser drops the request instead of parking the
// caller. It resets the held scroll offset because the new document starts at
// the top. It never rewrites the held launch URL: NavHome permanently returns
// to the page Nyala was launched with.
func (e *ChromiumEngine) OpenURL(url string) {
	e.mu.Lock()
	ctx := e.ctx
	e.scrollX, e.scrollY, e.scrollYTarget = 0, 0, 0
	e.mu.Unlock()
	if ctx == nil {
		return
	}
	runCtx, cancel := context.WithTimeout(ctx, inputDispatchTimeout)
	defer cancel()
	_, _, _, _ = e.runNavigate(runCtx, url)
}

// CurrentURL returns the last committed main-frame URL, falling back to the
// launch URL before the first commit. watchLifecycle records commits and
// within-document navigation, so the value tracks SPA and fragment moves.
func (e *ChromiumEngine) CurrentURL() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.currentURL != "" {
		return e.currentURL
	}
	return e.url
}

// recordCurrentURL stores the committed main-frame URL CurrentURL reports.
// watchLifecycle calls it for main-frame Page.frameNavigated and
// Page.navigatedWithinDocument events; an empty URL is dropped so a sparse
// event never blanks the display back to the launch URL.
func (e *ChromiumEngine) recordCurrentURL(url string) {
	if url == "" {
		return
	}
	e.mu.Lock()
	e.currentURL = url
	e.mu.Unlock()
}

// CurrentTitle returns the last recorded page title. It is empty before the
// first title read and when the page has no title; the viewer owns the host
// fallback, so the engine never substitutes one.
func (e *ChromiumEngine) CurrentTitle() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.currentTitle
}

// recordCurrentTitle stores the page title CurrentTitle reports. An empty
// title is recorded as empty, because a page with no title must not keep a
// stale one on screen; the viewer supplies the host fallback.
func (e *ChromiumEngine) recordCurrentTitle(title string) {
	e.mu.Lock()
	e.currentTitle = title
	e.mu.Unlock()
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
		urls := make([]string, len(entries))
		for i, entry := range entries {
			urls[i] = entry.URL
		}
		target, ok := historyTarget(current, urls, delta)
		if !ok {
			return nil
		}
		return page.NavigateToHistoryEntry(entries[target].ID).Do(ctx)
	})
}
