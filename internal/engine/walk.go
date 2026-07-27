package engine

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"image/color"
	"math"
	"strings"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/wimpysworld/nyala/internal/render"
)

const textWalkConfigPlaceholder = "__NYALA_TEXT_WALK_CONFIG__"

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
// front-to-back stacking rank for the run, 0 being front-most.
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
}

// walkResult is the whole in-page walk return: the current layout generation, the
// DOM scroll offset the walk read at, and the per-line runs. The generation and
// scroll are read in the same evaluate as the walk, so the returned generation and
// the runs share one layout generation and composeFrame can pair a bitmap to the
// run set by that generation and shift it to the bitmap's own scroll.
type walkResult struct {
	Gen     int64     `json:"gen"`
	ScrollX float64   `json:"sx"`
	ScrollY float64   `json:"sy"`
	Runs    []walkRun `json:"runs"`
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
// sorts on the rank at the render seam. It returns the current layout generation
// alongside the runs. The whole walk is one expression evaluated per decode.
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
	runCtx, cancel := context.WithTimeout(ctx, snapshotReadTimeout)
	defer cancel()
	return evaluateWalk(runCtx, buildTextWalkScript(cellW, cellH))
}

// evaluateWalkJSON runs the in-page text-walk expression against ctx and returns
// the page's JSON verbatim, so a caller reading a field that walkResult does not
// model decodes the string itself rather than widening the shared type. A non-nil
// ExceptionDetails is an error even when the transport call succeeds, so a thrown
// walk surfaces as an error the caller holds against rather than a false empty
// result.
func evaluateWalkJSON(ctx context.Context, expr string) (string, error) {
	var raw string
	err := chromedp.Run(ctx, chromedp.ActionFunc(func(c context.Context) error {
		res, exc, evalErr := runtime.Evaluate(expr).WithReturnByValue(true).Do(c)
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
// returns it by value and this unmarshals it.
func evaluateWalk(ctx context.Context, expr string) (walkResult, error) {
	var out walkResult
	raw, err := evaluateWalkJSON(ctx, expr)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return out, fmt.Errorf("engine: text walk decode result: %w", err)
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
// stable sort does not care about.
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
			Col:          r.Col,
			Row:          r.Row,
			Text:         clipped,
			FG:           color.RGBA{R: r.FG.R, G: r.FG.G, B: r.FG.B, A: r.FG.A},
			BG:           color.RGBA{R: r.BG.R, G: r.BG.G, B: r.BG.B, A: r.BG.A},
			X:            r.X,
			Y:            r.Y,
			W:            r.W,
			H:            r.H,
			CellW:        cellW,
			DisplayCells: render.DisplayWidth(clipped),
			StackRank:    r.StackRank,
		})
	}
	return runs
}
