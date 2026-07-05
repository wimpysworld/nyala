package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"image/color"
	"math"
	"strings"

	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"github.com/wimpysworld/nyala/internal/render"
)

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
// sub-cell background recompose keep their geometry.
type walkRun struct {
	Text string     `json:"t"`
	Col  int        `json:"c"`
	Row  int        `json:"r"`
	X    float64    `json:"x"`
	Y    float64    `json:"y"`
	W    float64    `json:"w"`
	H    float64    `json:"h"`
	FG   walkColour `json:"fg"`
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
// forced to and the round is exact and Z-independent. For each text node it reads
// the parent element's computed style once and folds in the visibility, opacity,
// and clip-offscreen drops (the same drops the DOMSnapshot decode did on the
// client), so a hidden node emits no run. It takes per-line boxes from
// Range.getClientRects() and buckets the node's code units by their rounded row,
// so a soft-wrapped node yields one run per line with that line's own origin,
// which removes the per-glyph advance drift a single origin plus whole-cell
// stepping accumulates across a long run. It returns the current layout generation
// alongside the runs. The whole walk is one expression evaluated per decode.
func buildTextWalkScript(cellW, cellH float64) string {
	var b strings.Builder
	b.WriteString("(function(){")
	fmt.Fprintf(&b, "var cw=%g,ch=%g;", cellW, cellH)
	b.WriteString("var runs=[];")
	// hiddenStyle folds the client-side computed-style drops into the page: a
	// visibility of hidden or collapse, an effective opacity below the floor, or the
	// screen-reader-only clip-offscreen pattern (absolute or fixed positioning with a
	// collapsed clip rect or an inset(50%) clip-path). The opacity walks ancestors so
	// an ancestor opacity:0 zeroes a child whose own opacity is 1, matching the
	// blended text opacity the DOMSnapshot decode read.
	b.WriteString("function collapsed(clip){")
	b.WriteString("var m=/^rect\\((.+)\\)$/.exec(clip);if(!m)return false;")
	b.WriteString("var f=m[1].split(/[ ,]+/);if(f.length!==4)return false;")
	b.WriteString("function off(s){s=s.trim();if(s==='auto')return null;return parseFloat(s);}")
	b.WriteString("var t=off(f[0]),r=off(f[1]),bo=off(f[2]),l=off(f[3]);")
	b.WriteString("var hc=(r!==null&&l!==null&&r<=l);var vc=(bo!==null&&t!==null&&bo<=t);")
	b.WriteString("return hc||vc;}")
	b.WriteString("function hiddenStyle(el){")
	b.WriteString("var s=getComputedStyle(el);")
	b.WriteString("if(s.visibility==='hidden'||s.visibility==='collapse')return true;")
	b.WriteString("var pos=s.position;")
	b.WriteString("if((pos==='absolute'||pos==='fixed')&&(s.clipPath==='inset(50%)'||collapsed(s.clip)))return true;")
	b.WriteString("var op=1;for(var a=el;a;a=a.parentElement){var ao=parseFloat(getComputedStyle(a).opacity);if(!isNaN(ao))op*=ao;}")
	fmt.Fprintf(&b, "if(op<%g)return true;", hiddenOpacityEps)
	b.WriteString("return false;}")
	b.WriteString("function rgba(el){var c=getComputedStyle(el).color;")
	b.WriteString("var m=/rgba?\\(([^)]+)\\)/.exec(c);if(!m)return{r:0,g:0,b:0,a:255};")
	b.WriteString("var p=m[1].split(/[ ,\\/]+/).filter(function(x){return x.length;});")
	b.WriteString("var a=p.length>3?Math.round(Math.max(0,Math.min(1,parseFloat(p[3])))*255):255;")
	b.WriteString("return{r:parseInt(p[0])||0,g:parseInt(p[1])||0,b:parseInt(p[2])||0,a:a};}")
	// The tree walker visits every text node. A node with no non-whitespace content
	// or no parent element is skipped. rejectHidden drops a whole subtree whose parent
	// element is hidden, so the walk never descends into a hidden container.
	b.WriteString("var w=document.createTreeWalker(document.body||document.documentElement,NodeFilter.SHOW_TEXT,null);")
	b.WriteString("var node;while((node=w.nextNode())){")
	b.WriteString("var el=node.parentElement;if(!el)continue;")
	b.WriteString("if(!node.nodeValue||!node.nodeValue.trim())continue;")
	b.WriteString("if(hiddenStyle(el))continue;")
	b.WriteString("var fg=rgba(el);")
	// Bucket the node's code units by their rounded row, so a soft-wrapped node yields
	// one run per line box with that line's own origin. For each code unit a one-unit
	// range gives its client rect; the unit joins the line whose top rounds to the same
	// row. The origin (left, top, and the box geometry) tracks only non-whitespace
	// units, so a leading indent or a collapsed soft-wrap space never shifts the origin
	// right of the first inked glyph. All units are kept in order so inter-word spaces
	// survive; the joined line is then trimmed both ends, and because the origin already
	// points at the first inked unit the trimmed text and the snapped origin agree, so
	// the visible run lands glued to the paint. A unit whose rect is empty (a collapsed
	// soft-wrap space) is dropped.
	b.WriteString("var text=node.nodeValue;var lines={};var order=[];")
	b.WriteString("var rng=document.createRange();")
	b.WriteString("for(var i=0;i<text.length;i++){")
	b.WriteString("rng.setStart(node,i);rng.setEnd(node,i+1);")
	b.WriteString("var rc=rng.getClientRects();if(!rc.length)continue;")
	b.WriteString("var rr=rc[0];if(rr.width<=0&&rr.height<=0)continue;")
	b.WriteString("var row=Math.round(rr.top/ch);")
	b.WriteString("var ln=lines[row];")
	b.WriteString("if(!ln){ln={row:row,left:Infinity,top:Infinity,right:-Infinity,bottom:-Infinity,chars:[]};lines[row]=ln;order.push(row);}")
	b.WriteString("var ink=/\\S/.test(text[i]);")
	b.WriteString("if(ink){")
	b.WriteString("if(rr.left<ln.left)ln.left=rr.left;")
	b.WriteString("if(rr.top<ln.top)ln.top=rr.top;")
	b.WriteString("if(rr.left+rr.width>ln.right)ln.right=rr.left+rr.width;")
	b.WriteString("if(rr.top+rr.height>ln.bottom)ln.bottom=rr.top+rr.height;}")
	b.WriteString("ln.chars.push(text[i]);}")
	b.WriteString("for(var k=0;k<order.length;k++){var ln=lines[order[k]];")
	b.WriteString("if(!isFinite(ln.left))continue;")
	b.WriteString("var t=ln.chars.join('').replace(/\\s+/g,' ').replace(/^ | $/g,'');if(!t)continue;")
	b.WriteString("var h=ln.bottom-ln.top;")
	fmt.Fprintf(&b, "if(h<ch/4)continue;")
	b.WriteString("runs.push({t:t,c:Math.round(ln.left/cw),r:Math.round(ln.top/ch),x:ln.left,y:ln.top,w:ln.right-ln.left,h:h,fg:fg});}")
	b.WriteString("}")
	b.WriteString("var gen=(window." + layoutGenProperty + "||0);")
	b.WriteString("var de=document.documentElement||{};")
	b.WriteString("var sx=(window.pageXOffset||de.scrollLeft||0);")
	b.WriteString("var sy=(window.pageYOffset||de.scrollTop||0);")
	b.WriteString("return JSON.stringify({gen:gen,sx:sx,sy:sy,runs:runs});})();")
	return b.String()
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

// evaluateWalk runs the in-page text-walk expression against ctx and decodes the
// JSON result to a walkResult. The script returns a JSON string, so the evaluate
// returns it by value and this unmarshals it. A non-nil ExceptionDetails is an
// error even when the transport call succeeds, so a thrown walk surfaces as an
// error the caller holds against rather than a false empty result.
func evaluateWalk(ctx context.Context, expr string) (walkResult, error) {
	var out walkResult
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
		// unmarshals to a Go string first, then to the walkResult.
		var raw string
		if err := json.Unmarshal(res.Value, &raw); err != nil {
			return fmt.Errorf("engine: text walk decode envelope: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			return fmt.Errorf("engine: text walk decode result: %w", err)
		}
		return nil
	}))
	return out, err
}

// decodeWalkRuns maps the in-page walk result to crisp text runs without any
// browser dependency, so it is unit-testable on canned structs. The page already
// snapped each run's origin against the forced cell, so this trusts Col and Row and
// never divides a pixel by a cell width. It keeps only the grid right-edge clip:
// a run may run at most to the grid's right edge (cols), counting cells with the
// Stage 2 width rule so a width-2 cluster is never half-emitted. cellW converts the
// box right edge to a cell to cap the clip at the nearer of the box's own right edge
// and the grid, matching the old dual-edge clip so a font-metric residue never drops
// the last cell. A non-positive cellW falls back to the grid edge alone.
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
			Col:   r.Col,
			Row:   r.Row,
			Text:  clipped,
			FG:    color.RGBA{R: r.FG.R, G: r.FG.G, B: r.FG.B, A: r.FG.A},
			X:     r.X,
			Y:     r.Y,
			W:     r.W,
			H:     r.H,
			CellW: cellW,
		})
	}
	return runs
}
