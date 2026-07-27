// The overlay run-set loader and the WCAG low-contrast text-cell count.
//
// LoadRunSet parses a committed .runs file into render.GlyphRuns; the grading
// harness composes each fixture with its run set, then grades the overlay.
// LowContrastTextCells counts overlay text cells whose foreground/background
// contrast falls below the WCAG AA bound, the legibility co-signal the gate
// tracks. Both are pure Go and untagged, so the routine gate compiles them and
// the diagnostics grade reuses them.
//
// Channel order is RGBA throughout.
package harness

import (
	"bufio"
	"fmt"
	"image/color"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
)

// OverlayFixture pairs a page with its source PNG and its overlay run-set file.
// The grading harness composes each page's bitmap with its runs, then grades the
// overlay placement against the four measures.
type OverlayFixture struct {
	// Page is the fixture name, for example "linuxmatters".
	Page string
	// PNG is the source image filename under testdata/.
	PNG string
	// Runs is the run-set filename under testdata/overlay/.
	Runs string
	// Truth is the text the fold leaves visible, in the order the matcher walks
	// the cells. It is empty for a fixture whose runs never contend for a cell,
	// where the joined run text is already what the overlay shows. A fixture
	// whose runs overlap states it, because the run that loses a contested cell
	// loses that grapheme from the visible text.
	Truth string
}

// OverlayFixtures lists the committed pages and their run sets. The first five
// mirror the goldenFixtures order, so the overlay grade runs the same corpus.
// The sixth reuses the linuxmatters page to carry a contended run set: the other
// five are the identity permutation on distinct rows, so their fold takes the
// no-copy fast path and no cell is ever contested, which leaves the whole grade
// blind to paint order.
var OverlayFixtures = []OverlayFixture{
	{Page: "linuxmatters", PNG: "linuxmatters.png", Runs: "linuxmatters.runs"},
	{Page: "testpage-01-grayscale", PNG: "testpage-01-grayscale.png", Runs: "testpage-01-grayscale.runs"},
	{Page: "testpage-02-basic", PNG: "testpage-02-basic.png", Runs: "testpage-02-basic.runs"},
	{Page: "testpage-03-cmyk", PNG: "testpage-03-cmyk.png", Runs: "testpage-03-cmyk.runs"},
	{Page: "testpage-04-color", PNG: "testpage-04-color.png", Runs: "testpage-04-color.runs"},
	{
		Page: "overlay-contended",
		PNG:  "linuxmatters.png",
		Runs: "overlay-contended.runs",
		// "BEHIND RUN" holds columns 14 to 23 of row 12 at rank 1, and "FRONT RUN"
		// holds columns 22 to 30 at rank 0, so the two overlap on columns 22 and
		// 23 and the front-most run keeps them: the behind run shows "BEHIND R"
		// and loses its "UN". The run set is out of rank order, so the fold sorts
		// rather than taking the no-copy fast path.
		//
		// The overlap sits exactly on the page's dark-to-light boundary, so the
		// two runs carry different foregrounds and the contested cells name their
		// winner in the golden archive as well as in the recovered text. It also
		// keeps every visible cell legible: the behind run's white text never
		// lands on the light side, because the only cells where it would are the
		// two it loses.
		Truth: "BEHIND RFRONT RUNUNCONTESTED LINE",
	},
}

// OverlayDir is the directory under testdata/ that holds the committed run-set
// files, relative to the harness package.
func OverlayDir() string {
	return filepath.Join("..", "..", "testdata", "overlay")
}

// LoadRunSet parses a .runs file into a slice of render.GlyphRun. Each non-blank,
// non-comment line is "col row fgHex rank text": col and row are cell coordinates,
// fgHex is a six-digit RRGGBB foreground (alpha forced opaque), rank is the
// front-to-back stacking rank with 0 front-most, and text runs to the end of the
// line so it may contain spaces. The rank field is required. Lines starting with
// # and blank lines are skipped. It returns an error on a malformed line.
func LoadRunSet(path string) ([]render.GlyphRun, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var runs []render.GlyphRun
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		raw := sc.Text()
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		run, err := parseRun(trimmed)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		runs = append(runs, run)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return runs, nil
}

// parseRun parses one "col row fgHex rank text" line into a render.GlyphRun. The
// text field is everything after the fourth space, so it keeps internal spaces.
// The rank field is required: an optional middle field would be ambiguous,
// because a run whose text begins with an integer would parse either way.
func parseRun(s string) (render.GlyphRun, error) {
	fields := strings.SplitN(s, " ", 5)
	if len(fields) < 5 {
		return render.GlyphRun{}, fmt.Errorf("want \"col row fgHex rank text\", got %q", s)
	}
	col, err := strconv.Atoi(fields[0])
	if err != nil {
		return render.GlyphRun{}, fmt.Errorf("col: %w", err)
	}
	row, err := strconv.Atoi(fields[1])
	if err != nil {
		return render.GlyphRun{}, fmt.Errorf("row: %w", err)
	}
	fg, err := parseHexRGB(fields[2])
	if err != nil {
		return render.GlyphRun{}, fmt.Errorf("fgHex: %w", err)
	}
	rank, err := strconv.Atoi(fields[3])
	if err != nil {
		return render.GlyphRun{}, fmt.Errorf("rank: %w", err)
	}
	return render.GlyphRun{Col: col, Row: row, Text: fields[4], FG: fg, StackRank: rank}, nil
}

// parseHexRGB parses a six-digit RRGGBB hex string into an opaque color.RGBA.
// Channel order is RGBA; alpha is forced opaque.
func parseHexRGB(s string) (color.RGBA, error) {
	if len(s) != 6 {
		return color.RGBA{}, fmt.Errorf("want 6 hex digits, got %q", s)
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return color.RGBA{}, err
	}
	return color.RGBA{
		R: byte((v >> 16) & 0xff),
		G: byte((v >> 8) & 0xff),
		B: byte(v & 0xff),
		A: 0xff,
	}, nil
}

// LowContrastTextCells counts the overlay text cells of g whose WCAG contrast
// ratio falls below metrics.WCAGThreshold (4.5:1). A text cell is one with a
// non-empty Text that is not a Wide trailing cell; the count ignores bitmap
// cells and wide trailing cells, which carry no glyph. It reads each cell's
// foreground against its background in RGBA order.
func LowContrastTextCells(g block.Grid) int {
	var n int
	for i := range g.Cells {
		c := g.Cells[i]
		if c.Text == "" || c.Wide {
			continue
		}
		ratio := metrics.WCAGContrast(c.FG.R, c.FG.G, c.FG.B, c.BG.R, c.BG.G, c.BG.B)
		if ratio < metrics.WCAGThreshold {
			n++
		}
	}
	return n
}
