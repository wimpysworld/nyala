# Stage 2 - Crisp text overlay

Stage 2 builds the crisp text overlay on the block path. It composites positioned glyph runs over the bitmap, so page text renders as real Unicode glyphs where text exists and as block cells everywhere else. This is the make-or-break differentiator from browsh-grade fuzzy text, and the risk Stage 2 kills is the compositor itself. One combined cell grid holds the bitmap fill and an optional grapheme; the grapheme wins the cell, with no separate z-buffer. The input stays the static PNG from Stage 0/1, now carrying hardcoded `GlyphRun`s. There is no browser yet; the live text source is Stage 3. The demo is harness-only: the interactive viewer renders no overlay in Stage 2. This document is the durable record of the completed work.

## Status summary

Stage 2 is code-complete. The compositor, the matcher, the font fallback, the run-set fixtures, the four-measure harness grade, and the new `just` recipes are in the tree.

Acceptance criteria are met. The compositor folds the overlay into `block.Compose`, grapheme-wins-the-cell, with the foreground from the run and the background from the whole-cell bitmap average. All three consumers (`capture.Rasteriser.Rasterise`, `block.Renderer.Render`, the viewer `writeGrid`) prefer `Cell.Text` over `Cell.Glyph`. The overlay grades through four measures with gating: the overlaid-grid golden hard-gates byte-exact, the matcher hard-gates `CER == 0`, the WCAG low-contrast text-cell count gates against `baseline.json`, and the gosseract differentiator was reported, not gated (since removed: the gosseract engine and this differentiator test are gone, so the matcher `CER == 0` gate is the live read-back check).

The KMSCON quadrant fallback was verified by the owner on a real KMSCON TTY via `just kmscon-testcard` (an owner-eyeball check, like Stage 0's terminal verification, not an automated gate).

What remains for later stages: the diff writer, REP, and the text-only monochrome readout are deferred; the chromedp screencast replaces the PNG in Stage 3; and Bubble Tea chrome wraps the viewer in Stage 4. The Sixel and Kitty overlay is retired because the graphics backends render the page bitmap with its own text and Stage 4 has no separate overlay consumer.

This paragraph records the Stage 2 boundary. Stage 3 later delivered the Chromium
screencast and Block grid diff writer; current open work is tracked in Linear.

## Objective and scope

The objective: prove the grapheme-wins-the-cell compositor on the block path, the permanent KMSCON floor, and grade it through the harness. The overlay renders each glyph as a full crisp terminal cell, always legible whatever the page text size, so it clears the bitmap's `minLegiblePx` floor (Stage 0 measured 10 px, with 8 px illegible). The Stage 2 headline is placement correctness plus the differentiator: the overlay reads text where the bitmap floor failed.

In scope, all delivered:

- Extend `block.Cell` with `Text` and `Wide`, and fold the overlay into `block.Compose`.
- Read `Frame.Text` cell-positioned runs; place each run at its cell with no binning.
- Grapheme segmentation (`rivo/uniseg`) and display width (`mattn/go-runewidth`), with wide-glyph occupancy and edge clipping.
- Text-cell colour: foreground from the run, background from the whole-cell bitmap average.
- A pure-Go exact-template nearest-glyph matcher (`internal/ocr/matcher.go`).
- A KMSCON quadrant font fallback (`font.Atlas.QuadrantSupport`).
- Run-set fixtures under `testdata/overlay/*.runs` and a loader.
- The four-measure grade folded into `just diagnostics`, the overlay goldens, the overlay baseline rows, the `just overlay` artefact recipe, and the `just kmscon-testcard` owner check.

Out of scope:

- The diff writer and REP (Stage 3; static PNG input has no motion, so a per-frame diff saves nothing).
- The text-only monochrome readout (later refinement).
- The overlay over Sixel and Kitty. This was deferred in Stage 2, then retired when Stage 4 no longer needed a separate graphics overlay.
- The DOM text source and cell-binning (Stage 3); Stage 2 feeds hardcoded runs.
- The luma-distance background nudge, dither, and the nearest-of-two colour converter (opt-in or off by default; promoted only if a measured case demands it). Each is promoted in a future stage only after a harness A/B on its fixture: the luma nudge after a thin-glyph-on-photo contrast measure, the nearest-of-two converter after an A/B on a text and a photo fixture, and dither after a text-OCR versus photo-banding benchmark (per-region only).
- Bidi reorder: Stage 2 accepts CDP visual-order positions; there is no DOM to test against until Stage 3.

The target architecture and later-stage research are in [`OVERVIEW.md`](./OVERVIEW.md).

## The seam and the combined grid

The seam is unchanged: `block.Compose` takes the same `Frame` and now reads its `Text` field. `Frame.Text` has been present since Stage 0 and no renderer read it; Stage 2 is the first reader. Nothing above the seam changes.

One cell grid holds the bitmap fill and an optional grapheme plus its colour. A grapheme present means a text cell and emits the glyph; absent means emit the block. There is no separate z-buffer: the grapheme overriding the block collapses cell ownership, z-order, and block suppression into one branch. This is Browsh's indexed-grid model and Carbonyl's `Cell{quadrant, grapheme}`, ported.

`block.Cell` gained two fields:

```go
type Cell struct {
    Glyph rune       // block glyph for this cell
    FG    color.RGBA // foreground colour
    BG    color.RGBA // background colour
    Text  string     // overlay grapheme; "" means a bitmap cell
    Wide  bool       // trailing cell of a wide (width-2) overlay glyph
}
```

The overlay lives inside the block `Grid`, so all three consumers and the Route A rasterise read it from one place. The zero value (`Text == ""`, `Wide == false`) is a plain bitmap cell, so an empty `Frame.Text` composes exactly as Stage 0/1 did.

## The compositor

`Compose` composites the bitmap onto a fresh grid (quadrant or half-block), then folds the overlay on in a second pass via `applyOverlay` (`internal/render/block/overlay.go`). The overlay reuses the same source pixels the bitmap pass already samples, so the background average needs no second decode.

`applyOverlay` walks each `GlyphRun`, segments its text into grapheme clusters, and places one cluster per cell starting at `(Col, Row)`:

- The cluster overwrites the bitmap glyph in its cell. The grapheme wins; the original `Glyph` is kept on the cell but `Text` is preferred by every consumer.
- The foreground is the run colour. The background comes from `cellBGAverageFrom`: the exact whole-cell average of the source pixels under the cell, in RGBA order. The glyph sits on its local page background, so contrast holds by construction at near-zero cost.
- A run past the grid edge is clipped. A negative start column is skipped cell by cell until the run reaches column 0.

Cell positions are the producer's contract, not a compositor step. `GlyphRun.Col, Row` is already a cell address, so the compositor places each run at its cell and does no binning. The `floor(px/cellW)`, `floor(py/cellH)` rule that maps pixels to cells is the producer's job (the Stage 2 fixtures, Stage 3's DOM mapping later). Floor, never round: floor keeps the text and the quadrant bitmap on one monotonic, non-overlapping partition, so no glyph drifts into a neighbour.

Forward note: Stage 3's producer snap is now round, not floor. Page insets are arbitrary sub-cell offsets, so round centres the residue at zero. Round preserves the monotonic non-overlapping partition (order-preserving), the property this rule protected.

## Grapheme width and wide glyphs

Each grapheme is segmented with `rivo/uniseg` (cluster segmentation: ZWJ, emoji) and measured with `mattn/go-runewidth` (display width, clamped to 1 or 2). A width-0 cluster (a standalone combining mark) is treated as width 1, so it still occupies a cell.

A width-2 glyph (CJK, emoji) claims two cells: its lead cell carries the grapheme, the trailing cell is marked `Wide` so the block fill never paints into it and the consumers write a single space there. Never half-emit a wide glyph: when a 2-cell glyph would cross the viewport right edge, the compositor writes a single background-coloured space in the last cell instead. Terminals disagree on a clipped wide glyph (wrap, overflow, drop), so half-emitting is undefined and corrupts the grid; a space is always exactly one cell. Clip, do not half-emit.

`rivo/uniseg` and `mattn/go-runewidth` are pure Go and were already in the tree as indirect Charm dependencies, so the overlay pulls in no cgo. Stage 2 promotes them to direct requires.

## The three consumers

`Cell.Text` and `Cell.Wide` wire into all three consumers, which prefer `Text` over `Glyph` and write a space for a `Wide` trailing cell:

- `block.Renderer.Render` writes each grid cell as truecolour SGR. The shared body is `renderGrid`, split out so a hand-built grid renders without a compose pass; `writeCellText` picks the overlay grapheme, the bitmap glyph, or the wide-cell space.
- `capture.Rasteriser.Rasterise` rasterises the grid back to a PNG for grading. `drawCell` now draws `c.Text` when set, else the bitmap glyph, returns early for a `Wide` cell, and routes the glyph through a new shared `drawText`. A new `RenderGlyphCell` draws one grapheme to a single cell tile through the same face path, so the matcher templates draw through the same face the cells do.
- The viewer `writeGrid` carries the same three-way branch. Stage 2 feeds no overlay to the viewer (the demo is harness-only), so a `Text` or `Wide` cell never reaches it yet; the branch readies Stage 3 and keeps the Stage 1 truecolour, xterm-256, and monochrome colour paths.

## The exact-template matcher

The exact-template nearest-glyph matcher (`internal/ocr/matcher.go`) reads the crisp overlay back from the rasterised PNG. It is pure Go and carries no build tag, so it builds and unit-tests under the routine gate. (At the time, only the gosseract engine needed cgo; since removed: the gosseract engine is gone, so the whole tree is cgo-free.)

For each candidate grapheme it renders a template through `Rasteriser.RenderGlyphCell`, the same face path that draws the graded cells, then binarises it. To match a cell it binarises the cell by an adaptive Otsu split and picks the candidate with the nearest template by Hamming distance.

Three properties make it a placement-and-coverage check, not general OCR:

- The candidate set is fixed and grid-independent: the distinct graphemes of the run text plus the block glyph set (the ten quadrant glyphs U+2596..U+259F, the lower-half-block U+2584, the full block U+2588, and space). The fixed block glyphs let the matcher catch a block showing through where the overlay should have won.
- The match is polarity-invariant: each cell compares against every template and its inverse, taking the smaller distance, so a dark-on-light cell and a light-on-dark cell recover the same glyph. The run foreground colour does not affect the match.
- `Recover` walks the overlay text cells in placement order (row-major), skipping bitmap cells and wide trailing cells, and joins the recovered graphemes for CER scoring against the run truth.

A near-zero CER proves each run landed in the right cells with the right glyphs; a non-zero CER is a coverage, placement, or glyph-collision bug.

## The KMSCON font fallback

A bare KMSCON TTY has no pixel readback, so an automatic bleed probe is not buildable. Stage 2 handles KMSCON in two parts: a built and unit-tested font fallback, and a manual owner-eyeball test card.

`font.Atlas.QuadrantSupport` reports the highest tier the atlas reaches for the quadrant block set U+2596..U+259F, with no runtime terminal probe. It checks coverage through `sfnt.GlyphIndex` on each font in turn and returns `QuadrantPrimary` (FiraCode Nerd Font Mono covers the set), `QuadrantUnifont` (only GNU Unifont covers it), or `QuadrantHalfBlock` (neither, so the half-block floor U+2584 applies). The fallback is built and unit-tested in Stage 2; wiring it into a scheme switch in the renderer is left for the stage that ships the KMSCON-only path, since the pinned FiraCode primary already covers the set on every target so far.

`just kmscon-testcard` (`tools/kmscon-testcard`) prints the ten quadrant glyphs plus a half-block floor row across contrasting colour pairs, two abutting rows each, so inter-row bleed shows as a seam. It draws to stdout and asks the operator to look, so it cannot run headless; the owner ran it on a real KMSCON console and confirmed no bleed.

## Run-set fixtures

The five committed PNGs are reused; no new fixtures. Each gets one run set under `testdata/overlay/*.runs`, one run per line as `col row fgHex text`, where the text runs to the end of the line so it may contain spaces. `LoadRunSet` (`internal/harness/overlay.go`) parses a file into `render.GlyphRun`s, skipping blank and `#` comment lines and erroring on a malformed line. `tools/genruns` generates the run sets.

The run text reuses the synthetic OCR ladder strings, known truth at known positions. The WCAG thin-glyph case is runs placed over the photo regions of `testpage-04` and `linuxmatters`, not a new fixture.

## Grading the overlay

The harness grades the overlay through four measures, three gated and one reported. The grade folds into `just diagnostics`; it composes each fixture at the pinned 120x40 grid with its run set folded on, the same downscale geometry the bitmap golden track uses.

- **Overlaid-grid golden** (`TestOverlayGoldenTrack`): the composed `Grid` against a committed `txtar` archive per page (`testdata/golden/overlay/*.txtar`), byte-exact. It wires the per-cell text flag into the existing `CompareGolden`, so the archive records which cells are overlay text. Hard-gated.
- **Matcher** (`TestOverlayMatcherRoundTrip`): rasterise the grid, recover the overlay text with the exact-template matcher, score CER against the run truth. Hard-gated on `CER == 0`.
- **WCAG contrast** (`TestOverlayWCAGGate`): `LowContrastTextCells` counts the overlay text cells whose foreground-against-background contrast falls below the WCAG AA 4.5:1 bound. The count gates against the seeded overlay rows in `baseline.json`; a rise fails.
- **Gosseract differentiator** (`TestOverlayGosseractDifferentiator`, since removed): built the OCR ladder twice, once as a crisp overlay grid and once as the Stage 0 bitmap-only floor, OCR'd both through one gosseract engine, and reported the overlay CER against the bitmap CER. The overlay reads page text where the bitmap floor's 10 px limit fails, so the overlay CER sat well below. Reported, never gated: Tesseract output varies and a tuned threshold would flake. It carried both the `diagnostics` and `ocr` tags and skipped when no engine was present. The gosseract engine and this test have since been removed; the matcher `CER == 0` gate remains the live read-back check.

## Decisions recorded with rationale

- Single combined grid, grapheme wins, no z-buffer. The grapheme overriding the block collapses cell ownership, z-order, and block suppression into one branch. Ported from Browsh and Carbonyl.
- Text-cell background is the whole-cell bitmap average, not a luma-distance nudge. The glyph sits on its local page background, so contrast holds by construction at near-zero cost. The nudge stays opt-in and off by default; it rescues thin glyphs on busy backgrounds but adds per-cell work and, for the DOM-background variant, a source not wired until Stage 3.
- Cell positions are the producer's contract. `GlyphRun.Col, Row` arrives as a cell address; the compositor does no binning. The snap rule lives upstream. Stage 3 moved it from floor to round: page insets are arbitrary sub-cell offsets, so round centres the residue at zero while preserving the monotonic non-overlapping partition (order-preserving).
- Wide glyphs claim N cells and clip, never half-emit. A clipped wide glyph at the right edge writes a single background space, because terminals disagree on a half-clipped wide glyph.
- The matcher is an exact-template placement-and-coverage check, not general OCR. Its candidate set is fixed (run graphemes plus the block glyphs), it is polarity-invariant, and it gates on `CER == 0`. This is the distinct role the matcher was deferred from Stage 0 to fill: it needs the crisp glyph cells the Stage 2 overlay provides.
- Gate the deterministic measures, report the variable one. The golden, the matcher, and the WCAG count are deterministic and hard-gated; the gosseract differentiator varied and was reported (since removed).
- Quadrant default, half-block floor, with a built font fallback and a manual KMSCON card. There is no automatic bleed probe on a bare TTY, so the fallback is decided from font coverage (`sfnt.GlyphIndex`) and the bleed is owner-verified on a real console.
- Stage 2 keeps the overlay on the block path. Its risk is the grapheme-wins compositor, which lives on the block path (the KMSCON floor). The graphics backends carry no text risk, so they ignore `Frame.Text`, as all three did before. Burning glyphs into the bitmap is rejected permanently: it re-rasterises text into fuzzy pixels, the browsh failure mode Nyala exists to avoid. The later Stage 4 scope retired the graphics-overlay path because it has no remaining consumer.
- The viewer renders no overlay in Stage 2. The demo is harness-only; the viewer `writeGrid` branch is added now to ready Stage 3 but sees no text cells yet.

## Acceptance criteria and how each was met

- The compositor folds the overlay into `block.Compose`, grapheme-wins-the-cell, foreground from the run, background from the whole-cell bitmap average: met. `applyOverlay` runs as the second pass of `Compose`; `block/overlay_test.go` pins grapheme-wins, the background average, and clipping.
- All three consumers prefer `Cell.Text` over `Cell.Glyph` and handle a `Wide` cell: met. `block.Renderer.Render`/`writeCellText`, `capture.Rasteriser.drawCell`, and the viewer `writeGrid` all carry the branch, each with a test.
- Wide glyphs claim two cells and never half-emit at the edge: met. `applyOverlay` marks the trailing `Wide` cell or writes a background space at the right edge; `block/overlay_test.go` covers both.
- The overlaid-grid golden matches byte-exact: met. `TestOverlayGoldenTrack` compares each fixture against its committed archive, with the amd64 skip the bitmap golden track uses.
- The matcher recovers the overlay text at `CER == 0`: met. `TestOverlayMatcherRoundTrip` hard-gates zero CER over the five fixtures; `matcher_test.go` unit-tests the matcher.
- The WCAG low-contrast text-cell count holds against the baseline: met. `TestOverlayWCAGGate` gates the count against the seeded overlay rows in `baseline.json`.
- The gosseract differentiator shows the overlay beats the bitmap floor: met and reported (since removed). `TestOverlayGosseractDifferentiator` logged the overlay CER against the bitmap CER; it was not gated. The gosseract engine and this test have since been removed.
- The KMSCON quadrant font fallback is built and unit-tested, and the test card is owner-verified: met. `font.Atlas.QuadrantSupport` plus `font_test.go`; `just kmscon-testcard` confirmed by the owner on a KMSCON TTY.

## Risks identified and how they were resolved or carried forward

- RGBA versus BGRA byte order: resolved. The whole-cell background average reads source pixels as RGBA, never BGRA, matching the repo-wide invariant.
- KMSCON inter-row bleed with no pixel readback: resolved by split. The font fallback is decided from coverage (`sfnt.GlyphIndex`), and the bleed is owner-verified via `just kmscon-testcard`, since a bare TTY cannot be probed automatically.
- arm64 floating-point divergence in the background average: carried forward as accepted. The bg-average rounds differently on arm64, so `TestOverlayGoldenTrack` keeps the amd64 skip the bitmap golden track uses; regenerate the overlay goldens on amd64 only.
- cgo-free shipped binary: resolved. The compositor's new dependencies (`rivo/uniseg`, `mattn/go-runewidth`) are pure Go, and the matcher is pure Go. (At the time, only the gosseract differentiator needed cgo, behind the `ocr` tag; since removed: it is gone, so the tree is cgo-free.)
- Matcher false matches between similar glyphs: resolved by scope. The candidate set is fixed and small, the match is polarity-invariant, and the gate is `CER == 0`, so any collision fails the gate loudly rather than passing silently.
