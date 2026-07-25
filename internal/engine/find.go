package engine

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/chromedp/chromedp"
)

const findConfigPlaceholder = "__NYALA_FIND_CONFIG__"

//go:embed find.js
var findSource string

// findMaxMatches caps the matches one search collects and paints. A page with
// more matches than this reports the cap, because collecting every match on a
// pathological page would hold thousands of live ranges and paint highlights the
// reader can never step through.
const findMaxMatches = 2000

// findStateProperty is the page global the search keeps its match ranges and
// current index in. A navigation destroys it with the document, and the script
// re-collects when the held query no longer matches the page state, so a stale
// global can never survive a document change.
const findStateProperty = "__nyalaFind"

// findAllName and findCurrentName are the CSS custom highlight registrations the
// search paints through: every match in findAllName, the active one in
// findCurrentName. Chromium's compositor paints both, so they reach the terminal
// through ordinary screencast frames.
const (
	findAllName     = "nyala-find"
	findCurrentName = "nyala-find-current"
)

// findStyleID is the id of the injected <style> element carrying the
// ::highlight() rules, so a clear removes exactly the element the search added.
const findStyleID = "nyala-find-style"

// findMatchRGB and findCurrentRGB are the highlight background colours: amber
// for every match and orange for the active one, the desktop-browser
// convention. They are RGB triples rather than CSS strings so a test can count
// the painted pixels in a frame against the same values the page was given.
var (
	findMatchRGB   = [3]int{255, 214, 0}
	findCurrentRGB = [3]int{255, 122, 0}
)

// cssRGB formats an RGB triple as a CSS rgb() colour.
func cssRGB(c [3]int) string {
	return fmt.Sprintf("rgb(%d,%d,%d)", c[0], c[1], c[2])
}

// findOp is the operation one search dispatch runs in the page.
type findOp string

const (
	// findOpQuery searches for a new query and selects the first match.
	findOpQuery findOp = "query"
	// findOpStep moves to the next or previous match of the held query.
	findOpStep findOp = "step"
	// findOpClear removes the highlights and the page-side search state.
	findOpClear findOp = "clear"
)

// findConfig is the configuration one search dispatch hands the page script.
type findConfig struct {
	Op    findOp `json:"op"`
	Query string `json:"query"`
	// Step is +1 for the next match and -1 for the previous one, 0 for a query.
	Step int `json:"step"`
	// CellHeight is the CSS-pixel cell height a revealing scroll snaps to, 0
	// when the backend does not quantise scroll.
	CellHeight    float64 `json:"cellHeight"`
	Max           int     `json:"max"`
	StateProperty string  `json:"stateProperty"`
	StyleID       string  `json:"styleID"`
	AllName       string  `json:"allName"`
	CurrentName   string  `json:"currentName"`
	AllColour     string  `json:"allColour"`
	CurrentColour string  `json:"currentColour"`
	// Ink is the CSS colour the highlight paints the matched glyphs in:
	// transparent where the backend takes the crisp text overlay, so the page
	// paints no glyph ink the overlay would then draw over, and black
	// elsewhere, so the matched words stay legible on the highlight.
	Ink string `json:"ink"`
}

// findResult is the page script's return: the match count, the 1-based current
// match, and the scroll offset the page settled at, which the engine adopts as
// its held offset so a revealing scroll does not desync the wheel transport.
type findResult struct {
	Total   int     `json:"total"`
	Current int     `json:"current"`
	ScrollX float64 `json:"sx"`
	ScrollY float64 `json:"sy"`
}

// FindState is the page-search state snapshot the viewer renders. Reading it
// never waits on the browser: each dispatch records the state it reached, so a
// repaint always has a value to draw. Pending marks a dispatch in flight, with
// the previous counts still readable; Failed marks the last dispatch as dropped
// (a wedged browser hits the bounded dispatch deadline), with the last known
// counts kept, because the highlights the previous search painted are still on
// the page.
type FindState struct {
	// Query is the active search text, empty when no search is running.
	Query string
	// Total is the number of matches the page found, capped at findMaxMatches.
	Total int
	// Current is the 1-based index of the selected match, 0 when there is none.
	Current int
	// Pending reports a search or step dispatch in flight.
	Pending bool
	// Failed reports that the last dispatch did not land.
	Failed bool
}

// buildFindScript returns the page script for one search dispatch, with cfg
// substituted as its JSON configuration.
func buildFindScript(cfg findConfig) string {
	encoded, err := json.Marshal(cfg)
	if err != nil {
		return findSource
	}
	return strings.Replace(findSource, findConfigPlaceholder, string(encoded), 1)
}

// Find searches the page for query, highlights every match, and selects the
// first, scrolling it into view when it is off screen. Chromium owns the search:
// the page script drives window.find for the matching and the CSS custom
// highlight registrations for the painting, so the highlights reach the terminal
// through ordinary screencast frames. The page's DOM focus and selection survive
// the search, because the script restores the selection window.find borrowed and
// never moves focus. An empty query clears the search. The dispatch is bounded
// like every other input, so a wedged browser drops the search rather than
// parking the caller; FindState then reports it as failed.
func (e *ChromiumEngine) Find(query string) {
	e.dispatchFind(findOpQuery, query, 0)
}

// FindNext selects the next match of the active search, wrapping past the last
// match to the first. It is a no-op when no search is active.
func (e *ChromiumEngine) FindNext() {
	e.dispatchFind(findOpStep, "", 1)
}

// FindPrev selects the previous match of the active search, wrapping past the
// first match to the last. It is a no-op when no search is active.
func (e *ChromiumEngine) FindPrev() {
	e.dispatchFind(findOpStep, "", -1)
}

// FindClear removes the search highlights, the injected highlight stylesheet,
// and the page-side search state. It leaves the page's DOM focus and selection
// untouched, so page input after a clear reaches the element that held focus all
// along, and it leaves the page at the match the reader stopped on, the way
// closing a desktop browser's find bar does.
func (e *ChromiumEngine) FindClear() {
	e.dispatchFind(findOpClear, "", 0)
}

// FindState returns the current search-state snapshot. It takes only the state
// lock, so the viewer reads it on a repaint without ever waiting on Chromium.
func (e *ChromiumEngine) FindState() FindState {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.find
}

// resetFind drops the search state on a main-frame commit: the new document
// carries neither the highlights nor the page-side state, so a stale count must
// not survive the navigation.
func (e *ChromiumEngine) resetFind() {
	e.mu.Lock()
	e.find = FindState{}
	e.mu.Unlock()
}

// dispatchFind runs one search operation in the page through the same bounded
// dispatch seam as keys and mouse events, then records the state it reached. It
// marks the state pending before the dispatch, so a viewer repaint during a slow
// search sees a search in flight rather than a stale count presented as current.
func (e *ChromiumEngine) dispatchFind(op findOp, query string, step int) {
	e.mu.Lock()
	ctx := e.ctx
	cfg, run := e.findConfigLocked(op, query, step)
	e.mu.Unlock()
	if !run {
		return
	}
	if ctx == nil {
		e.applyFindResult(findResult{}, errNoSession)
		return
	}
	var res findResult
	err := e.runInput(ctx, chromedp.Evaluate(buildFindScript(cfg), &res))
	e.applyFindResult(res, err)
}

// errNoSession reports a dispatch with no live session, so the search state
// records the same failure a wedged browser produces rather than silently
// keeping a stale count.
var errNoSession = errors.New("engine: no live session")

// findConfigLocked records the state one search operation starts from and
// returns the page configuration for it, called with e.mu held. It reports
// run=false for a step with no active search, the one operation with nothing to
// dispatch. A query and a clear both dispatch: a clear must reach the page to
// remove the highlights even when the engine holds no search state.
func (e *ChromiumEngine) findConfigLocked(op findOp, query string, step int) (findConfig, bool) {
	if op == findOpQuery && query == "" {
		op = findOpClear
	}
	switch op {
	case findOpClear:
		e.find = FindState{Pending: true}
	case findOpStep:
		if e.find.Query == "" {
			return findConfig{}, false
		}
		query = e.find.Query
		e.find.Pending = true
	default:
		e.find = FindState{Query: query, Pending: true}
	}
	cellHeight := 0.0
	if e.backend.QuantisesScroll() {
		if _, cellH := e.snapshotCellPx(); cellH > 0 {
			cellHeight = cellH
		}
	}
	ink := "black"
	if e.backend.UsesTextOverlay() {
		ink = "transparent"
	}
	return findConfig{
		Op:            op,
		Query:         query,
		Step:          step,
		CellHeight:    cellHeight,
		Max:           findMaxMatches,
		StateProperty: findStateProperty,
		StyleID:       findStyleID,
		AllName:       findAllName,
		CurrentName:   findCurrentName,
		AllColour:     cssRGB(findMatchRGB),
		CurrentColour: cssRGB(findCurrentRGB),
		Ink:           ink,
	}, true
}

// applyFindResult folds one dispatch outcome into the search state. A failure
// keeps the last known counts and marks the state failed, because the previous
// search's highlights are still painted on the page. A success adopts the page's
// settled scroll offset as the engine's held offset, so a revealing scroll
// leaves the wheel transport, the Home key, and crash recovery aligned with
// where the page actually sits.
func (e *ChromiumEngine) applyFindResult(res findResult, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.find.Pending = false
	if err != nil {
		e.find.Failed = true
		return
	}
	e.find.Failed = false
	e.find.Total = res.Total
	e.find.Current = res.Current
	e.scrollX = int(math.Round(res.ScrollX))
	e.scrollY = int(math.Round(res.ScrollY))
	e.scrollYTarget = e.scrollY
}
