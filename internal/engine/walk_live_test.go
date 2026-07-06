//go:build live

package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/wimpysworld/nyala/internal/render/font"
)

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

// TestLiveScreenReaderOnlyTextDropped is the live-tagged regression test for the
// block backend leaking screen-reader-only text as overlay runs. It launches a real
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

// occlusionTestPage carries four text nodes in a deterministic 640x384 CSS viewport
// (grid 80x24 at cell 8x16, device scale 1). An opaque position:fixed banner at the
// bottom (background rgb(0,0,0), its own text "acceptcookies") covers the absolute
// "behindtext". A translucent position:fixed scrim (rgba(0,0,0,0.3)) covers the
// absolute "underscrim". "visibletext" sits clear of both. The banner and scrim
// carry explicit z-index so their paint order is fixed regardless of source order.
// Colours use rgb()/rgba() rather than hex: a '#' in a data: URL starts the fragment
// and would drop the rest of the document.
const occlusionTestPage = "data:text/html," +
	"<html><body style='margin:0'>" +
	"<div style='position:absolute;top:10px;left:10px'>visibletext</div>" +
	"<div style='position:absolute;top:110px;left:10px'>underscrim</div>" +
	"<div style='position:fixed;top:100px;left:0;width:400px;height:40px;background:rgba(0,0,0,0.3);z-index:10'></div>" +
	"<div style='position:absolute;top:320px;left:10px'>behindtext</div>" +
	"<div style='position:fixed;bottom:0;left:0;width:640px;height:80px;background:rgb(0,0,0);color:rgb(255,255,255);z-index:20'>acceptcookies</div>" +
	"</body></html>"

// TestLiveOccludedTextDropped is the live-tagged regression test for the block
// backend leaking page text through opaque overlays (cookie banners, sticky
// headers, modals). It launches a real Chromium, navigates the occlusion page,
// injects the production forcing stylesheet, waits for the font, then runs the walk
// and asserts the opaque banner culls the behind-text while the banner's own text,
// the clear text, and the text under a translucent scrim all survive. The live tag
// keeps it off `just test` and CI; run it with `just live`.
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
}
