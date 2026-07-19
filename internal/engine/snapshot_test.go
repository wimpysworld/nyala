package engine

import (
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

// TestBuildTextWalkScriptContents pins the load-bearing fragments of the emitted
// walk script: it snaps the origin against the passed cell (round(left/cw)), reads
// per-line boxes from getClientRects, folds in the visibility and effective-opacity
// drops, resolves the computed color and the layout generation, and returns the
// scroll offset. A format drift that dropped one of these would silently change the
// overlay, so each fragment is asserted.
func TestBuildTextWalkScriptContents(t *testing.T) {
	script := buildTextWalkScript(8.1, 16.7)
	wants := []struct {
		desc, fragment string
	}{
		{"cell dims seeded", "var cw=8.1,ch=16.7;"},
		{"per-line client rects", "getClientRects()"},
		{"origin column snap against cell", "c:Math.round(ln.left/cw)"},
		{"origin row snap", "r:Math.round(ln.top/ch)"},
		{"bucket row snap", "var row=Math.round(rr.top/ch);"},
		{"visibility drop", "s.visibility==='hidden'"},
		{"clip-path inset drop", "clipPathHidden(s.clipPath)"},
		{"clip-path inset parse", "function clipPathHidden(cp){"},
		{"occlusion hit-test", "elementFromPoint("},
		{"occlusion predicate", "if(occluded(el,cx,cy))continue;"},
		{"1px overflow-hidden drop", "s.overflow==='hidden'&&parseFloat(s.width)<=1&&parseFloat(s.height)<=1"},
		{"effective-opacity drop", "if(op<0.05)return true;"},
		{"computed colour read", "gcs(el).color"},
		{"computed-style cache", "function gcs(el){"},
		{"layout generation read", "window." + layoutGenProperty},
		{"scroll offset read", "window.pageYOffset"},
		{"json envelope", "JSON.stringify("},
	}
	for _, w := range wants {
		if !strings.Contains(script, w.fragment) {
			t.Errorf("walk script missing %s: %q not found", w.desc, w.fragment)
		}
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
