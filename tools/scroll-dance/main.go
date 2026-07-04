// Command scroll-dance reads a capture bundle's per-frame text-map.json sequence
// (the text runs a NYALA_CAPTURE bundle writes per frame, with on-screen box
// geometry) and scores overlay re-placement across the frames. It matches the same
// on-page run frame to frame by its Text and horizontal box position, classifies a
// run that holds its viewport cell as the page scrolls as fixed (a position:fixed or
// sticky run, never danced), and counts every other run as danced when its
// scroll-compensated placement takes two or more distinct values. A stable overlay
// that tracks a clean scroll scores zero danced runs; a run that jumps to a different
// column or document row dances. It quantifies the scroll-dance regression where
// overlay runs change cell placement between consecutive snapshot generations during
// scroll and rest misaligned. It reuses the metrics ScrollDance function, so the
// score matches the measured path. Run it via `just scroll-dance <path>`.
//
// The argument is a capture bundle directory (or its frames/ directory); the tool
// globs frames/frame-*/text-map.json in frame-number order and needs at least two
// frames to score. It reads each frame's page scroll offset from the sibling
// cdp-payload.json (scroll_offset_y), so the per-run scroll compensation uses each
// frame's own scroll rather than a shared shift, and reads the horizontal identity
// bucket and the cell height from the bundle's performance.json cell_px (falling back
// to 8x16), so runs match within roughly one cell of horizontal jitter and a document
// row is snapped with the real cell height. It prints the frame count and the
// aggregates: the matched-run count, the fixed-run count, the danced-run count, the
// total re-placement count, and the largest distinct-placement count any single
// non-fixed run took.
//
// With -gate the tool exits non-zero when the danced-run count is above -danced or
// the total re-placement count is above -replacements. Without -gate it only
// prints.
//
// With -thresholds <fixture.json> the gate defaults come from the file's optional
// scroll_dance_gate object; absent object keys fall back to the -danced and
// -replacements flag values. A missing or unreadable file named by the flag is an
// error.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/wimpysworld/nyala/internal/metrics"
)

// mapName is the per-frame text-map.json filename the bundle writes.
const mapName = "text-map.json"

// payloadName is the per-frame cdp-payload.json filename the scroll offset reads
// from. It sits beside text-map.json in each frame directory.
const payloadName = "cdp-payload.json"

// perfName is the bundle-root performance.json filename the horizontal bucket and
// the cell height read their cell size from.
const perfName = "performance.json"

// fallbackBucketPx is the horizontal identity bucket in CSS pixels used when the
// bundle's performance.json is absent; headless capture is deterministic at an 8px
// cell.
const fallbackBucketPx = 8

// fallbackCellH is the cell height in CSS pixels used when the bundle's
// performance.json is absent, matching the deterministic headless 16px cell height.
const fallbackCellH = 16

// entry mirrors one text-map.json record: the metric reads Col, Row, Text, X, and
// Y, and the rest is decoded but unused. A local struct keeps the tool free of the
// render seam, which the capture writer owns.
type entry struct {
	Col  int     `json:"Col"`
	Row  int     `json:"Row"`
	Text string  `json:"Text"`
	X    float64 `json:"X"`
	Y    float64 `json:"Y"`
}

// perfDoc mirrors the fields of performance.json the tool reads: the first frame's
// cell_px, whose width sets the horizontal identity bucket and whose height sets the
// document-row cell, and its optional layout_scale Z. The rest of the document is
// decoded but unused. A nil LayoutScale (absent field) means Z=1.
type perfDoc struct {
	Frames []struct {
		CellPx      [2]int   `json:"cell_px"`
		LayoutScale *float64 `json:"layout_scale"`
	} `json:"frames"`
}

func main() {
	gate := flag.Bool("gate", false, "exit non-zero when a threshold is breached")
	dancedMax := flag.Int("danced", 0, "gate: fail when the danced-run count is above this")
	replacementsMax := flag.Int("replacements", 0, "gate: fail when the total re-placement count is above this")
	thresholds := flag.String("thresholds", "", "gate: fixture.json whose scroll_dance_gate object overrides the flag defaults")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: scroll-dance [flags] <bundle-dir | frames-dir>\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	gc := gateConfig{on: *gate, danced: *dancedMax, replacements: *replacementsMax}
	if *thresholds != "" {
		if err := applyThresholds(&gc, *thresholds); err != nil {
			fmt.Fprintf(os.Stderr, "scroll-dance: %v\n", err)
			os.Exit(1)
		}
	}
	if err := run(flag.Arg(0), gc); err != nil {
		fmt.Fprintf(os.Stderr, "scroll-dance: %v\n", err)
		os.Exit(1)
	}
}

// gateConfig holds the gate mode switch and its thresholds.
type gateConfig struct {
	on           bool
	danced       int
	replacements int
}

// gateDoc mirrors the scroll_dance_gate object in a fixture.json: the per-fixture
// gate override. A pointer per field distinguishes an absent key (keep the flag
// default) from a present zero value.
type gateDoc struct {
	ScrollDanceGate *struct {
		Danced       *int `json:"danced"`
		Replacements *int `json:"replacements"`
	} `json:"scroll_dance_gate"`
}

// applyThresholds reads the fixture.json at path and overrides gc's thresholds
// from its scroll_dance_gate object. A present field overrides the flag default;
// an absent field keeps it. A missing or unreadable file, or invalid JSON, is an
// error. A readable file with no scroll_dance_gate object leaves gc unchanged.
func applyThresholds(gc *gateConfig, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc gateDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	if doc.ScrollDanceGate == nil {
		return nil
	}
	if doc.ScrollDanceGate.Danced != nil {
		gc.danced = *doc.ScrollDanceGate.Danced
	}
	if doc.ScrollDanceGate.Replacements != nil {
		gc.replacements = *doc.ScrollDanceGate.Replacements
	}
	return nil
}

// run resolves arg to the bundle's frame directories, decodes each frame's runs,
// reads the horizontal bucket from the bundle performance.json, scores the
// sequence with metrics.ScrollDance, and prints the aggregates. In gate mode it
// returns an error when an aggregate breaches its threshold. It fails when the
// bundle holds fewer than two frame text maps or a text map is not valid JSON.
func run(arg string, gc gateConfig) error {
	mapPaths, err := resolveFrames(arg)
	if err != nil {
		return err
	}
	if len(mapPaths) < 2 {
		return fmt.Errorf("need at least two frame %s files to score a sequence, found %d", mapName, len(mapPaths))
	}
	frames := make([][]metrics.ScrollRun, len(mapPaths))
	scrollY := make([]float64, len(mapPaths))
	for i, p := range mapPaths {
		runs, rerr := readRuns(p)
		if rerr != nil {
			return rerr
		}
		frames[i] = runs
		scrollY[i] = readScrollY(p)
	}
	bucket, cellH := readCellCSS(mapPaths[0])
	s := metrics.ScrollDance(frames, scrollY, cellH, bucket)
	nonFixed := s.Runs - s.FixedRuns
	fmt.Printf("frames:               %d\n", len(mapPaths))
	fmt.Printf("bucket px:            %.1f\n", bucket)
	fmt.Printf("cell height px:       %.1f\n", cellH)
	fmt.Printf("matched runs:         %d\n", s.Runs)
	fmt.Printf("fixed runs:           %d\n", s.FixedRuns)
	fmt.Printf("danced runs:          %d\n", s.DancedRuns)
	fmt.Printf("re-placements:        %d\n", s.Placements-nonFixed)
	fmt.Printf("max placements/run:   %d\n", s.MaxPlacements)
	if gc.on {
		return checkGate(s, gc)
	}
	return nil
}

// checkGate returns an error naming every threshold the stats breach, or nil when
// all pass. The re-placement count is Placements minus the non-fixed matched runs
// (FixedRuns contribute no placement), so a glued scrolling overlay scores zero.
func checkGate(s metrics.ScrollDanceStats, gc gateConfig) error {
	if s.DancedRuns > gc.danced {
		return fmt.Errorf("gate: danced runs %d > %d", s.DancedRuns, gc.danced)
	}
	replacements := s.Placements - (s.Runs - s.FixedRuns)
	if replacements > gc.replacements {
		return fmt.Errorf("gate: re-placements %d > %d", replacements, gc.replacements)
	}
	return nil
}

// readRuns decodes one frame's text-map.json into the metric's run type.
func readRuns(path string) ([]metrics.ScrollRun, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	runs := make([]metrics.ScrollRun, len(entries))
	for i, e := range entries {
		runs[i] = metrics.ScrollRun{Text: e.Text, Col: e.Col, Row: e.Row, X: e.X, Y: e.Y}
	}
	return runs, nil
}

// readCellCSS reads the horizontal identity bucket and the document-row cell height
// from the bundle-root performance.json cell_px, resolved from a frame's text-map.json
// path (its directory's parent's parent, since frames live in frames/frame-*/). The
// cell_px is in terminal device pixels; the text map's box geometry is in CSS pixels,
// so both are divided by the frame's layout_scale Z to reach CSS pixels, matching the
// placement-score convention. It falls back to 8x16 CSS pixels when the file is
// absent, unreadable, carries no frame record, or reports a non-positive cell size.
func readCellCSS(mapPath string) (bucketPx, cellH float64) {
	frameDir := filepath.Dir(mapPath)
	bundleRoot := filepath.Dir(filepath.Dir(frameDir))
	raw, err := os.ReadFile(filepath.Join(bundleRoot, perfName))
	if err != nil {
		return fallbackBucketPx, fallbackCellH
	}
	var doc perfDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fallbackBucketPx, fallbackCellH
	}
	if len(doc.Frames) == 0 || doc.Frames[0].CellPx[0] <= 0 || doc.Frames[0].CellPx[1] <= 0 {
		return fallbackBucketPx, fallbackCellH
	}
	scale := 1.0
	if doc.Frames[0].LayoutScale != nil && *doc.Frames[0].LayoutScale > 0 {
		scale = *doc.Frames[0].LayoutScale
	}
	return float64(doc.Frames[0].CellPx[0]) / scale, float64(doc.Frames[0].CellPx[1]) / scale
}

// readScrollY reads a frame's page vertical scroll offset in CSS pixels from the
// sibling cdp-payload.json (scroll_offset_y), resolved from the frame's text-map.json
// path. A missing or unreadable payload, or invalid JSON, reads as scroll offset 0,
// so a bundle without the payload scores as an unscrolled sequence rather than
// failing.
func readScrollY(mapPath string) float64 {
	payload := filepath.Join(filepath.Dir(mapPath), payloadName)
	raw, err := os.ReadFile(payload)
	if err != nil {
		return 0
	}
	var doc struct {
		ScrollOffsetY float64 `json:"scroll_offset_y"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0
	}
	return doc.ScrollOffsetY
}

// resolveFrames maps arg to the ordered text-map.json paths under it. arg is a
// bundle directory (holding frames/) or the frames/ directory itself. It globs
// frames/frame-*/text-map.json, sorts by path so the frame-number order holds, and
// fails when the path does not exist or holds no frame text maps.
func resolveFrames(arg string) ([]string, error) {
	info, err := os.Stat(arg)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory; pass a capture bundle or its frames directory", arg)
	}
	roots := []string{filepath.Join(arg, "frames"), arg}
	for _, root := range roots {
		matches, gerr := filepath.Glob(filepath.Join(root, "frame-*", mapName))
		if gerr != nil {
			return nil, gerr
		}
		if len(matches) > 0 {
			sort.Strings(matches)
			return matches, nil
		}
	}
	return nil, fmt.Errorf("no frames/frame-*/%s under %s", mapName, arg)
}
