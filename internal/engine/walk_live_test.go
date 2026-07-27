//go:build live

package engine

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

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
// It skips the test when no Chromium resolves.
func liveWalkPage(t *testing.T, page string) (context.Context, float64, float64) {
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

	styleScript := buildStylesheetScript(font.FiraCodeTTF(), cellW, cellH)
	if err := evaluateVoid(ctx, "inject forcing", styleScript, false); err != nil {
		t.Fatalf("inject forcing stylesheet: %v", err)
	}
	if err := evaluateVoid(ctx, "fonts ready", "document.fonts.ready", true); err != nil {
		t.Fatalf("await fonts ready: %v", err)
	}
	return ctx, cellW, cellH
}

func liveTextWalk(t *testing.T, page string) walkResult {
	t.Helper()
	ctx, cellW, cellH := liveWalkPage(t, page)
	res, err := evaluateWalk(ctx, buildTextWalkScript(cellW, cellH))
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

	// Inject the production forcing stylesheet and wait for the embedded font, so the
	// walk reads the forced monospace layout exactly as production does.
	styleScript := buildStylesheetScript(font.FiraCodeTTF(), cellW, cellH)
	if err := evaluateVoid(ctx, "inject forcing", styleScript, false); err != nil {
		t.Fatalf("inject forcing stylesheet: %v", err)
	}
	if err := evaluateVoid(ctx, "fonts ready", "document.fonts.ready", true); err != nil {
		t.Fatalf("await fonts ready: %v", err)
	}

	res, err := evaluateWalk(ctx, buildTextWalkScript(cellW, cellH))
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

	// Inject the production forcing stylesheet and wait for the embedded font, so the
	// walk reads the forced monospace layout exactly as production does.
	styleScript := buildStylesheetScript(font.FiraCodeTTF(), cellW, cellH)
	if err := evaluateVoid(ctx, "inject forcing", styleScript, false); err != nil {
		t.Fatalf("inject forcing stylesheet: %v", err)
	}
	if err := evaluateVoid(ctx, "fonts ready", "document.fonts.ready", true); err != nil {
		t.Fatalf("await fonts ready: %v", err)
	}

	res, err := evaluateWalk(ctx, buildTextWalkScript(cellW, cellH))
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
	ctx, cellW, cellH := liveWalkPage(t, page)
	raw, err := evaluateWalkJSON(ctx, buildTextWalkScript(cellW, cellH))
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
