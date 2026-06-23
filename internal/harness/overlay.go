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
}

// OverlayFixtures lists the five committed pages and their run sets, mirroring
// the goldenFixtures order so the overlay grade runs the same corpus.
var OverlayFixtures = []OverlayFixture{
	{"linuxmatters", "linuxmatters.png", "linuxmatters.runs"},
	{"testpage-01-grayscale", "testpage-01-grayscale.png", "testpage-01-grayscale.runs"},
	{"testpage-02-basic", "testpage-02-basic.png", "testpage-02-basic.runs"},
	{"testpage-03-cmyk", "testpage-03-cmyk.png", "testpage-03-cmyk.runs"},
	{"testpage-04-color", "testpage-04-color.png", "testpage-04-color.runs"},
}

// OverlayDir is the directory under testdata/ that holds the committed run-set
// files, relative to the harness package.
func OverlayDir() string {
	return filepath.Join("..", "..", "testdata", "overlay")
}

// LoadRunSet parses a .runs file into a slice of render.GlyphRun. Each non-blank,
// non-comment line is "col row fgHex text": col and row are cell coordinates,
// fgHex is a six-digit RRGGBB foreground (alpha forced opaque), and text runs to
// the end of the line so it may contain spaces. Lines starting with # and blank
// lines are skipped. It returns an error on a malformed line.
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

// parseRun parses one "col row fgHex text" line into a render.GlyphRun. The text
// field is everything after the third space, so it keeps internal spaces.
func parseRun(s string) (render.GlyphRun, error) {
	fields := strings.SplitN(s, " ", 4)
	if len(fields) < 4 {
		return render.GlyphRun{}, fmt.Errorf("want \"col row fgHex text\", got %q", s)
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
	return render.GlyphRun{Col: col, Row: row, Text: fields[3], FG: fg}, nil
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
