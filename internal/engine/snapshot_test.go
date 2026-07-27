package engine

import (
	"encoding/json"
	"fmt"
	"image/color"
	"strings"
	"testing"
)

// walkRunAt builds one walkRun at an integer cell origin the page snapped, with a
// box rect and a solid black foreground. It is the minimal shape decodeWalkRuns
// consumes, so a test drives the client decode without a browser.
func walkRunAt(text string, col, row int, x, w float64) walkRun {
	return walkRun{
		Text: text,
		Col:  col,
		Row:  row,
		X:    x,
		Y:    0,
		W:    w,
		H:    16,
		FG:   walkColour{R: 0, G: 0, B: 0, A: 255},
	}
}

// TestDecodeWalkRunsTrustsCells asserts that the client trusts
// the page's integer Col and Row and never re-divides a pixel by a cell width. A run
// the page placed at an arbitrary cell survives at exactly that cell, whatever its
// box X implies, so a client-side origin snap cannot reintroduce advance drift.
func TestDecodeWalkRunsTrustsCells(t *testing.T) {
	res := walkResult{Runs: []walkRun{walkRunAt("hello", 42, 7, 337.9, 40)}}
	runs := decodeWalkRuns(res, 8, 200)
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	if runs[0].Col != 42 || runs[0].Row != 7 {
		t.Fatalf("cell = (%d, %d), want (42, 7): the client must trust the page origin", runs[0].Col, runs[0].Row)
	}
	if runs[0].Text != "hello" {
		t.Fatalf("text = %q, want %q", runs[0].Text, "hello")
	}
}

// TestDecodeWalkRunsGridClip checks the surviving grid right-edge clip: a run whose
// box right edge or the grid edge is nearer caps the run there, counting cells with
// the decoded run width. The box width is a pure clip bound; a width-2 cluster past
// the budget drops whole rather than half-emitting.
func TestDecodeWalkRunsGridClip(t *testing.T) {
	cases := []struct {
		name    string
		run     walkRun
		cellW   float64
		cols    int
		want    string
		wantLen int
	}{
		{
			// Box 5*cellW wide from col 0, text 6 glyphs: box right edge round(40/8)=5
			// caps at 5 cells.
			name: "box right edge caps the run", run: walkRunAt("abcdef", 0, 0, 0, 40),
			cellW: 8, cols: 200, want: "abcd" + "e", wantLen: 1,
		},
		{
			// Box starts at col 118, far wider than the grid: the grid edge (cols 120)
			// caps the run to 120-118 = 2 cells.
			name: "grid edge caps the run", run: walkRunAt("abcdef", 118, 0, 944, 800),
			cellW: 8, cols: 120, want: "ab", wantLen: 1,
		},
		{
			// A width-2 cluster past the budget drops whole. Budget 2 cells: "A" fits,
			// "世" would make 3, so it drops.
			name: "wide cluster past budget drops whole", run: walkRunAt("A世", 0, 0, 0, 16),
			cellW: 8, cols: 200, want: "A", wantLen: 1,
		},
		{
			// Zero-width box clips to no cells, so the run is dropped.
			name: "zero-width box drops the run", run: walkRunAt("abc", 0, 0, 0, 0),
			cellW: 8, cols: 200, want: "", wantLen: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runs := decodeWalkRuns(walkResult{Runs: []walkRun{tc.run}}, tc.cellW, tc.cols)
			if len(runs) != tc.wantLen {
				t.Fatalf("want %d runs, got %d", tc.wantLen, len(runs))
			}
			if tc.wantLen == 1 && runs[0].Text != tc.want {
				t.Fatalf("clipped = %q, want %q", runs[0].Text, tc.want)
			}
		})
	}
}

// TestDecodeWalkRunsTrimsTrailingWhitespace checks a run whose text carries a
// soft-wrap trailing space or newline is trimmed before the clip, so the phantom
// whitespace never claims a cell.
func TestDecodeWalkRunsTrimsTrailingWhitespace(t *testing.T) {
	for _, tc := range []struct {
		name, text, want string
	}{
		{"trailing space", "hello ", "hello"},
		{"trailing newline", "developing\n", "developing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := walkResult{Runs: []walkRun{walkRunAt(tc.text, 0, 0, 0, 100)}}
			runs := decodeWalkRuns(res, 8, 200)
			if len(runs) != 1 {
				t.Fatalf("want 1 run, got %d", len(runs))
			}
			if runs[0].Text != tc.want {
				t.Fatalf("text = %q, want %q", runs[0].Text, tc.want)
			}
		})
	}
}

// TestDecodeWalkRunsColour checks the walk's RGBA channels map straight to
// GlyphRun.FG in RGBA order, no channel swap (the RGBA-vs-BGRA gotcha).
func TestDecodeWalkRunsColour(t *testing.T) {
	run := walkRunAt("A", 0, 0, 0, 16)
	run.FG = walkColour{R: 10, G: 20, B: 30, A: 128}
	runs := decodeWalkRuns(walkResult{Runs: []walkRun{run}}, 8, 200)
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	want := color.RGBA{R: 10, G: 20, B: 30, A: 128}
	if runs[0].FG != want {
		t.Fatalf("FG = %+v, want %+v", runs[0].FG, want)
	}
}

// TestDecodeWalkRunsBackground checks the walk's background RGBA channels map
// straight to GlyphRun.BG in RGBA order, no channel swap, so a consumer reading the
// resolved DOM background gets the true colour.
func TestDecodeWalkRunsBackground(t *testing.T) {
	run := walkRunAt("A", 0, 0, 0, 16)
	run.BG = walkColour{R: 240, G: 248, B: 255, A: 255}
	runs := decodeWalkRuns(walkResult{Runs: []walkRun{run}}, 8, 200)
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	want := color.RGBA{R: 240, G: 248, B: 255, A: 255}
	if runs[0].BG != want {
		t.Fatalf("BG = %+v, want %+v", runs[0].BG, want)
	}
}

// TestDecodeWalkRunsGuards checks the degenerate inputs return no runs rather than
// panicking: a non-positive column count, and an empty run set.
func TestDecodeWalkRunsGuards(t *testing.T) {
	if runs := decodeWalkRuns(walkResult{Runs: []walkRun{walkRunAt("A", 0, 0, 0, 16)}}, 8, 0); len(runs) != 0 {
		t.Fatalf("want no runs for zero cols, got %d", len(runs))
	}
	if runs := decodeWalkRuns(walkResult{}, 8, 200); len(runs) != 0 {
		t.Fatalf("want no runs for an empty walk, got %d", len(runs))
	}
}

// walkRunSpec is one run in a canned walk payload: the text, the snapped cell
// origin, the box rect, the resolved colours, and the front-to-back stacking rank
// the page emits per run. One spec renders both payload shapes, so a pair of
// payloads built from the same specs can differ only by the rank key.
type walkRunSpec struct {
	text       string
	col, row   int
	x, y, w, h float64
	fg, bg     walkColour
	rank       int
}

// walkPayload renders specs as the JSON the in-page walk returns. When withRank is
// true each run object carries the page's "z" stacking rank; when it is false the
// key is absent, which is the shape the walk returned before the rank existed.
func walkPayload(specs []walkRunSpec, withRank bool) string {
	objects := make([]string, 0, len(specs))
	for _, s := range specs {
		rank := ""
		if withRank {
			rank = fmt.Sprintf(`,"z":%d`, s.rank)
		}
		objects = append(objects, fmt.Sprintf(
			`{"t":%q,"c":%d,"r":%d,"x":%g,"y":%g,"w":%g,"h":%g,`+
				`"fg":{"r":%d,"g":%d,"b":%d,"a":%d},"bg":{"r":%d,"g":%d,"b":%d,"a":%d}%s}`,
			s.text, s.col, s.row, s.x, s.y, s.w, s.h,
			s.fg.R, s.fg.G, s.fg.B, s.fg.A,
			s.bg.R, s.bg.G, s.bg.B, s.bg.A, rank))
	}
	return fmt.Sprintf(`{"gen":9,"sx":0,"sy":240.5,"runs":[%s]}`, strings.Join(objects, ","))
}

// TestDecodeWalkRunsCarriesStackingRank proves the per-run stacking rank the page
// emits reaches render.GlyphRun on every run, in decode order, and that a payload
// with no rank key decodes to rank 0 throughout. Every other field stays identical
// between the ranked and the unranked payloads, so the rank rides along without
// disturbing the overlay until a consumer deliberately reads it. The runs carry
// differing text, geometry, and colours, so the equality assertion is meaningful
// rather than trivially empty. The ordering the rank encodes is decided in the page
// and is proved against real Chromium, so nothing here reimplements it.
func TestDecodeWalkRunsCarriesStackingRank(t *testing.T) {
	specs := []walkRunSpec{
		{
			text: "Headline", col: 0, row: 0, x: 0, y: 0, w: 64, h: 16,
			fg:   walkColour{R: 17, G: 17, B: 17, A: 255},
			bg:   walkColour{R: 255, G: 255, B: 255, A: 255},
			rank: 2,
		},
		{
			text: "modal copy", col: 4, row: 3, x: 32.4, y: 48, w: 80, h: 16,
			fg:   walkColour{R: 255, G: 255, B: 255, A: 255},
			bg:   walkColour{R: 32, G: 32, B: 48, A: 255},
			rank: 0,
		},
		{
			text: "世界 wide", col: 12, row: 7, x: 96, y: 112, w: 72, h: 16,
			fg:   walkColour{R: 0, G: 128, B: 64, A: 200},
			bg:   walkColour{R: 240, G: 248, B: 255, A: 255},
			rank: 3,
		},
		{
			text: "footer note", col: 30, row: 11, x: 240.9, y: 176, w: 88, h: 16,
			fg:   walkColour{R: 90, G: 90, B: 90, A: 255},
			bg:   walkColour{},
			rank: 1,
		},
	}
	ranked, plain := walkPayload(specs, true), walkPayload(specs, false)
	if ranked == plain {
		t.Fatalf("the two payloads are identical, so the ranked one carries no rank key")
	}
	var rankedResult, plainResult walkResult
	if err := json.Unmarshal([]byte(ranked), &rankedResult); err != nil {
		t.Fatalf("decode the ranked walk payload: %v", err)
	}
	if err := json.Unmarshal([]byte(plain), &plainResult); err != nil {
		t.Fatalf("decode the unranked walk payload: %v", err)
	}
	rankedRuns := decodeWalkRuns(rankedResult, 8, 120)
	plainRuns := decodeWalkRuns(plainResult, 8, 120)
	if len(rankedRuns) != len(specs) {
		t.Fatalf("want %d decoded runs, got %d: the fixture must exercise every run", len(specs), len(rankedRuns))
	}
	if len(rankedRuns) != len(plainRuns) {
		t.Fatalf("run counts differ: ranked %d, unranked %d", len(rankedRuns), len(plainRuns))
	}
	for i := range rankedRuns {
		if rankedRuns[i].StackRank != specs[i].rank {
			t.Fatalf("run %d StackRank = %d, want %d: the page rank must reach the render seam in decode order", i, rankedRuns[i].StackRank, specs[i].rank)
		}
		if plainRuns[i].StackRank != 0 {
			t.Fatalf("run %d StackRank = %d without a rank key, want 0", i, plainRuns[i].StackRank)
		}
		stripped := rankedRuns[i]
		stripped.StackRank = 0
		if stripped != plainRuns[i] {
			t.Fatalf("run %d differs beyond the rank: ranked %+v, unranked %+v: the stacking rank must change nothing else", i, rankedRuns[i], plainRuns[i])
		}
	}
}

// TestBuildTextWalkScriptContents pins the load-bearing fragments of the emitted
// walk script: it snaps the origin against the passed cell (round(left/cw)), reads
// per-line boxes from getClientRects, folds in the visibility and effective-opacity
// drops, collects the viewport-anchored boxes the compose-time shift re-tests
// against, resolves the computed color and the layout generation, and returns the
// scroll offset. A format drift that dropped one of these would silently change the
// overlay, so each fragment is asserted.
func TestBuildTextWalkScriptContents(t *testing.T) {
	script := buildTextWalkScript(8.1, 16.7)
	wants := []struct {
		desc, fragment string
	}{
		{"cell dims read from config", "var cw=config.cellWidth,ch=config.cellHeight;"},
		{"per-line client rects", "getClientRects()"},
		{"origin column snap against cell", "c:Math.round(ln.left/cw)"},
		{"origin row snap", "r:Math.round(ln.top/ch)"},
		{"bucket row snap", "var row=Math.round(rr.top/ch);"},
		{"visibility drop", "s.visibility==='hidden'"},
		{"clip-path inset drop", "clipPathHidden(s.clipPath)"},
		{"clip-path inset parse", "function clipPathHidden(cp){"},
		{"occlusion hit-test", "elementFromPoint("},
		{"occlusion predicate", "if(occluded(el,ln.left,ln.top,ln.right,ln.bottom))continue;"},
		{"occlusion single-point sample", "function coveredAt(el,x,y){"},
		{"occlusion three-sample span", "var xs=[l+inset,(l+r)/2,r-inset];"},
		{"occlusion null sample is no evidence", "if(cov===null)continue;"},
		{"occlusion keeps a run with any uncovered sample", "if(!cov)return false;"},
		{"occlusion LCA helper", "function lca(a,b){"},
		{"LCA-bounded background composite", "return bgOf(top,lca(top,el)).a>=255;"},
		{"bounded composite loop stop", "for(var a=el;a&&a!==stop;a=a.parentElement){"},
		{"bounded composite skips document fallback", "if(cov<0.999&&!stop){"},
		{"1px overflow-hidden drop", "s.overflow==='hidden'&&parseFloat(s.width)<=1&&parseFloat(s.height)<=1"},
		{"effective-opacity drop", "if(op<config.opacityThreshold)return true;"},
		{"fixed-box collector", "function collectFixed(el){"},
		{"fixed-box collection rides the text-node walk", "collectFixed(el);"},
		{"fixed-box collection cap", "var FIXED_CAP=32;"},
		{"fixed-box cap guard", "if(fixed.length>=FIXED_CAP)return;"},
		{"fixed-box ancestor counted once", "if(fixedCache.has(a))return;"},
		{"fixed-box position test", "if(s.position!=='fixed')continue;"},
		{"fixed-box negative z-index skip", "if(!isNaN(z)&&z<0)continue;"},
		{"fixed-box degenerate rect skip", "if(rc.width<=0||rc.height<=0)continue;"},
		{"fixed-box opaque flag", "o:bgOf(a,null).a>=255"},
		{"fixed boxes ride the json envelope", "fixed:fixed}"},
		{"computed colour read", "gcs(el).color"},
		{"computed-style cache", "function gcs(el){"},
		{"layout generation read", "window[config.generationProperty]"},
		{"scroll offset read", "window.pageYOffset"},
		{"json envelope", "JSON.stringify("},
	}
	for _, w := range wants {
		if !strings.Contains(script, w.fragment) {
			t.Errorf("walk script missing %s: %q not found", w.desc, w.fragment)
		}
	}
	if strings.Contains(script, "position==='sticky'") {
		t.Error("walk script collects sticky boxes: an unstuck sticky element scrolls with the page, so anchoring it would cull visible text")
	}
}

// TestBuildTextWalkScriptIsSingleExpression guards the CDP seam: each snapshot
// builds one self-contained expression for one runtime evaluation.
func TestBuildTextWalkScriptIsSingleExpression(t *testing.T) {
	script := buildTextWalkScript(8.1, 16.7)
	t.Logf("generated expression size: %d bytes", len(script))
	if !strings.HasPrefix(script, "(function(config){") || !strings.HasSuffix(script, ");\n") {
		t.Fatalf("walk script is not one self-contained IIFE: %q", script)
	}
	if strings.Count(script, "(function(config){") != 1 {
		t.Fatalf("walk script contains more than one outer IIFE")
	}
	if strings.Contains(script, textWalkConfigPlaceholder) {
		t.Fatalf("walk script still contains the config placeholder")
	}
}

// TestBuildTextWalkScriptEncodesConfiguration checks that configuration stays a
// JSON value even when the generation property contains JavaScript punctuation.
func TestBuildTextWalkScriptEncodesConfiguration(t *testing.T) {
	config := textWalkConfig{
		CellWidth:          8.125,
		CellHeight:         16.25,
		OpacityThreshold:   0.05,
		GenerationProperty: `x");globalThis.injected=true;//<`,
	}
	script := buildTextWalkScriptFromConfig(config)
	start := strings.LastIndex(script, "})(")
	if start < 0 || !strings.HasSuffix(script, ");\n") {
		t.Fatalf("walk script has no encoded configuration invocation")
	}
	encoded := script[start+3 : len(script)-3]
	var got textWalkConfig
	if err := json.Unmarshal([]byte(encoded), &got); err != nil {
		t.Fatalf("decode embedded config: %v", err)
	}
	if got != config {
		t.Fatalf("embedded config = %#v, want %#v", got, config)
	}
}

// TestBuildTextWalkScriptKeepsVisualFragmentsSeparate guards multi-column text
// nodes whose visual fragments share a snapped row. The whole-node range gives
// each visual fragment a stable identity. Word and code-unit rects must use that
// identity, rather than row changes, to keep adjacent words together without
// joining columns.
func TestBuildTextWalkScriptKeepsVisualFragmentsSeparate(t *testing.T) {
	script := buildTextWalkScript(8, 16)
	for _, fragment := range []string{
		"rng.selectNodeContents(node);",
		"var nodeRects=rng.getClientRects();",
		"function fragmentOf(rr){",
		"var lines=[];",
		"function lineAt(frag,row){",
		"var ln=lines.length?lines[lines.length-1]:null;",
		"if(!ln||ln.frag!==frag",
		"var frag=fragmentOf(rr);",
		"var ln=lineAt(frag,row);",
		"var ln=lineAt(wd.frag,wd.row);",
	} {
		if !strings.Contains(script, fragment) {
			t.Errorf("walk script missing visual fragment identity: %q", fragment)
		}
	}
	if strings.Contains(script, "if(!ln||ln.row!==row)") {
		t.Error("walk script still splits line fragments only when the snapped row changes")
	}
}
