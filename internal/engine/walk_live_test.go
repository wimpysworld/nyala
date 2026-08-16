//go:build live

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/runtime"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/font"
)

const multiColumnText = "alpha bravo cider delta ember fable gamma hotel ivory joker karma lemon " +
	"mango noble ocean piano queen river solar tango ultra vivid water xenon " +
	"young zesty acorn birch coral denim eagle frost grape hazel inlet juniper " +
	"koala lilac maple north olive pearl quest robin stone tiger urban vocal " +
	"whale xray yield zebra amber beach cloud drift earth flame green honey"

// multiColumnTestPage keeps one text node in a four-row, two-column box. Its
// overflow creates five visual columns, each with four fragments.
const multiColumnTestPage = "data:text/html," +
	"<html><body style='margin:0'>" +
	"<p style='margin:0;width:352px;height:64px;column-count:2;column-fill:auto;column-gap:32px'>" +
	multiColumnText +
	"</p></body></html>"

// sameRowMultiColumnTestPage uses the same text in a one-row column box. Every
// visual fragment snaps to row zero, so row changes cannot identify columns.
const sameRowMultiColumnTestPage = "data:text/html," +
	"<html><body style='margin:0'>" +
	"<p style='margin:0;width:352px;height:16px;column-count:2;column-fill:auto;column-gap:32px'>" +
	multiColumnText +
	"</p></body></html>"

// TestLiveMultiColumnTextKeepsLineFragmentsSeparate checks a text node that
// returns to the same snapped rows in later CSS columns. It must return exactly
// five fragments on each of four rows. The exact count rejects both joined
// columns and per-word over-fragmentation.
func TestLiveMultiColumnTextKeepsLineFragmentsSeparate(t *testing.T) {
	res := liveTextWalk(t, multiColumnTestPage)
	assertFragmentGrid(t, res, 4, 5)
}

// TestLiveSameRowMultiColumnTextKeepsFragmentsSeparate checks the edge case
// where every visual column fragment shares one snapped row. Whole-node visual
// fragment identity must return twenty narrow runs on that row.
func TestLiveSameRowMultiColumnTextKeepsFragmentsSeparate(t *testing.T) {
	res := liveTextWalk(t, sameRowMultiColumnTestPage)
	assertFragmentGrid(t, res, 1, 20)
}

// liveWalkPage brings a real Chromium up on page, waits for the first synced frame
// so the page has laid out, injects the production forcing stylesheet, and waits
// for the embedded font. It returns the evaluate context and the forced CSS cell,
// so every live walk test reads the same forced monospace layout production does.
// It also returns the layoutShiftWorld execution context id the walk must run in.
// It skips the test when no Chromium resolves.
func liveWalkPage(t *testing.T, page string) (context.Context, runtime.ExecutionContextID, float64, float64) {
	t.Helper()
	if _, err := resolveExecutable(""); err != nil {
		t.Skipf("no Chromium for the live test: %v", err)
	}

	const cellW, cellH = 8.0, 16.0

	e := New("")
	e.SetCellPx(cellW, cellH)
	e.Resize(120, 24)
	if err := e.Navigate(page); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	select {
	case _, ok := <-e.Frames():
		if !ok {
			t.Fatal("frames channel closed before a frame arrived")
		}
	case <-deadline.C:
		t.Fatal("no synced frame within the deadline")
	}

	e.mu.Lock()
	ctx := e.ctx
	e.mu.Unlock()
	if ctx == nil {
		t.Fatal("engine context is nil after navigate")
	}
	// The announce arrives on its own bindingCalled event, so the first synced
	// frame does not imply the id is held yet. Poll for it, or the walk fails on
	// the missing-context guard whenever the event lands second.
	ctxID := awaitProductionLayoutShiftContext(t, e)

	styleScript := buildStylesheetScript(font.FiraCodeTTF(), cellW, cellH)
	if err := evaluateVoid(ctx, "inject forcing", styleScript, false); err != nil {
		t.Fatalf("inject forcing stylesheet: %v", err)
	}
	if err := evaluateVoid(ctx, "fonts ready", "document.fonts.ready", true); err != nil {
		t.Fatalf("await fonts ready: %v", err)
	}
	return ctx, ctxID, cellW, cellH
}

func liveTextWalk(t *testing.T, page string) walkResult {
	t.Helper()
	ctx, ctxID, cellW, cellH := liveWalkPage(t, page)
	res, err := evaluateWalk(ctx, buildTextWalkScript(cellW, cellH), ctxID)
	if err != nil {
		t.Fatalf("text walk: %v", err)
	}
	return res
}

func assertFragmentGrid(t *testing.T, res walkResult, wantRows, wantPerRow int) {
	t.Helper()
	if wantRuns := wantRows * wantPerRow; len(res.Runs) != wantRuns {
		t.Fatalf("runs = %d, want %d", len(res.Runs), wantRuns)
	}
	rows := make(map[int]int)
	for _, run := range res.Runs {
		rows[run.Row]++
		if run.W > 160 {
			t.Errorf("run %q width = %.1fpx, want one column fragment at most 160px", run.Text, run.W)
		}
	}
	if len(rows) != wantRows {
		t.Errorf("rows = %d, want %d: %+v", len(rows), wantRows, rows)
	}
	for row, count := range rows {
		if count != wantPerRow {
			t.Errorf("row %d fragments = %d, want %d", row, count, wantPerRow)
		}
	}
}

// srOnlyTestPage carries three text nodes: two screen-reader-only hidden variants
// and one genuinely visible narrow element. The first hidden div uses the GitHub
// Primer .sr-only idiom (1px-square, overflow-hidden, clip-path normalised to
// inset(0px 100% 100% 0px)); the second uses the literal clip-path:inset(50%). The
// visible <p> holds short text with normal overflow that the walk must keep.
const srOnlyTestPage = "data:text/html," +
	"<html><body style='margin:0'>" +
	"<div style='position:absolute;width:1px;height:1px;overflow:hidden;clip-path:inset(0px 100% 100% 0px)'>screen reader only description</div>" +
	"<div style='position:absolute;clip-path:inset(50%)'>second hidden phrase</div>" +
	"<p>keepme</p>" +
	"</body></html>"

// TestLiveScreenReaderOnlyTextDropped verifies that the block backend omits
// screen-reader-only text from overlay runs. It launches a real
// Chromium, navigates the srOnly page, injects the production forcing stylesheet,
// waits for the font to load, then runs the in-page text walk and asserts both
// hidden variants emit no run while the visible narrow text survives. The live tag
// keeps it off `just test` and CI; run it with `just live`.
func TestLiveScreenReaderOnlyTextDropped(t *testing.T) {
	if _, err := resolveExecutable(""); err != nil {
		t.Skipf("no Chromium for the live test: %v", err)
	}

	const cellW, cellH = 8.0, 16.0

	e := New("")
	e.SetCellPx(cellW, cellH)
	e.Resize(80, 24)
	if err := e.Navigate(srOnlyTestPage); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	// Wait for the first synced frame so the page has laid out before the walk.
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	select {
	case _, ok := <-e.Frames():
		if !ok {
			t.Fatal("frames channel closed before a frame arrived")
		}
	case <-deadline.C:
		t.Fatal("no synced frame within the deadline")
	}

	e.mu.Lock()
	ctx := e.ctx
	e.mu.Unlock()
	if ctx == nil {
		t.Fatal("engine context is nil after navigate")
	}
	// The announce arrives on its own bindingCalled event, so the first synced
	// frame does not imply the id is held yet. Poll for it, or the walk fails on
	// the missing-context guard whenever the event lands second.
	ctxID := awaitProductionLayoutShiftContext(t, e)

	// Inject the production forcing stylesheet and wait for the embedded font, so the
	// walk reads the forced monospace layout exactly as production does.
	styleScript := buildStylesheetScript(font.FiraCodeTTF(), cellW, cellH)
	if err := evaluateVoid(ctx, "inject forcing", styleScript, false); err != nil {
		t.Fatalf("inject forcing stylesheet: %v", err)
	}
	if err := evaluateVoid(ctx, "fonts ready", "document.fonts.ready", true); err != nil {
		t.Fatalf("await fonts ready: %v", err)
	}

	res, err := evaluateWalk(ctx, buildTextWalkScript(cellW, cellH), ctxID)
	if err != nil {
		t.Fatalf("text walk: %v", err)
	}

	var joined strings.Builder
	visibleFound := false
	for _, r := range res.Runs {
		joined.WriteString(r.Text)
		joined.WriteString("\n")
		if strings.Contains(r.Text, "keepme") {
			visibleFound = true
		}
	}
	all := joined.String()

	if strings.Contains(all, "screen reader only") || strings.Contains(all, "reader") {
		t.Errorf("clip-path:inset(0px 100%% 100%% 0px) sr-only text leaked into runs:\n%s", all)
	}
	if strings.Contains(all, "second hidden") || strings.Contains(all, "hidden phrase") {
		t.Errorf("clip-path:inset(50%%) sr-only text leaked into runs:\n%s", all)
	}
	if !visibleFound {
		t.Errorf("visible narrow text 'keepme' was dropped; runs:\n%s", all)
	}
}

// occlusionTestPage carries seven text nodes in a deterministic 640x384 CSS viewport
// (grid 80x24 at cell 8x16, device scale 1). An opaque position:fixed banner at the
// bottom (background rgb(0,0,0), its own text "acceptcookies") covers the absolute
// "behindtext". A translucent position:fixed scrim (rgba(0,0,0,0.3)) covers the
// absolute "underscrim". "visibletext" sits clear of both. The banner and scrim
// carry explicit z-index so their paint order is fixed regardless of source order.
// The top-right card mirrors the linuxmatters.sh episode badge: an opaque card
// holds a transparent in-flow div (the img stand-in) and an absolutely positioned
// pointer-events:none sibling span "badge86" on top. The hit-test at the badge
// centre returns the transparent stand-in, and only the opaque card ancestry, at
// the LCA and above, carries alpha; the badge must survive the occlusion cull.
// Two partly covered lines prove the cull reads more than the line-box centre. A
// 16px opaque strip sits over the centre of "narrowcovered" (x 10..114) and nowhere
// else, so the start and end samples stay clear. An opaque box covers the left half
// of "halfcovered" (x 10..98), so the end sample stays clear. Each line keeps at
// least one uncovered sample, so both runs must survive whole; the full-width
// banner over "behindtext" covers every sample, so it must still cull. Colours use
// rgb()/rgba() rather than hex: a '#' in a data: URL starts the fragment and would
// drop the rest of the document.
const occlusionTestPage = "data:text/html," +
	"<html><body style='margin:0'>" +
	"<div style='position:absolute;top:10px;left:10px'>visibletext</div>" +
	"<div style='position:absolute;top:10px;left:440px;width:160px;height:80px;background:rgb(40,40,40)'>" +
	"<div style='width:160px;height:80px'></div>" +
	"<span style='position:absolute;top:10px;right:10px;color:rgb(248,179,29);pointer-events:none'>badge86</span>" +
	"</div>" +
	"<div style='position:absolute;top:110px;left:10px'>underscrim</div>" +
	"<div style='position:fixed;top:100px;left:0;width:400px;height:40px;background:rgba(0,0,0,0.3);z-index:10'></div>" +
	"<div style='position:absolute;top:150px;left:10px'>narrowcovered</div>" +
	"<div style='position:absolute;top:146px;left:54px;width:16px;height:24px;background:rgb(0,0,0);z-index:30'></div>" +
	"<div style='position:absolute;top:190px;left:10px'>halfcovered</div>" +
	"<div style='position:absolute;top:184px;left:0;width:64px;height:24px;background:rgb(0,0,0);z-index:30'></div>" +
	"<div style='position:absolute;top:320px;left:10px'>behindtext</div>" +
	"<div style='position:fixed;bottom:0;left:0;width:640px;height:80px;background:rgb(0,0,0);color:rgb(255,255,255);z-index:20'>acceptcookies</div>" +
	"</body></html>"

// TestLiveOccludedTextDropped verifies that the block backend does not leak page
// text through opaque overlays (cookie banners, sticky
// headers, modals). It launches a real Chromium, navigates the occlusion page,
// injects the production forcing stylesheet, waits for the font, then runs the walk
// and asserts the opaque banner culls the behind-text while the banner's own text,
// the clear text, the text under a translucent scrim, the pointer-events:none badge
// over the image stand-in, and the two partly covered lines all survive. The live
// tag keeps it off `just test` and CI; run it with `just live`.
func TestLiveOccludedTextDropped(t *testing.T) {
	if _, err := resolveExecutable(""); err != nil {
		t.Skipf("no Chromium for the live test: %v", err)
	}

	const cellW, cellH = 8.0, 16.0

	e := New("")
	e.SetCellPx(cellW, cellH)
	e.Resize(80, 24)
	if err := e.Navigate(occlusionTestPage); err != nil {
		t.Fatalf("navigate: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })

	// Wait for the first synced frame so the page has laid out before the walk.
	deadline := time.NewTimer(30 * time.Second)
	defer deadline.Stop()
	select {
	case _, ok := <-e.Frames():
		if !ok {
			t.Fatal("frames channel closed before a frame arrived")
		}
	case <-deadline.C:
		t.Fatal("no synced frame within the deadline")
	}

	e.mu.Lock()
	ctx := e.ctx
	e.mu.Unlock()
	if ctx == nil {
		t.Fatal("engine context is nil after navigate")
	}
	// The announce arrives on its own bindingCalled event, so the first synced
	// frame does not imply the id is held yet. Poll for it, or the walk fails on
	// the missing-context guard whenever the event lands second.
	ctxID := awaitProductionLayoutShiftContext(t, e)

	// Inject the production forcing stylesheet and wait for the embedded font, so the
	// walk reads the forced monospace layout exactly as production does.
	styleScript := buildStylesheetScript(font.FiraCodeTTF(), cellW, cellH)
	if err := evaluateVoid(ctx, "inject forcing", styleScript, false); err != nil {
		t.Fatalf("inject forcing stylesheet: %v", err)
	}
	if err := evaluateVoid(ctx, "fonts ready", "document.fonts.ready", true); err != nil {
		t.Fatalf("await fonts ready: %v", err)
	}

	res, err := evaluateWalk(ctx, buildTextWalkScript(cellW, cellH), ctxID)
	if err != nil {
		t.Fatalf("text walk: %v", err)
	}

	var joined strings.Builder
	for _, r := range res.Runs {
		joined.WriteString(r.Text)
		joined.WriteString("\n")
	}
	all := joined.String()

	if strings.Contains(all, "behindtext") {
		t.Errorf("text under the opaque banner leaked into runs:\n%s", all)
	}
	if !strings.Contains(all, "acceptcookies") {
		t.Errorf("the opaque banner's own text 'acceptcookies' was dropped; runs:\n%s", all)
	}
	if !strings.Contains(all, "visibletext") {
		t.Errorf("clear text 'visibletext' was dropped; runs:\n%s", all)
	}
	if !strings.Contains(all, "underscrim") {
		t.Errorf("text under the translucent scrim was culled; runs:\n%s", all)
	}
	if !strings.Contains(all, "badge86") {
		t.Errorf("pointer-events:none badge over the image stand-in was culled; runs:\n%s", all)
	}
	if !strings.Contains(all, "narrowcovered") {
		t.Errorf("text under a narrow strip over its centre alone was culled; runs:\n%s", all)
	}
	if !strings.Contains(all, "halfcovered") {
		t.Errorf("text with only its left half covered was culled; runs:\n%s", all)
	}
}

// TestLiveFixedBoxesReportViewportAnchoredRects verifies that the walk reports the
// viewport-anchored boxes the compose-time scroll shift re-tests against. Over the
// occlusion page it must report exactly one box: the opaque position:fixed banner,
// flagged opaque and carrying its screen rect. The page's position:absolute divs
// are not viewport-anchored, so the exact count also proves none of them produced a
// box.
//
// The translucent position:fixed scrim yields no box, and that is a documented
// limit rather than a defect. Collection rides the text walk, so only a fixed
// ancestor of text the walk keeps is ever recorded, and the scrim holds no text. An
// undiscovered cover lets flow text bleed through for one refresh window, which is
// the safe polarity: it can never cull text the reader can see. The next settle
// walk corrects it. The non-opaque flag is covered without a browser instead.
//
// The live tag keeps it off `just test` and CI; run it with `just live`.
func TestLiveFixedBoxesReportViewportAnchoredRects(t *testing.T) {
	res := liveTextWalk(t, occlusionTestPage)

	if len(res.Fixed) != 1 {
		t.Fatalf("fixed boxes = %d, want the opaque banner alone: %+v", len(res.Fixed), res.Fixed)
	}
	box := res.Fixed[0]
	if !box.Opaque {
		t.Errorf("banner box opaque = false, want true for background rgb(0,0,0)")
	}

	// The banner's own width:640px fixes its left and right edges, and the 24-row
	// grid at cell height 16 fixes the 384px viewport its bottom:0 anchors to, so
	// the rect holds whatever column count the live walk page runs at.
	const tolerance = 0.5
	edges := []struct {
		name string
		got  float64
		want float64
	}{
		{name: "left", got: box.Left, want: 0},
		{name: "top", got: box.Top, want: 304},
		{name: "right", got: box.Right, want: 640},
		{name: "bottom", got: box.Bottom, want: 384},
	}
	for _, edge := range edges {
		if math.Abs(edge.got-edge.want) > tolerance {
			t.Errorf("banner box %s = %.2f, want %.2f", edge.name, edge.got, edge.want)
		}
	}
}

// transparentFixedBoxTestPage pairs two position:fixed bars over an opaque body,
// which is the discrimination the opaque flag has to make. The top bar sets no
// background of its own, so it must read non-opaque even though the body under it is
// solid white; compositing past the bar to the body would read it as a cover and cull
// the text scrolling beneath. The bottom bar paints rgb(0,0,0) itself, so it must
// still read opaque and the cull must still fire. The bars neither overlap nor share
// text, so each is collected on its own. Colours use rgb() rather than hex: a '#' in
// a data: URL starts the fragment and would drop the rest of the document.
const transparentFixedBoxTestPage = "data:text/html," +
	"<html><body style='margin:0;background:rgb(255,255,255)'>" +
	"<div style='position:fixed;top:0;left:0;width:320px;height:32px'>transparentbar</div>" +
	"<div style='position:fixed;top:96px;left:0;width:320px;height:32px;background:rgb(0,0,0);color:rgb(255,255,255)'>opaquebar</div>" +
	"</body></html>"

// TestLiveTransparentFixedBoxIsNotOpaque verifies the opaque flag reads the fixed
// element's own background and nothing above it. A transparent bar on an opaque body
// must report non-opaque, because a page that sets body{background} is the common
// case and the flag drives a cull that deletes text. The opaque bar on the same page
// is the control: it must still report opaque, so the pair proves the walk
// discriminates rather than answering false to everything.
//
// It then feeds the live boxes into the real shiftRunsToBitmapScroll, which is where
// a wrong flag does its damage: a flow run shifted under the transparent bar must
// survive, and one shifted under the opaque bar must still be culled.
//
// The live tag keeps it off `just test` and CI; run it with `just live`.
func TestLiveTransparentFixedBoxIsNotOpaque(t *testing.T) {
	res := liveTextWalk(t, transparentFixedBoxTestPage)

	if len(res.Fixed) != 2 {
		t.Fatalf("fixed boxes = %d, want the two bars: %+v", len(res.Fixed), res.Fixed)
	}
	// Named by top edge rather than by index, so collection order cannot decide the
	// result. The bars are 32 CSS pixels tall at Y 0 and Y 96.
	const tolerance = 0.5
	boxAtTop := func(top float64) walkFixedBox {
		t.Helper()
		for _, box := range res.Fixed {
			if math.Abs(box.Top-top) <= tolerance {
				return box
			}
		}
		t.Fatalf("no fixed box with top %.0f: %+v", top, res.Fixed)
		return walkFixedBox{}
	}
	transparent, opaque := boxAtTop(0), boxAtTop(96)

	if transparent.Opaque {
		t.Error("the bar with no background of its own reported opaque; only the body under it is solid, and compositing to the body culls text the reader can see")
	}
	if !opaque.Opaque {
		t.Error("the bar painting rgb(0,0,0) reported non-opaque, so the coverage cull would never fire")
	}

	// The consequence, on the production shift. The cell is 16 CSS pixels and the grid
	// 24 rows, matching liveWalkPage. The run is hand-built and unanchored, so the
	// anchor test cannot hold it and the coverage test decides alone. A 192-pixel
	// delta lands the first under the transparent bar and a 96-pixel one lands the
	// second under the opaque bar.
	const cellCSSH, rows = 16.0, 24
	flow := []render.GlyphRun{{Col: 1, Row: 12, Text: "flowtext", X: 10, Y: 200, W: 64, H: 16}}

	under := shiftRunsToBitmapScroll(flow, res.Fixed, 192, 0, cellCSSH, rows)
	if len(under) != 1 {
		t.Errorf("flow runs under the transparent bar = %d, want 1: a bar with no background of its own is not a cover", len(under))
	}
	culled := shiftRunsToBitmapScroll(flow, res.Fixed, 96, 0, cellCSSH, rows)
	if len(culled) != 0 {
		t.Errorf("flow runs under the opaque bar = %d, want 0: an opaque bar still covers what scrolls beneath it", len(culled))
	}
}

// fixedContainerTestPage is the shape that breaks a geometric answer to "is this run
// anchored". A full-viewport transparent position:fixed container holds one toast, so
// its rect encloses the whole page while it owns one line of text. Two flow lines sit
// inside that rect without descending from it, and an opaque fixed banner at the foot
// owns its own line. The rows fall at 16, 160, 320 and 352 CSS pixels, which is rows
// 1, 10, 20 and 22 at the live cell of 16.
const fixedContainerTestPage = "data:text/html," +
	"<html><body style='margin:0'>" +
	"<div style='position:fixed;top:0;left:0;right:0;bottom:0'>" +
	"<div style='position:absolute;top:16px;left:0'>toastnotice</div></div>" +
	"<div style='position:absolute;top:160px;left:0'>flowalpha</div>" +
	"<div style='position:absolute;top:320px;left:0'>flowbravo</div>" +
	"<div style='position:fixed;top:352px;left:0;width:960px;height:32px;" +
	"background:rgb(0,0,0);color:rgb(255,255,255)'>bannertext</div>" +
	"</body></html>"

// TestLiveFixedContainerDoesNotFreezeFlowText verifies that the compose-time shift
// holds a run because it descends from a position:fixed element, not because its
// rectangle happens to sit inside one. Geometric containment answers this page wrong:
// the transparent container covers the viewport, so every flow run is inside it and
// the whole overlay freezes for the scroll, holding rows [1 10 20] where the flow runs
// must reach [6 16].
//
// The opaque banner is the control in the other direction. Its own text must still be
// held, and held before the cover test runs, or the banner's own box would cull it.
//
// It drives the production path end to end: the live walk, decodeWalkRuns, and the
// real shiftRunsToBitmapScroll. The live tag keeps it off `just test` and CI; run it
// with `just live`.
func TestLiveFixedContainerDoesNotFreezeFlowText(t *testing.T) {
	res := liveTextWalk(t, fixedContainerTestPage)

	// The container and the banner both hold text, so both are collected. The count
	// also proves the flow divs produced no box of their own.
	if len(res.Fixed) != 2 {
		t.Fatalf("fixed boxes = %d, want the container and the banner: %+v", len(res.Fixed), res.Fixed)
	}

	const cellCSSW, cellCSSH, cols, rows = 8.0, 16.0, 120, 24
	byText := func(runs []render.GlyphRun) map[string]render.GlyphRun {
		t.Helper()
		out := make(map[string]render.GlyphRun, len(runs))
		for _, run := range runs {
			out[run.Text] = run
		}
		for _, want := range []string{"toastnotice", "flowalpha", "flowbravo", "bannertext"} {
			if _, ok := out[want]; !ok {
				t.Fatalf("run %q missing; got %+v", want, runs)
			}
		}
		return out
	}

	walked := byText(decodeWalkRuns(res, cellCSSW, cols))
	for _, tc := range []struct {
		text     string
		row      int
		anchored bool
	}{
		{"toastnotice", 1, true},
		{"flowalpha", 10, false},
		{"flowbravo", 20, false},
		{"bannertext", 22, true},
	} {
		run := walked[tc.text]
		if run.Row != tc.row {
			t.Fatalf("run %q walked at row %d, want %d: the fixture is not laid out as the test reads it", tc.text, run.Row, tc.row)
		}
		if run.ViewportAnchored != tc.anchored {
			t.Errorf("run %q anchored = %v, want %v: the tag must follow DOM descent from a position:fixed element", tc.text, run.ViewportAnchored, tc.anchored)
		}
	}

	// Four cells of scroll. The two flow runs move up four rows; the toast and the
	// banner hold, because the page moved under them and they did not.
	shifted := byText(shiftRunsToBitmapScroll(decodeWalkRuns(res, cellCSSW, cols), res.Fixed, 4*cellCSSH, 0, cellCSSH, rows))
	if got := shifted["flowalpha"].Row; got != 6 {
		t.Errorf("flowalpha row = %d, want 6: a flow run inside a transparent fixed container's rect is not that container's text and must shift", got)
	}
	if got := shifted["flowbravo"].Row; got != 16 {
		t.Errorf("flowbravo row = %d, want 16: a flow run inside a transparent fixed container's rect is not that container's text and must shift", got)
	}
	if got := shifted["toastnotice"].Row; got != 1 {
		t.Errorf("toastnotice row = %d, want 1: the container's own text is viewport-anchored and holds its screen position", got)
	}
	if got := shifted["bannertext"].Row; got != 22 {
		t.Errorf("bannertext row = %d, want 22: an opaque banner's own text is held before the cover test, so its own box never culls it", got)
	}
}

// stackRun is the pair of walk run fields the stacking tests read: the run text
// and the dense front-to-back rank. walkRun does not model the rank, so these
// tests decode the walk's raw JSON into this local struct instead.
type stackRun struct {
	Text string `json:"t"`
	Rank int    `json:"z"`
}

// stackWalk is the walk envelope reduced to the runs the stacking tests read.
type stackWalk struct {
	Runs []stackRun `json:"runs"`
}

// liveStackRanks walks page in a real Chromium and returns both the decoded runs
// and a lookup from run text to stacking rank, rank 0 being front-most. Every
// fixture below gives each box unique text, so the lookup names a box by what it
// holds. The runs come back too, so a test can assert over the whole rank set.
func liveStackRanks(t *testing.T, page string) ([]stackRun, map[string]int) {
	t.Helper()
	ctx, ctxID, cellW, cellH := liveWalkPage(t, page)
	raw, err := evaluateWalkJSON(ctx, buildTextWalkScript(cellW, cellH), ctxID)
	if err != nil {
		t.Fatalf("text walk: %v", err)
	}
	var decoded stackWalk
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("decode walk JSON: %v", err)
	}
	ranks := make(map[string]int, len(decoded.Runs))
	for _, run := range decoded.Runs {
		ranks[run.Text] = run.Rank
	}
	return decoded.Runs, ranks
}

// stackRankOf returns the rank of the run holding text and fails when the walk
// emitted no such run, so a silently culled run is never read as a pass.
func stackRankOf(t *testing.T, ranks map[string]int, text string) int {
	t.Helper()
	rank, ok := ranks[text]
	if !ok {
		t.Fatalf("no run with text %q; ranks: %v", text, ranks)
	}
	return rank
}

// assertRankedInFront fails unless the run holding front carries a lower rank
// than the run holding behind, because rank 0 is front-most.
func assertRankedInFront(t *testing.T, ranks map[string]int, front, behind string) {
	t.Helper()
	frontRank := stackRankOf(t, ranks, front)
	behindRank := stackRankOf(t, ranks, behind)
	if frontRank >= behindRank {
		t.Errorf("rank %q = %d and %q = %d; want %q in front", front, frontRank, behind, behindRank, front)
	}
}

// zIndexOrderTestPage proves the rank follows z-index rather than DOM order. All
// three boxes are absolutely positioned, carry no background, and sit at distinct
// tops so none occludes another. "zbase" comes first in DOM at z-index 0, "zhigh"
// second at z-index 5, and "zlow" last at z-index 1. CSS 2.1 Appendix E paints them
// zbase, zlow, zhigh back to front, so DOM order alone cannot produce the expected
// answer for any pair.
const zIndexOrderTestPage = "data:text/html," +
	"<html><body style='margin:0'>" +
	"<div style='position:absolute;top:80px;left:0;z-index:0'>zbase</div>" +
	"<div style='position:absolute;top:0;left:0;z-index:5'>zhigh</div>" +
	"<div style='position:absolute;top:40px;left:0;z-index:1'>zlow</div>" +
	"</body></html>"

// TestLiveStackRankFollowsZIndexNotDOMOrder checks that a higher z-index ranks in
// front of a later sibling with a lower z-index, and that both rank in front of the
// z-index 0 box that precedes them both in the document.
func TestLiveStackRankFollowsZIndexNotDOMOrder(t *testing.T) {
	_, ranks := liveStackRanks(t, zIndexOrderTestPage)
	assertRankedInFront(t, ranks, "zhigh", "zlow")
	assertRankedInFront(t, ranks, "zhigh", "zbase")
	assertRankedInFront(t, ranks, "zlow", "zbase")
}

// TestLiveStackRanksAreDenseAndUnique checks that the emitted ranks over a page are
// exactly the integers 0 to n-1 for n runs, so nothing downstream has to cope with
// a gap or a repeat.
func TestLiveStackRanksAreDenseAndUnique(t *testing.T) {
	runs, _ := liveStackRanks(t, zIndexOrderTestPage)
	if len(runs) == 0 {
		t.Fatal("the walk emitted no runs")
	}
	got := make([]int, 0, len(runs))
	for _, run := range runs {
		got = append(got, run.Rank)
	}
	sort.Ints(got)
	for i, rank := range got {
		if rank != i {
			t.Fatalf("ranks over %d runs = %v, want the integers 0 to %d", len(runs), got, len(runs)-1)
		}
	}
}

// negativeZIndexTestPage proves a negative z-index paints behind the stacking
// context's own in-flow content. The in-flow "inflowtext" paragraph and the
// absolutely positioned "behindz" box carry no background and sit at distinct tops,
// so neither occludes the other. CSS 2.1 Appendix E paints negative-z-index
// children before in-flow content, so inflowtext is in front.
const negativeZIndexTestPage = "data:text/html," +
	"<html><body style='margin:0'>" +
	"<p style='margin:0'>inflowtext</p>" +
	"<div style='position:absolute;top:40px;left:0;z-index:-1'>behindz</div>" +
	"</body></html>"

// TestLiveStackRankNegativeZIndexBehindInFlowText checks the comparator's prefix
// rule: the in-flow run's vector is a prefix of the positioned run's, and the next
// level's negative z-index puts the longer vector behind.
func TestLiveStackRankNegativeZIndexBehindInFlowText(t *testing.T) {
	_, ranks := liveStackRanks(t, negativeZIndexTestPage)
	assertRankedInFront(t, ranks, "inflowtext", "behindz")
}

// autoZIndexTestPage proves a positioned box at z-index auto paints in front of the
// stacking context's own in-flow content. The in-flow "inflowtext" paragraph and the
// positioned "overtext" box carry no background and sit at distinct tops, so neither
// occludes the other. CSS 2.1 Appendix E paints positioned descendants after in-flow
// content, so overtext is in front.
const autoZIndexTestPage = "data:text/html," +
	"<html><body style='margin:0'>" +
	"<p style='margin:0'>inflowtext</p>" +
	"<div style='position:absolute;top:40px;left:0'>overtext</div>" +
	"</body></html>"

// TestLiveStackRankPositionedAutoInFrontOfInFlowText checks the other branch of the
// prefix rule: a non-negative next level puts the longer vector in front.
func TestLiveStackRankPositionedAutoInFrontOfInFlowText(t *testing.T) {
	_, ranks := liveStackRanks(t, autoZIndexTestPage)
	assertRankedInFront(t, ranks, "overtext", "inflowtext")
}

// stackingTrapTestPageTemplate proves a non-positioned wrapper that establishes a
// stacking context traps its descendant's z-index. The wrapper carries a trigger in
// place of the style placeholder and holds the absolutely positioned "trapped" box at
// z-index 100. Its sibling "sibling" is absolutely positioned at z-index 1. When the
// wrapper establishes a context, the trapped box cannot rise above the sibling,
// because the wrapper itself paints as non-positioned in-flow content below every
// positioned box. With the trigger removed the two boxes share the root context and
// z-index 100 wins. Both boxes carry no background and sit at distinct tops, so
// neither occludes the other.
const stackingTrapTestPageTemplate = "data:text/html," +
	"<html><body style='margin:0'>" +
	"<div style='__TRIGGER__'>" +
	"<div style='position:absolute;top:0;left:0;z-index:100'>trapped</div>" +
	"</div>" +
	"<div style='position:absolute;top:40px;left:0;z-index:1'>sibling</div>" +
	"</body></html>"

// stackingTrapTestPage returns the trap page with trigger as the wrapper's style. An
// empty trigger gives the control page whose wrapper establishes no context.
func stackingTrapTestPage(trigger string) string {
	return strings.Replace(stackingTrapTestPageTemplate, "__TRIGGER__", trigger, 1)
}

// TestLiveStackRankNonPositionedTriggersTrapDescendantZIndex checks each
// non-positioned stacking-context trigger. The trapped z-index 100 descendant must
// rank behind its wrapper's z-index 1 sibling, and the control with no trigger must
// rank it in front, which proves the trigger was detected rather than the assertion
// passing by accident.
func TestLiveStackRankNonPositionedTriggersTrapDescendantZIndex(t *testing.T) {
	triggers := []struct {
		name  string
		style string
	}{
		{name: "opacity", style: "opacity:0.5"},
		{name: "transform", style: "transform:translateZ(0)"},
		{name: "filter", style: "filter:blur(0px)"},
		{name: "will-change", style: "will-change:transform"},
	}
	for _, trigger := range triggers {
		t.Run(trigger.name, func(t *testing.T) {
			_, ranks := liveStackRanks(t, stackingTrapTestPage(trigger.style))
			assertRankedInFront(t, ranks, "sibling", "trapped")
		})
	}
	t.Run("control", func(t *testing.T) {
		_, ranks := liveStackRanks(t, stackingTrapTestPage(""))
		assertRankedInFront(t, ranks, "trapped", "sibling")
	})
}

// flexChildZIndexTestPage proves a flex child ranks by z-index while it stays at
// position static. The column flex container stacks "flexlow" at z-index 1 above
// "flexhigh" at z-index 5 in DOM and layout order, both without a background, so DOM
// order and z-index disagree and the boxes do not occlude each other. CSS Flexbox
// gives a flex item's z-index effect regardless of position, so flexhigh is in front.
const flexChildZIndexTestPage = "data:text/html," +
	"<html><body style='margin:0'>" +
	"<div style='display:flex;flex-direction:column'>" +
	"<div style='z-index:1'>flexlow</div>" +
	"<div style='z-index:5'>flexhigh</div>" +
	"</div></body></html>"

// TestLiveStackRankFlexChildUsesZIndexAtStatic checks that the later, higher
// z-index flex child ranks in front of the earlier, lower one.
func TestLiveStackRankFlexChildUsesZIndexAtStatic(t *testing.T) {
	_, ranks := liveStackRanks(t, flexChildZIndexTestPage)
	assertRankedInFront(t, ranks, "flexhigh", "flexlow")
}

// displayContentsFlexChildTestPage proves a flex item is found through a
// 'display: contents' wrapper. That wrapper generates no box, so "contentslow" and
// "contentshigh" are flex items of the outer column container and their z-index
// takes effect. Reading only the immediate parent's display drops both z-indices
// and leaves DOM order to decide, which puts the wrong box in front. Neither box
// carries a background and the column keeps them on separate rows, so nothing
// occludes anything.
const displayContentsFlexChildTestPage = "data:text/html," +
	"<html><body style='margin:0'>" +
	"<div style='display:flex;flex-direction:column'>" +
	"<div style='display:contents'>" +
	"<div style='z-index:1'>contentslow</div>" +
	"<div style='z-index:5'>contentshigh</div>" +
	"</div></div></body></html>"

// TestLiveStackRankFlexChildThroughDisplayContents checks that the higher z-index
// flex item ranks in front although a 'display: contents' wrapper sits between it
// and the flex container.
func TestLiveStackRankFlexChildThroughDisplayContents(t *testing.T) {
	_, ranks := liveStackRanks(t, displayContentsFlexChildTestPage)
	assertRankedInFront(t, ranks, "contentshigh", "contentslow")
}

// positionedAutoEscapeTestPage proves a positioned descendant escapes a
// 'position: relative; z-index: auto' wrapper. Such a wrapper opens no stacking
// context, so "trapped" competes at z-index 100 against "sibling" at z-index 1 in
// the root context and Chromium paints trapped on top. Treating the wrapper as a
// context resolves the pair on the wrapper's spurious level and inverts them.
//
// The two boxes sit at the same origin on purpose, so the rank decides a genuinely
// contested cell rather than an abstract order. Neither carries a background, so
// the occlusion cull cannot decide it instead and both runs reach the overlay.
const positionedAutoEscapeTestPage = "data:text/html," +
	"<html><body style='margin:0'>" +
	"<div style='position:relative'>" +
	"<div style='position:absolute;top:0;left:0;z-index:100'>trapped</div>" +
	"</div>" +
	"<div style='position:absolute;top:0;left:0;z-index:1'>sibling</div>" +
	"</body></html>"

// TestLiveStackRankEscapesPositionedAutoWrapper checks that the z-index 100
// descendant of a z-index auto wrapper ranks in front of the wrapper's z-index 1
// sibling, the order Chromium paints.
func TestLiveStackRankEscapesPositionedAutoWrapper(t *testing.T) {
	_, ranks := liveStackRanks(t, positionedAutoEscapeTestPage)
	assertRankedInFront(t, ranks, "trapped", "sibling")
}

// negativeZIndexEscapeTestPage proves the same escape in the negative direction.
// "negtext" carries z-index -1 inside a 'position: relative; z-index: auto'
// wrapper, so it joins the root context's negative-z-index children and Chromium
// paints it behind the in-flow "inflowtext" paragraph. Treating the wrapper as a
// context buries the negative level one step down, where the comparator's prefix
// rule never reaches it, and the paragraph loses.
//
// The zero-height wrapper puts both boxes at the same origin, for the same reason
// the escape page does, and neither carries a background.
const negativeZIndexEscapeTestPage = "data:text/html," +
	"<html><body style='margin:0'>" +
	"<div style='position:relative;height:0'>" +
	"<div style='position:absolute;top:0;left:0;z-index:-1'>negtext</div>" +
	"</div>" +
	"<p style='margin:0'>inflowtext</p>" +
	"</body></html>"

// TestLiveStackRankNegativeZIndexEscapesPositionedAutoWrapper checks that the
// z-index -1 descendant of a z-index auto wrapper ranks behind the in-flow
// paragraph it overlaps.
func TestLiveStackRankNegativeZIndexEscapesPositionedAutoWrapper(t *testing.T) {
	_, ranks := liveStackRanks(t, negativeZIndexEscapeTestPage)
	assertRankedInFront(t, ranks, "inflowtext", "negtext")
}

// topLayerTestPage proves the top layer outranks any z-index the page can set.
// The modal dialog paints above the whole document, so "dialogtext" is in front of
// "rivaltext" although the rival carries the largest z-index CSS accepts. The
// dialog drops its own background so the occlusion cull cannot decide the pair,
// and both boxes share an origin so the rank decides a contested cell.
const topLayerTestPage = "data:text/html," +
	"<html><body style='margin:0'>" +
	"<dialog id='modal' style='margin:0;padding:0;border:0;background:transparent;position:fixed;top:0;left:0'>dialogtext</dialog>" +
	"<div style='position:absolute;top:0;left:0;z-index:2147483647'>rivaltext</div>" +
	"<script>document.getElementById('modal').showModal()</script>" +
	"</body></html>"

// TestLiveStackRankTopLayerOutranksMaximumZIndex checks that an open modal dialog
// ranks in front of a box at z-index 2147483647.
func TestLiveStackRankTopLayerOutranksMaximumZIndex(t *testing.T) {
	_, ranks := liveStackRanks(t, topLayerTestPage)
	assertRankedInFront(t, ranks, "dialogtext", "rivaltext")
}

// equalZIndexSiblingTestPage proves the comparator's document-order tie-break.
// Two absolutely positioned siblings carry the same z-index 1 and share one
// origin, so their stacking vectors agree on the z-index and only document order
// can separate them. CSS 2.1 Appendix E paints tree order within one level, so
// Chromium paints "laterbox" on top of "earlierbox". Neither carries a
// background, so the occlusion cull cannot decide the pair, and the shared origin
// makes the rank decide a genuinely contested cell.
const equalZIndexSiblingTestPage = "data:text/html," +
	"<html><body style='margin:0'>" +
	"<div style='position:absolute;top:0;left:0;z-index:1'>earlierbox</div>" +
	"<div style='position:absolute;top:0;left:0;z-index:1'>laterbox</div>" +
	"</body></html>"

// TestLiveStackRankEqualZIndexKeepsLaterSiblingInFront checks that the later of
// two tied siblings ranks in front, the order Chromium paints. Dropping the
// document-order term from the comparator leaves the pair tied, and the sort's
// own index fallback then puts the earlier sibling in front instead.
func TestLiveStackRankEqualZIndexKeepsLaterSiblingInFront(t *testing.T) {
	_, ranks := liveStackRanks(t, equalZIndexSiblingTestPage)
	assertRankedInFront(t, ranks, "laterbox", "earlierbox")
}

// stackingTriggerTraps lists the stacking-context triggers that no other live
// test covers, each with the wrapper style that carries it. Every one of them
// makes the wrapper a real stacking context, so the z-index 100 box inside it
// cannot rise above a z-index 1 box outside it. Deleting any one of these lines
// from the walk's stacks() passes every other live test.
//
// The last two are positioned triggers rather than the non-positioned ones the
// older test covers: a positioned box with an explicit z-index, and a sticky box
// at z-index auto. Both take their own branch of the three-state participation
// test, so neither falls out of the blanket positioned rule any more.
var stackingTriggerTraps = []struct {
	name  string
	style string
}{
	{name: "mix-blend-mode", style: "mix-blend-mode:multiply"},
	{name: "scale", style: "scale:1.01"},
	{name: "rotate", style: "rotate:1deg"},
	{name: "translate", style: "translate:1px"},
	{name: "perspective", style: "perspective:100px"},
	{name: "backdrop-filter", style: "backdrop-filter:blur(1px)"},
	{name: "clip-path", style: "clip-path:inset(0)"},
	{name: "mask-image", style: "mask-image:linear-gradient(black,black)"},
	{name: "isolation", style: "isolation:isolate"},
	{name: "contain", style: "contain:layout"},
	{name: "content-visibility", style: "content-visibility:auto"},
	{name: "view-transition-name", style: "view-transition-name:trapped"},
	{name: "relative-with-z-index", style: "position:relative;z-index:0"},
	{name: "sticky", style: "position:sticky;top:0"},
}

// trapTextFor returns the unique text the trapped box under the named trigger
// carries, so one rank lookup names one trigger.
func trapTextFor(name string) string {
	return "trap" + strings.ReplaceAll(name, "-", "")
}

// stackingTriggerTrapPage builds one page carrying every trigger in
// stackingTriggerTraps at once: one wrapper each, holding an absolutely
// positioned z-index 100 box, against one shared z-index 1 sibling, plus an
// untriggered control wrapper.
//
// One page rather than one page per trigger. Each trigger's assertion reads only
// its own trapped box against the shared sibling, and the wrappers cannot
// interfere: a trapped box compares against the sibling on its own wrapper's
// level, which is z-index 0 (or 1 for the sticky and relative wrappers, still
// below the sibling's own document position), so the answer for one trigger does
// not depend on any other. That buys the whole matrix for one Chromium launch
// and one walk instead of fifteen.
//
// Every wrapper is sized rather than collapsed, so a trigger that brings paint
// containment with it (content-visibility, contain) does not clip its own box
// away, and the whole column stays inside the 384px viewport so
// content-visibility: auto renders rather than skipping its subtree. Nothing
// carries a background, so the occlusion cull cannot decide any pair.
func stackingTriggerTrapPage() string {
	var b strings.Builder
	b.WriteString("data:text/html,<html><body style='margin:0'>")
	b.WriteString("<div style='position:absolute;top:0;left:0;z-index:1'>sibling</div>")
	for _, trap := range stackingTriggerTraps {
		fmt.Fprintf(
			&b,
			"<div style='width:200px;height:20px;%s'>"+
				"<div style='position:absolute;top:0;left:0;z-index:100'>%s</div></div>",
			trap.style, trapTextFor(trap.name),
		)
	}
	b.WriteString(
		"<div style='width:200px;height:20px'>" +
			"<div style='position:absolute;top:0;left:0;z-index:100'>trapcontrol</div></div>")
	b.WriteString("</body></html>")
	return b.String()
}

// TestLiveStackRankTriggerMatrixTrapsDescendantZIndex checks every stacking
// trigger the older non-positioned test leaves out. Each trigger's trapped
// z-index 100 descendant must rank behind the shared z-index 1 sibling, and the
// untriggered control must rank its own trapped box in front, which proves the
// assertions are not passing by accident.
func TestLiveStackRankTriggerMatrixTrapsDescendantZIndex(t *testing.T) {
	_, ranks := liveStackRanks(t, stackingTriggerTrapPage())
	for _, trap := range stackingTriggerTraps {
		t.Run(trap.name, func(t *testing.T) {
			assertRankedInFront(t, ranks, "sibling", trapTextFor(trap.name))
		})
	}
	t.Run("control", func(t *testing.T) {
		assertRankedInFront(t, ranks, "trapcontrol", "sibling")
	})
}

// flexChildZIndexAgainstDOMOrderTestPage is the flex page with DOM order and
// z-index in disagreement. "flexearlyhigh" comes first at z-index 5 and
// "flexlatelow" second at z-index 1, so only a flex item's z-index taking effect
// can rank the earlier box in front: dropping every flex item to z-index 0 leaves
// the two tied, and the comparator's document-order tie-break then puts the later
// box in front instead. flexChildZIndexTestPage cannot see that mutation, because
// there DOM order and z-index agree on the answer. Neither box carries a
// background and the column keeps them on separate rows, so nothing occludes
// anything.
const flexChildZIndexAgainstDOMOrderTestPage = "data:text/html," +
	"<html><body style='margin:0'>" +
	"<div style='display:flex;flex-direction:column'>" +
	"<div style='z-index:5'>flexearlyhigh</div>" +
	"<div style='z-index:1'>flexlatelow</div>" +
	"</div></body></html>"

// TestLiveStackRankFlexChildZIndexBeatsDOMOrder checks that the earlier, higher
// z-index flex child ranks in front of the later, lower one.
func TestLiveStackRankFlexChildZIndexBeatsDOMOrder(t *testing.T) {
	_, ranks := liveStackRanks(t, flexChildZIndexAgainstDOMOrderTestPage)
	assertRankedInFront(t, ranks, "flexearlyhigh", "flexlatelow")
}
