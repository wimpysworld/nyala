package engine

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"image/color"
	"math"
	"slices"
	"strings"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/wimpysworld/nyala/internal/render"
)

const textWalkConfigPlaceholder = "__NYALA_TEXT_WALK_CONFIG__"

// walkFixedBoxCap mirrors the walk script's own FIXED_CAP and bounds the boxes
// the decode admits. The script runs in the page's main world and returns a
// string from its own JSON.stringify, so the page controls the result and the
// in-page cap is advice rather than a limit: overriding Array.prototype.push
// alone appends past it. Every box is re-tested against every run on the
// frame-emit goroutine, a scan no context can interrupt, so an unbounded box
// count makes each frame of a scroll quadratic. TestBuildTextWalkScriptContents
// asserts the script's cap against this constant, so the two cannot drift apart.
// The cap truncates in DOM text order, so the boxes it keeps need not be the ones
// that matter, but it bounds the coverage cull alone: whether a run holds still
// rides walkRun.Anchored, which no cap touches. A truncated box therefore loses a
// cover and lets text bleed through, never drops text the reader can see.
const walkFixedBoxCap = 32

//go:embed walk.js
var textWalkSource string

type textWalkConfig struct {
	CellWidth          float64 `json:"cellWidth"`
	CellHeight         float64 `json:"cellHeight"`
	OpacityThreshold   float64 `json:"opacityThreshold"`
	GenerationProperty string  `json:"generationProperty"`
}

// walkColour is one RGBA colour the in-page walk returns per run, each channel a
// 0..255 integer. The walk resolves the computed color in the page and folds the
// blended text opacity into the alpha, so the client never parses a CSS colour
// string.
type walkColour struct {
	R uint8 `json:"r"`
	G uint8 `json:"g"`
	B uint8 `json:"b"`
	A uint8 `json:"a"`
}

// walkRun is one line box the in-page walk returns: the run text, the integer
// cell origin the page computed against the forced cell (Col, Row), and the box
// rect in on-screen CSS pixels (scroll already subtracted by getClientRects). The
// page snaps the origin, so the client trusts Col and Row and never re-divides by
// a cell width. X, Y, W, H carry the rect so the paint-displacement metric and the
// sub-cell background recompose keep their geometry. StackRank carries the page's
// front-to-back stacking rank for the run, 0 being front-most. Anchored is the
// page's own answer to whether the run descends from a position: fixed element,
// which is a DOM relationship no client-side geometry can recover.
type walkRun struct {
	Text      string     `json:"t"`
	Col       int        `json:"c"`
	Row       int        `json:"r"`
	X         float64    `json:"x"`
	Y         float64    `json:"y"`
	W         float64    `json:"w"`
	H         float64    `json:"h"`
	FG        walkColour `json:"fg"`
	BG        walkColour `json:"bg"`
	StackRank int        `json:"z"`
	Anchored  bool       `json:"f"`
}

// walkFixedBox is one viewport-anchored position: fixed box the in-page walk
// reports, its edges on-screen CSS pixels and Opaque true when the element's own
// background reaches alpha 255. Ancestor backgrounds are excluded on purpose, so
// the opaque body nearly every page carries can never make a transparent bar read
// as a cover. Such a box holds its screen rect while the page scrolls, so the
// compose-time scroll shift re-tests occlusion against it on a shifted frame.
// Telling viewport-anchored text from flow text is not its job: walkRun.Anchored
// carries the DOM relationship the walk itself resolved, so a box collected here
// is only ever asked what it covers.
type walkFixedBox struct {
	Left   float64 `json:"l"`
	Top    float64 `json:"t"`
	Right  float64 `json:"r"`
	Bottom float64 `json:"b"`
	Opaque bool    `json:"o"`
}

// walkResult is the whole in-page walk return: the current layout generation, the
// DOM scroll offset the walk read at, the per-line runs, and the viewport-anchored
// boxes the runs were walked against. The generation and scroll are read in the
// same evaluate as the walk, so the returned generation and the runs share one
// layout generation and composeFrame can pair a bitmap to the run set by that
// generation and shift it to the bitmap's own scroll. The boxes travel with the
// runs because that shift needs them: an opaque box covers whatever flow text
// scrolls beneath it. Which runs hold still rides the runs themselves.
type walkResult struct {
	Gen     int64          `json:"gen"`
	ScrollX float64        `json:"sx"`
	ScrollY float64        `json:"sy"`
	Runs    []walkRun      `json:"runs"`
	Fixed   []walkFixedBox `json:"fixed"`
}

// buildTextWalkScript returns the JavaScript that walks visible text nodes in the
// page and returns integer cell positions per line box. It computes the origin
// column and row against cellW and cellH, the exact forced CSS cell the stylesheet
// and calibration establish, so the snap divisor equals the value the layout was
// forced to and the column round is exact and Z-independent. The row snap rounds
// the line-box top over the cell, so a browser-laid line at N*cellH snaps to row N
// with no residue, matching the forced line pitch. For each text node it reads
// the parent element's computed style once and folds in the visibility, opacity,
// and clip-offscreen drops (the same drops the DOMSnapshot decode did on the
// client), so a hidden node emits no run. A whole-node Range.getClientRects call
// gives each visual line fragment an ordered identity. The walk assigns each word
// or code-unit rect to that identity, so adjacent words in one fragment stay
// together while separate CSS columns cannot join even when every fragment snaps
// to the same row. This removes the per-glyph advance drift a single origin plus
// whole-cell stepping accumulates across a long run. The forced line-height
// puts every line box top at an integer multiple of the cell, so a word measured
// with one Range.getClientRects() call whose rects share a row buckets as a unit;
// only a word spanning more than one line box (a wrapped word, a spaceless CJK run,
// or vertical/ruby text) falls back to a per-code-unit measurement of the node.
// Before it pushes a line it hit-tests the line box at its start, centre, and
// end (an opaque-aware occlusion test) and culls the run only when every sample
// that hit an element is covered by an opaque background, so text behind a
// cookie banner, sticky header, or modal no longer leaks through the
// first-writer-wins overlay while a partly covered line keeps all its text.
// Each run also carries a
// front-to-back stacking rank the page derives from the stacking-context
// ancestry of the run's element and flattens to a dense integer over the run
// set, 0 being front-most (docs/ARCHITECTURE.md holds the ordering model); the
// returned run array still comes back in DOM order, and the Block overlay fold
// sorts on the rank at the render seam. Climbing the ancestors of the text it
// keeps, it tags each run that descends from a position: fixed element and records
// each such ancestor once as a viewport-anchored box, skipping a box with a
// negative z-index (which paints behind content) or a degenerate rect. The tag and
// the box are separate answers: the tag says what the compose-time shift must hold
// still, the box says what it may cull, so a skipped box never costs a hold.
// position: sticky is deliberately left out of both, because separating a stuck
// element from an unstuck one needs a second layout measurement. It returns the
// current layout generation alongside the runs and the boxes. The whole walk is
// one expression evaluated per decode.
func buildTextWalkScript(cellW, cellH float64) string {
	return buildTextWalkScriptFromConfig(textWalkConfig{
		CellWidth:          cellW,
		CellHeight:         cellH,
		OpacityThreshold:   hiddenOpacityEps,
		GenerationProperty: layoutGenProperty,
	})
}

func buildTextWalkScriptFromConfig(config textWalkConfig) string {
	encoded, err := json.Marshal(config)
	if err != nil {
		return textWalkSource
	}
	return strings.Replace(textWalkSource, textWalkConfigPlaceholder, string(encoded), 1)
}

// textWalk runs one in-page text walk under the snapshot read timeout, so a wedged
// layout read degrades to an error hold rather than stalling the snapshot goroutine.
// It builds the walk script for the CSS cell and evaluates it, returning the decoded
// walk result. It runs on the snapshot goroutine and holds no lock across the
// evaluate.
func (e *ChromiumEngine) textWalk(ctx context.Context, cellW, cellH float64) (walkResult, error) {
	e.mu.Lock()
	ctxID := e.layoutShiftCtxID
	e.mu.Unlock()
	runCtx, cancel := context.WithTimeout(ctx, snapshotReadTimeout)
	defer cancel()
	return evaluateWalk(runCtx, buildTextWalkScript(cellW, cellH), ctxID)
}

// evaluateWalkJSON runs the in-page text-walk expression against ctx and returns
// the page's JSON verbatim, so a caller reading a field that walkResult does not
// model decodes the string itself rather than widening the shared type. ctxID is
// the execution context of the layoutShiftWorld isolated world, where the layout
// generation lives, and the walk must run there because it reads that generation
// in the same evaluate as the runs. A zero ctxID is an error rather than a
// page-world evaluate: the page world holds no generation, so the walk would pair
// runs to an undefined generation and break the generation pairing rule. A stale
// id fails the same way, because Chromium answers "Cannot find context" and that
// evaluate error comes straight back. Both degrade through the snapshot error
// hold. A non-nil ExceptionDetails is an error even when the transport call
// succeeds, so a thrown walk surfaces as an error the caller holds against rather
// than a false empty result.
func evaluateWalkJSON(ctx context.Context, expr string, ctxID runtime.ExecutionContextID) (string, error) {
	if ctxID == 0 {
		return "", fmt.Errorf("engine: text walk has no layout-shift execution context")
	}
	var raw string
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		res, exc, evalErr := runtime.Evaluate(expr).WithReturnByValue(true).WithContextID(ctxID).Do(c)
		if evalErr != nil {
			return evalErr
		}
		if exc != nil {
			return fmt.Errorf("engine: text walk threw: %s", exc.Text)
		}
		if res == nil || len(res.Value) == 0 {
			return fmt.Errorf("engine: text walk returned no value")
		}
		// The value is a JSON-encoded string (the script's JSON.stringify), so it
		// unmarshals to a Go string and that string is the page's JSON.
		if err := json.Unmarshal(res.Value, &raw); err != nil {
			return fmt.Errorf("engine: text walk decode envelope: %w", err)
		}
		return nil
	}))
	if err != nil {
		return "", err
	}
	return raw, nil
}

// evaluateWalk runs the in-page text-walk expression against ctx and decodes the
// JSON result to a walkResult. The script returns a JSON string, so the evaluate
// returns it by value and this unmarshals it. ctxID names the layoutShiftWorld
// execution context the walk must run in, under the rules evaluateWalkJSON states.
func evaluateWalk(ctx context.Context, expr string, ctxID runtime.ExecutionContextID) (walkResult, error) {
	raw, err := evaluateWalkJSON(ctx, expr, ctxID)
	if err != nil {
		return walkResult{}, err
	}
	return decodeWalkResult(raw)
}

// decodeWalkResult unmarshals one walk JSON string to a walkResult and clamps the
// fixed boxes to walkFixedBoxCap. This is the single trust boundary where page
// JSON becomes Go data, so the clamp lives here rather than at a call site and no
// later consumer can miss it. An over-cap result is truncated rather than
// rejected: the in-page cap already drops boxes past the limit, so truncation is
// what a page exceeding it asks for, and rejecting would let a page suppress the
// whole text overlay by emitting one box too many, trading a slow frame for no
// text at all. Truncation keeps the excess boxes off the per-frame scan while
// leaving the overlay working, and it neither wedges the walk nor spams the
// caller with errors on every read.
func decodeWalkResult(raw string) (walkResult, error) {
	var out walkResult
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return walkResult{}, fmt.Errorf("engine: text walk decode result: %w", err)
	}
	if len(out.Fixed) > walkFixedBoxCap {
		// Copy rather than reslice: the published slice outlives the decode, so
		// resliced excess would hold the page's whole array until the next walk.
		out.Fixed = slices.Clone(out.Fixed[:walkFixedBoxCap])
	}
	return out, nil
}

// decodeWalkRuns maps the in-page walk result to crisp text runs without any
// browser dependency, so it is unit-testable on canned structs. The page already
// snapped each run's origin against the forced cell, so this trusts Col and Row and
// never divides a pixel by a cell width. It keeps only the grid right-edge clip:
// a run may run at most to the grid's right edge (cols), counting cells with the
// decoded run width so a width-2 cluster is never half-emitted. cellW converts the
// box right edge to a cell to cap the clip at the nearer of the box's own right edge
// and the grid, so a font-metric residue never drops
// the last cell. A non-positive cellW falls back to the grid edge alone. It copies
// the page's stacking rank through untouched: the decode's own drops (an empty text,
// a fully clipped run) leave the surviving ranks unique but no longer dense, which a
// stable sort does not care about. The viewport-anchored tag comes through untouched
// too, so the compose-time shift reads the page's own DOM answer rather than
// re-deriving one from the run's rectangle.
func decodeWalkRuns(res walkResult, cellW float64, cols int) []render.GlyphRun {
	if cols <= 0 {
		return nil
	}
	runs := make([]render.GlyphRun, 0, len(res.Runs))
	for _, r := range res.Runs {
		text := strings.TrimRight(r.Text, " \n")
		if text == "" {
			continue
		}
		edgeCol := cols
		if cellW > 0 {
			rightCol := int(math.Round((r.X + r.W) / cellW))
			if rightCol < edgeCol {
				edgeCol = rightCol
			}
		}
		clipped := render.ClipToCells(text, edgeCol-r.Col)
		if clipped == "" {
			continue
		}
		runs = append(runs, render.GlyphRun{
			Col:              r.Col,
			Row:              r.Row,
			Text:             clipped,
			FG:               color.RGBA{R: r.FG.R, G: r.FG.G, B: r.FG.B, A: r.FG.A},
			BG:               color.RGBA{R: r.BG.R, G: r.BG.G, B: r.BG.B, A: r.BG.A},
			X:                r.X,
			Y:                r.Y,
			W:                r.W,
			H:                r.H,
			CellW:            cellW,
			DisplayCells:     render.DisplayWidth(clipped),
			StackRank:        r.StackRank,
			ViewportAnchored: r.Anchored,
		})
	}
	return runs
}
