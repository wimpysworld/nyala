//go:build diagnostics

package harness

import (
	"image"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/wimpysworld/nyala/internal/cellraster"
	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/ocr"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// overlayArtefacts reports whether OVERLAY_ARTEFACTS is set, the flag that makes
// TestOverlayMatcherRoundTrip write each fixture's rasterised overlay grid PNG to
// testdata/diagnostics/ for the owner to eyeball. It mirrors the UPDATE_GOLDEN
// convention.
func overlayArtefacts() bool {
	v := os.Getenv("OVERLAY_ARTEFACTS")
	return v != "" && v != "0"
}

// writeOverlayArtefact rasterises the composed overlay grid for a page and writes
// it as a PNG under testdata/diagnostics/overlay/ for visual review. The
// directory is gitignored, so the artefact never lands in the committed tree. It
// is a no-op unless OVERLAY_ARTEFACTS is set.
func writeOverlayArtefact(t *testing.T, page string, img *image.RGBA) {
	t.Helper()
	dir := filepath.Join(diagnosticsOutDir(), "overlay")
	if err := os.MkdirAll(dir, dirMode); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	path := filepath.Join(dir, page+".png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if err := cellraster.EncodePNG(f, img); err != nil {
		t.Fatalf("EncodePNG %s: %v", path, err)
	}
	t.Logf("overlay artefact: wrote %s", path)
}

// downscaleGolden downscales a source image to the 2x4-per-cell compose extent
// the pinned 120x40 grid maps, the same geometry goldenGrid uses so the overlay
// grids compose from the identical source the bitmap golden track reads.
func downscaleGolden(img *image.RGBA) *image.RGBA {
	return metrics.Downscale(img, CalibCols*2, CalibRows*4)
}

// overlayMode is the baseline mode label for the overlay grade rows. It is
// distinct from the sweep modes (block, sixel, kitty), so the overlay
// low-contrast text-cell count lands in its own baseline entries and never
// collides with the existing per-page block rows.
const overlayMode = "overlay"

// overlayGoldenPath returns the committed overlay golden archive path for a page,
// mirroring goldenPath but under the golden/overlay subdirectory.
func overlayGoldenPath(page string) string {
	return filepath.Join("..", "..", "testdata", "golden", "overlay", page+".txtar")
}

// composeOverlay composes a fixture PNG into the pinned 120x40 grid with its run
// set folded on, mirroring goldenGrid's downscale geometry. It returns the grid
// and the run truth in cell-walk order (see runTruth) for CER scoring.
func composeOverlay(t *testing.T, fx OverlayFixture) (block.Grid, string) {
	t.Helper()
	img := loadRGBA(t, filepath.Join("..", "..", "testdata", fx.PNG))
	composeSrc := downscaleGolden(img)
	runs, err := LoadRunSet(filepath.Join(OverlayDir(), fx.Runs))
	if err != nil {
		t.Fatalf("LoadRunSet %s: %v", fx.Runs, err)
	}
	br := block.Renderer{Cols: CalibCols, Rows: CalibRows}
	g := br.Compose(render.Frame{Img: composeSrc, Text: runs})
	return g, fixtureTruth(fx, runs)
}

// fixtureTruth returns the text the overlay is expected to show for a fixture:
// the fixture's own Truth when its runs contend for a cell, and the joined run
// text otherwise. A contested cell goes to the front-most run, so the run that
// loses it drops that grapheme from the visible text and the joined run text is
// no longer what the matcher can recover.
func fixtureTruth(fx OverlayFixture, runs []render.GlyphRun) string {
	if fx.Truth != "" {
		return fx.Truth
	}
	return runTruth(runs)
}

// runTruth joins the run texts in the order Recover walks them. Recover scans
// cells row-major (row outer, column inner), so the truth sorts the runs by row
// then column before joining. This holds whatever column each run starts in, so a
// two-column page (runs split across the left and right halves) still lines its
// truth up with the recovered text. Each run occupies one row in the committed
// fixtures, so a row-then-column sort reproduces the cell-walk order exactly.
func runTruth(runs []render.GlyphRun) string {
	ordered := append([]render.GlyphRun(nil), runs...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Row != ordered[j].Row {
			return ordered[i].Row < ordered[j].Row
		}
		return ordered[i].Col < ordered[j].Col
	})
	var b []byte
	for _, r := range ordered {
		b = append(b, r.Text...)
	}
	return string(b)
}

// textFlags builds the per-cell text flag slice from a composed grid: true for an
// overlay text cell, false for a bitmap cell. It feeds the existing CompareGolden
// flags argument so the overlay golden records the t/b column.
func textFlags(g block.Grid) []bool {
	flags := make([]bool, len(g.Cells))
	for i := range g.Cells {
		flags[i] = g.Cells[i].Text != ""
	}
	return flags
}

// TestOverlayGoldenTrack compares each overlay fixture's composed grid against
// its committed archive byte-exact, wiring the real per-cell text flags into
// CompareGolden. It keeps the amd64 skip the bitmap golden track uses, since the
// bg-average rounds differently on arm64.
func TestOverlayGoldenTrack(t *testing.T) {
	if runtime.GOARCH != goldenArch {
		t.Skipf("overlay golden archives are a %s snapshot; skipping byte comparison on %s", goldenArch, runtime.GOARCH)
	}
	update := updateGolden()
	for _, fx := range OverlayFixtures {
		t.Run(fx.Page, func(t *testing.T) {
			g, _ := composeOverlay(t, fx)
			ok, _, err := CompareGolden(overlayGoldenPath(fx.Page), fx.Page, g, textFlags(g), update)
			if err != nil {
				t.Fatalf("CompareGolden: %v", err)
			}
			if !ok {
				t.Fatalf("overlay golden mismatch for %s; rerun with UPDATE_GOLDEN=1 to refresh", fx.Page)
			}
		})
	}
}

// TestOverlayMatcherRoundTrip composes each overlay fixture, rasterises it,
// recovers the overlay text with the exact-template matcher, and hard-gates the
// CER at zero. A non-zero CER is a coverage, placement, or glyph-collision bug.
// The matcher is pure Go, so this runs under plain diagnostics with no ocr tag.
func TestOverlayMatcherRoundTrip(t *testing.T) {
	rast := newRasteriser(t)
	for _, fx := range OverlayFixtures {
		t.Run(fx.Page, func(t *testing.T) {
			g, truth := composeOverlay(t, fx)
			img := rast.Rasterise(g)
			if overlayArtefacts() {
				writeOverlayArtefact(t, fx.Page, img)
			}
			m := ocr.NewMatcher(rast, []string{truth})
			got := m.Recover(img, g)
			if cer := ocr.CER(truth, got); cer != 0 {
				t.Fatalf("matcher CER = %v for %s (got %q, want %q)", cer, fx.Page, got, truth)
			}
		})
	}
}

// TestOverlayWCAGGate counts the low-contrast overlay text cells per fixture and
// gates the count against the seeded baseline. The first run under
// UPDATE_BASELINE records the count into the overlay baseline rows; later runs
// fail when the measured count rises above the seeded value. It mirrors the
// CompareBaseline write-if-changed and the gate skip-on-nil pattern.
func TestOverlayWCAGGate(t *testing.T) {
	baseline, err := LoadBaseline(baselinePath())
	if err != nil {
		t.Fatalf("LoadBaseline: %v", err)
	}
	update := updateBaseline()

	for _, fx := range OverlayFixtures {
		g, _ := composeOverlay(t, fx)
		measured := LowContrastTextCells(g)
		t.Logf("overlay WCAG: page %s low-contrast text cells = %d", fx.Page, measured)

		seeded, ok := overlayLowContrastBaseline(baseline, fx.Page)
		switch {
		case update:
			baseline = upsertOverlayBaseline(baseline, fx.Page, measured)
		case !ok:
			t.Logf("overlay WCAG: page %s has no seeded baseline; run UPDATE_BASELINE=1 to seed", fx.Page)
		case measured > seeded:
			t.Errorf("overlay WCAG: page %s low-contrast text cells = %d, above seeded baseline %d", fx.Page, measured, seeded)
		}
	}

	if update && baseline != nil {
		ok, _, err := CompareBaseline(baselinePath(), *baseline, true)
		if err != nil {
			t.Fatalf("CompareBaseline: %v", err)
		}
		if !ok {
			t.Fatalf("CompareBaseline did not converge under UPDATE_BASELINE")
		}
	}
}

// overlayLowContrastBaseline returns the seeded low-contrast text-cell count for a
// page's overlay row and whether it exists and carries a non-nil count.
func overlayLowContrastBaseline(b *Baseline, page string) (int, bool) {
	if b == nil {
		return 0, false
	}
	m, ok := b.lookup(page, overlayMode)
	if !ok || m.LowContrastTextCells == nil {
		return 0, false
	}
	return *m.LowContrastTextCells, true
}

// upsertOverlayBaseline records the low-contrast text-cell count into the page's
// overlay baseline row, creating the baseline or the entry when absent. It
// returns the updated baseline. Existing non-overlay rows are untouched.
func upsertOverlayBaseline(b *Baseline, page string, count int) *Baseline {
	if b == nil {
		b = &Baseline{SchemaVersion: SchemaVersion, Grid: Viewport{Cols: CalibCols, Rows: CalibRows}}
	}
	n := count
	for i := range b.Entries {
		if b.Entries[i].Page == page && b.Entries[i].Mode == overlayMode {
			b.Entries[i].Metrics.LowContrastTextCells = &n
			return b
		}
	}
	b.Entries = append(b.Entries, BaselineEntry{
		Page:    page,
		Mode:    overlayMode,
		Metrics: BaselineMetrics{LowContrastTextCells: &n},
	})
	return b
}
