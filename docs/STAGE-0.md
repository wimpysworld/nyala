# Stage 0 - Image-based rendering proof of concept

Stage 0 is built, committed, and verified. It proves three swappable rendering backends sit behind one `Renderer` seam, that a static PNG renders through each, and that objective measurement plus OCR legibility tracking gate the result. A no-graphics terminal shows the bitmap as quadrant blocks; a graphics terminal shows the same frame via Sixel or Kitty, with no code change above the seam. There is no browser yet; the input is a static PNG that the engine replaces in Stage 3. This document is the durable record of the completed work.

## Status summary

Stage 0 is code-complete and committed at `1340b1b` (HEAD). The OCR calibration and the Nix `vendorHash` update are committed, not pending.

Acceptance criteria are met. Each backend renders the test images on a terminal that supports it. The block backend renders on a bare TTY (the KMSCON acceptance bar). Switching backend changes only the chosen `Renderer`. The xterm-256 output matches the truecolour output by eye.

Terminals were verified by the owner during Stage 0 review (not by an automated gate, and not recorded in commit messages):

- Block renders on any TTY, the bare-console target.
- Sixel verified in `foot`.
- Kitty verified in `kitty` and WezTerm.

What remains for later stages: capability detection matures in Stage 1 (a basic detector landed early, see `d4d44df`), the crisp text overlay arrives in Stage 2, the chromedp screencast replaces the PNG in Stage 3, and Bubble Tea chrome wraps the viewer in Stage 4.

This paragraph records the Stage 0 boundary. Stages 1 through 3 are now built; current
open work is indexed in [`TODO.md`](./TODO.md).

## Objective and scope

The objective: prove the three backends are swappable behind one seam using a static image, no browser. One screen settles it.

In scope, all delivered:

- Load a PNG into an `*image.RGBA`.
- Render three ways: quadrant or half-block cells, Sixel, Kitty.
- Cycle backends at runtime by hotkey.
- Fit the render to the terminal and re-fit on resize with `golang.org/x/image/draw`.
- Closed-form RGB to xterm-256 plus truecolour SGR passthrough.
- The diagnostics corpus sweep: capture, metrics, and the artefacts, at a fixed grid. (Then run via `nyala --diagnostics`; the sweep has since moved to a build-tagged test, run via `just diagnostics`.)

Out of scope, deferred to later stages:

- chromedp, screencast, and the frame ack loop (Stage 3).
- Automatic capability detection (Stage 1); Stage 0 selects by flag and hotkey. A basic detector was added later, see `d4d44df`.
- The text overlay (Stage 2); Stage 0 renders the bitmap only.
- Full Bubble Tea chrome (Stage 4); a light-touch `WithoutRenderer` coexistence check was allowed earlier (since removed: the spike `internal/viewer/bubbletea_check.go` and the `charm.land/bubbletea/v2` dependency are gone, and the Stage 4 design rejected `WithoutRenderer`, see `docs/OVERVIEW.md:481`).

Later-stage roadmap:

1. Stage 1: capability detection.
2. Stage 2: the crisp text overlay.
3. Stage 3: a chromedp screencast replaces the static PNG.
4. Stage 4: Bubble Tea chrome wraps the viewer.

The target architecture, the tech-stack choices, and the research behind these later stages are in [`OVERVIEW.md`](./OVERVIEW.md).

## Architecture and the contract

One `Frame` and one `Renderer` define the seam; everything downstream swaps behind it. Both live in `internal/render/render.go`.

```go
type GlyphRun struct {
    Col, Row int
    Text     string
    FG       color.RGBA
}

type Frame struct {
    Img  *image.RGBA // the viewport bitmap
    Text []GlyphRun  // empty in Stage 0
}

type Renderer interface {
    Render(f Frame, w io.Writer) error
    Name() string
}
```

`GlyphRun` and `Frame.Text` are present but unused in Stage 0; they carry the text overlay from Stage 2. The PNG loader is the fake frame source the browser engine replaces in Stage 3.

RGBA byte order is an invariant across the render and metrics pipeline. The decoders and image maths assume RGBA, never BGRA. This is the repo-wide gotcha: CDP decodes to RGBA, but Carbonyl's maths assume BGRA, so the port keeps RGBA and never swaps channels.

The three backends each satisfy the seam:

- `block.Renderer` (`internal/render/block`): the universal floor. Unicode block glyphs, truecolour SGR plus closed-form xterm-256. It owns a backend-neutral `Grid` of `Cell{Glyph, FG, BG}`.
- `sixel.Renderer` (`internal/render/sixel`): wraps `charmbracelet/x/ansi/sixel`, emits a DCS envelope.
- `kitty.Renderer` (`internal/render/kitty`): zlib-compressed RGBA via `charmbracelet/x/ansi/kitty`, base64, an APC envelope. Lossless.

## Rendering backends

The block backend is the universal floor; it runs on any TTY. Sixel and Kitty are the graphics paths for terminals that support them.

Cell geometry is pinned: the Route A cell raster is 8x16 pixels (1:2 aspect). Fit preserves the image aspect at the 1:2 cell aspect and letterboxes with blank cells; it never stretches. The closed-form converter maps RGB to the xterm-256 6x6x6 cube plus a 24-step grey ramp, with pure-grey inputs routed to the grey ramp; it is ported from Carbonyl's `xterm.rs`.

Determinism differs by backend:

- Block is deterministic. The same input yields byte-identical output, so it is the golden source.
- The Sixel encoder is not deterministic. Its lossy quantisation varies run to run, which the harness handles with a measured noise floor (see below).
- Kitty is lossless; a round trip reproduces the input with MSE 0.

## Block schemes and the quadrant mapping

The block backend offers two schemes. Quadrant is the default; half-block is the positional floor. `ToggleScheme` lives on the concrete `block.Renderer`, not the `Renderer` interface, to keep the seam thin.

The quadrant scheme maps one cell to a 2x4 source block, split into four 2x2 sub-cells (`TL`, `TR`, `BL`, `BR`). Each sub-cell box-averages a 1x2 column. The half-block floor maps one cell to a 1x2 column: the top pixel becomes the background, the bottom pixel the foreground, with glyph `U+2584`.

The corrected quadrant mapping (from the inversion fix, `2bc6caf`) is self-consistent. `quadrantGlyphs` emits all 16 quadrant blocks keyed by the mask `TL<<3 | TR<<2 | BL<<1 | BR`. A bit is set when the sub-cell's Rec.601 luma is above the cell midpoint, `min + (max-min)/2`. One rule then drives the colour: partition the sub-cells into bright and dark by the same mask, average each group, make bright the foreground and dark the background. The flat masks `0000` and `1111` emit the full block `U+2588` with `fg == bg`. Because one luma mask drives both the glyph and the fg/bg split, the cell is self-consistent.

Measured improvement: block SSIM rose from 0.51 to 0.59 up to 0.66 to 0.70, `deltaE2000Mean` halved, Sixel unchanged.

## Capture and decode routes

Two capture routes feed the same metric pipeline. Route A is the default and the golden source; Route B is an audit-only reality check, never the gate.

Route A rasterises Nyala's own cell grid (glyph, fg, bg per cell) to a PNG with the pinned font (`internal/capture/routea.go`). It is fully deterministic and CI-friendly. The pinned font is FiraCode Nerd Font Mono (`FiraCodeNerdFontMono-Regular.ttf`, embedded in `internal/render/font/font.go`) with GNU Unifont (`Unifont.otf`) as the fallback, so no cell renders as tofu; coverage is checked with `sfnt.GlyphIndex`. Route B captures a real terminal's pixels; it is non-deterministic and slow, so it serves periodic fidelity checks only.

For Sixel and Kitty the render is an escape payload, so the harness decodes it back to an image in-process (`internal/capture/decode.go`).

| Mode | Route Nyala uses | Why |
|---|---|---|
| block | A: rasterise the cell grid | Nyala owns every cell; deterministic, golden-able |
| sixel | Decode the DCS payload | Reflects Nyala's own palette quantisation |
| kitty | Decode the APC payload | Lossless RGBA, round-trip MSE 0 |
| any (audit) | B: real-terminal capture | Periodic reality check only, not the gate |

The round-trip identity proof is the acceptance proof for capture. Sixel decode reproduces the encoder's palette; Kitty decode reproduces the input with MSE 0. The metric therefore measures render error, not decoder error. A VT oracle (`charmbracelet/x/vt`) validates that emitted byte streams parse.

## Measurement framework

The model is the Jivetalking corpus sweep applied to rendering: render each page through each mode, then judge quality and cost with a JSON report plus images. Ground truth would be the browser screenshot; in Stage 0 it is the input PNG, since there is no browser.

Full-reference quality metrics live in `internal/metrics`. Each is scored twice (see dual scoring below). CIEDE2000 is the block colour headline; MS-SSIM and SSIM are structural co-signals only, because they read low on dither and near-black regions; PSNR and MSE are the cheap floor and the Kitty identity check.

| Metric | Measures | Range | Headline role | Go source |
|---|---|---|---|---|
| MS-SSIM | Multi-scale structural similarity | 0..1, 1 best | co-signal | ported ~100 lines from scikit-image |
| SSIM | Windowed structural similarity | 0..1, 1 best | co-signal | ported from scikit-image, validated |
| PSNR | Log-scaled fidelity floor | dB, higher better | cheap floor, Kitty check | `PSNRInfinite` sentinel for identical |
| MSE | Raw per-pixel error | 0 best | cheap floor | stdlib loop |
| GMSD | Gradient-magnitude deviation | 0 best | text-edge loss | ported, Xue 2014, constant 0.0026 |
| CIEDE2000 | Perceptual colour error | dE, <1 imperceptible | block headline | `go-colorful`, 0..100 scale |
| Histogram distance | Colour-distribution shift | intersection 0..1 | banding, palette collapse | intersection, per-channel EMD, chi-square |

Alignment downscales the reference to the render grid with a fixed CatmullRom filter, pinned corpus-wide. Structure scores on luma, colour on RGB, so a colour-only regression cannot hide behind good structure.

Dual scoring and the ideal-block oracle (`internal/metrics/oracle.go`): each metric is scored twice. The absolute score against the downscaled input is the cross-mode comparator and the "how lossy" number, not a renderer grade. The relative score against an ideal block render isolates the loss the real renderer adds over the block-format ceiling. The oracle reads each cell's 2x4 block, splits the four sub-cells by Otsu luma threshold, averages fg and bg in linear-light RGB, and picks the quadrant glyph from the same mask. It also builds a linear-light cell-mean floor (flat colour, space glyph) and a best-of-small-shift positional floor (shifts the source by `{-1, 0, +1}` cells per axis, keeps the best score). For Kitty the ideal equals the input, so absolute and relative collapse, which is correct. The per-cell split algorithm and the joint-versus-separate glyph choice are recorded in the `oracle.go` doc comment.

Legibility scoring (`internal/ocr`) is the make-or-break measure. The package is pure Go and cgo-free: `ocr.go` and `condition.go` compute CER and WER via Levenshtein over runes and word-level edit distance. Conditioning is fixed for Stage 0 (greyscale BT.601, nearest-neighbour integer upscale 2x to 4x to about 32 px capital height, Otsu binarise, force dark-on-light, 16 px quiet zone, char whitelist), so CER tracks regressions, not parameter drift. The Stage 0 sweep emitted null OCR scores; the live legibility gate is the pure-Go nearest-glyph matcher, which grades the Stage 2 overlay against `ocr.CER`.

No-reference co-signals (`internal/metrics/legibility.go`) back the legibility number: WCAG AA contrast at 4.5:1, with a count of low-contrast cells, plus Laplacian-variance sharpness on the text crop.

Performance metrics (`internal/metrics/perf.go`): bytes per frame via a `CountingWriter`, compose and encode timing, FPS, allocations via `runtime/metrics`, and Linux real CPU time via `syscall.Getrusage`. Per-backend `testing.B` sub-benchmarks run over a golden frame with pinned GOMAXPROCS and GOGC, emitting `ns/op`, `B/op`, `allocs/op`, and `bytes/frame`.

Per-mode matrix: colour-quantisation error is central for block and near-zero for truecolour Sixel and Kitty; structure and legibility apply to all three. CIEDE2000 and the quant-error map are the block headline; Sixel uses a paletted path; Kitty is control or n/a. WCAG contrast comes from the cell grid for block and from pixels for Sixel and Kitty. The per-cell error map sits on the native grid for block, a notional grid for the others.

## Harness and honest thresholds

The harness runs three tracks (`internal/harness`), and every threshold traces to calibration or a measured noise floor, never a guess.

- Golden track: stores the deterministic block cell grid (glyph, fg, bg, text flag) as one `golang.org/x/tools/txtar` archive per page, compared by a short stdlib helper behind `UPDATE_GOLDEN` with write-if-changed. The grid is text, so it diffs cleanly.
- Metrics-sweep track: renders corpus x `{block, sixel, kitty}`, captures, scores, and writes `report.json`, `report.csv`, and a markdown summary. It loads `baseline.json` and `thresholds.json`, computes deltas, marks pass or fail, and exits non-zero on a breach. Quality metrics fail when they drop; cost metrics fail when they rise. The report is `schemaVersion` 1 with flat, stably keyed fields and nulls for not-applicable metrics, so two runs diff field by field.
- A/B track: ran the sweep on base and head with at least 10 repeats, emitted benchmark-format lines, and gated with `benchstat` (`golang.org/x/perf`) using the Mann-Whitney p-value to set thresholds above the noise floor. (Since removed: this planned track was never wired, so the benchstat machinery and the `golang.org/x/perf` dependency are gone.)

The block path is deterministic, so its intrinsic-noise floor is zero; global bands sit above zero and stay a small fraction of each metric range. The Sixel encoder is the exception. Its PSNR noise floor is measured as the standard deviation of PSNR across 64 repeated renders, worst over the corpus, quantised to one decimal place, and recorded as `sixelPsnrNoiseFloor` 0.2 dB. A `Modes` map on `Thresholds` plus a `thresholdFor` method give Sixel PSNR a 1.5 dB band against the global 0.5 dB, covering the two-sample regenerate-then-verify gap while staying above the 0.2 dB floor. Block and Kitty keep the tight band.

## Fixtures and the pinned grid

Five committed PNGs in `testdata/` drive the sweep; each renders through every backend and doubles as a golden fixture. The four `testpage-*` pages are rendered from committed PDFs at 150 DPI (rebuild with `just regen-fixtures`).

| Fixture | Stresses |
|---|---|
| `linuxmatters.png` | Real page: dark UI, brand text, thumbnails, small body text, flat panels |
| `testpage-01-grayscale.png` | Greyscale calibration sheet: grey ramps, gradients, text-size ladder, fine alignment marks, no hue |
| `testpage-02-basic.png` | Basic colour calibration sheet: CMYK ramps, gradients, text-size ladder, alignment wheels and bars |
| `testpage-03-cmyk.png` | Full CMYK calibration sheet: per-channel colour wheels, paint-roller bars, line gratings, gradients, gradient spheres |
| `testpage-04-color.png` | Colour artwork sheet: saturated swirls, radial gradients, multi-hue starbursts, two photos |

The test pages stress greyscale, basic colour, full CMYK, and photographic content across a shared text-size ladder, isolating the background's effect on legibility. The calibration measures the four test pages; the sweep adds `linuxmatters.png`.

The sweep grid is pinned at 120 columns by 40 rows, cell 8x16 px, reference filter CatmullRom, RGBA everywhere.

## Viewer and capability detection

The viewer (`internal/viewer/viewer.go`) is a minimal raw-terminal program that fits the render and re-fits on resize. Hotkeys:

- `Tab` cycles the enabled backend set.
- `s` / `S` toggles the block scheme, quadrant to half-block.
- `c` / `C` toggles colour, truecolour to xterm-256.
- `q` / `Q` / `Ctrl-C` quits.

Capability detection (`internal/viewer/detect.go`, `d4d44df`) runs one startup handshake before the full-screen switch. It writes the Kitty graphics query (`a=q`), three cell-size queries (`CSI 16t`, `14t`, `18t`), and the DA1 fence (`RequestPrimaryDeviceAttributes`), then reads via `unix.Poll` until the DA1 reply (200 ms cap). DA1 is the fence: a terminal answers DA1 after any Kitty or cell-size replies, so DA1 arrival means the batch is buffered; a bare TTY never answers, so detection fails closed to block only. The code uses `unix.Poll`, not `SetReadDeadline`, because stdin returns `ErrNoDeadline`.

Sixel support comes from the DA1 attribute list (the element exactly `"4"`, matched as an element so `64` and `14` do not false-match). Kitty support comes from the `\x1b_G...;OK` APC reply.

Best-fit scaling resolves the cell pixel size in order: `TIOCGWINSZ` ioctl, then `CSI 16t` (direct), then `14t` / `18t`, else defaults. Graphics scale to the real cell pixel size so one source pixel maps to the real cell geometry. `State.SetEnabled` configures the `Tab` cycle, and `CycleBackend` steps only enabled backends.

Under `tmux` or `screen` (`TMUX` or `STY` set) the viewer offers block only, since Stage 0 does not do DCS passthrough. The viewer always runs automatic detection; the Tab cycle steps only the detected-supported backends.

## Work completed after the initial build

The base commit `6f42563` delivered the full Stage 0 spine: the seam, all three backends, capture and decode, the metrics, OCR and legibility, the harness, the viewer, the CLI, tests, and testdata. The commits below refine it through HEAD.

- `4661a82` feat(fonts): replace Cascadia Mono with FiraCode Nerd Font Mono. Swapped the embedded Route A face from Cascadia Mono to FiraCode Nerd Font Mono (`FiraCodeNerdFontMono-Regular.ttf`, 2.6 MB, plus `LICENSE-FiraCode`), keeping GNU Unifont as the fallback for missing code points. Rationale: FiraCode Nerd Font Mono is the owner's preferred terminal face. Expanded the metric calibration and regenerated `baseline.json`, `calibration.json`, `thresholds.json`, and the golden archives; updated `OVERVIEW.md`, `README.md`, `AGENTS.md`.
- `34b6fc6` feat(harness): measure the Sixel PSNR noise floor and add per-mode threshold overrides. The lossy Sixel encoder is non-deterministic, so a fixed global PSNR band fails. Measured the floor as the PSNR standard deviation across 64 repeated renders (`sixelNoiseFloorRuns`), worst over the corpus, quantised to one decimal (`sixelNoiseFloorDP`), recorded as `sixelPsnrNoiseFloor` 0.2. Added a `Modes` map and a `thresholdFor` method; `gateOne` uses it. Sixel PSNR gets a 1.5 dB band; block and Kitty stay tight. Added `measureSixelPSNRNoiseFloor`, `stdDev`, `sixelRoundTripPSNR`, and the `SixelPSNRNoiseFloor` field. Regenerated `baseline.json`, `calibration.json`, `thresholds.json`.
- `ee241c5` fix(render): CRLF for cursor positioning with auto-wrap disabled (interlacing fix). Root cause: with auto-wrap off in raw mode, a bare `LF` advanced the row but left the cursor in the same column, so rows interlaced. Fix: each row now ends with an SGR reset plus `\r\n`; `enterFullScreen` and `exitFullScreen` disable auto-wrap and hide the cursor on entry, restoring both on exit. Tests verify CRLF per row; calibration byte counts updated.
- `670f766` feat(kitty): large image transmission via chunked payload (black-screen fix). Root cause: the payload was transmitted but never displayed, and oversized payloads exceeded the single-APC chunk limit. Fix: use action `TransmitAndPut` (`a=T`) so the terminal loads and shows the image; set `Quite 2` (`q=2`) to suppress OK and error replies; `writeChunks` splits payloads over `kitty.MaxChunkSize` across APC frames (first carries the options plus `m=1`, middle frames `m=1`, final `m=0`); the decoder `reassembleKittyFrames` concatenates them back into one image; the viewer writes `kittyDeleteAll` (`\x1b_Ga=d,d=A\x1b\`) before each Kitty redraw, because `clearScreen` erases text but not Kitty images. Tests cover small, large, and round-trip; calibration adjusted.
- `2bc6caf` fix(render): correct quadrant block binarisation and fg/bg assignment (inversion fix). Root cause: a per-mask switch assigned some bright and dark colours backwards. Fix: rewrote `quadrantGlyphs` to emit all 16 quadrant blocks keyed by the luma mask, and replaced the colour switch with one rule that partitions sub-cells into bright and dark by the same mask. Added `avgRGBA`. Tests `TestComposeQuadrantNotInverted` and `TestComposeQuadrantTwoBright`. Result: block SSIM rose to 0.66 to 0.70, `deltaE2000Mean` halved, Sixel unchanged. Regenerated all four golden archives, `baseline.json`, `calibration.json`; updated the `oracle.go` comment.
- `d4d44df` feat(viewer): detect terminal graphics capabilities and add best-fit scaling. Added the DA1-fenced handshake that probes Kitty and cell sizes, parses Sixel and Kitty support, resolves the real cell pixel size, and scales graphics to it. Falls back to block under `tmux` or `screen`; the then-current `--all-renderers` flag forced all backends (since removed; the viewer now always auto-detects). Rationale: an early, conservative detector improves the default experience before the full Stage 1 work.
- `1340b1b` chore(harness): calibrate OCR metrics and update `vendorHash` (HEAD). Set `ocrBuilt` true, `cerBound` 0.15, the ladder across 8 to 28 px, and `minLegiblePx` 10 (8 px illegible at CER 0.23 / WER 0.67; 10 px and up legible; 14 px a minor CER 0.14). Bumped `flake.nix` `vendorHash` to `sha256-OMak9QNVkpoXNaKV6UAb/MBSaP8aayx2xNoxatcPl8M=`.

## Decisions recorded with rationale

Cross-cutting decisions, with the reasoning kept short:

- testdata layout: fixtures live in `testdata/`, goldens in `testdata/golden/*.txtar`, with `baseline.json` and `thresholds.json` committed. Small JSON and text goldens diff cleanly and stay stable across font and Chromium versions; pixel goldens bloat git and break across versions.
- Single binary for the viewer and the sweep: in Stage 0 one `nyala` binary ran both, gated by `--diagnostics`, so there was no parallel CLI and one toolchain shipped. (The sweep has since moved to a build-tagged harness test, run via `just diagnostics`; the binary is now a viewer only.)
- charmbracelet v2 stack plus the Bubble Tea coexistence check: `ansi/sixel`, `ansi/kitty`, and `x/vt` supply the backends and the oracle; a light-touch Bubble Tea v2 `WithoutRenderer` spike confirmed the bitmap survived the cell renderer, de-risking Stage 4 early. (Since removed: the spike `internal/viewer/bubbletea_check.go` and the `charm.land/bubbletea/v2` dependency are gone, and the Stage 4 design rejected `WithoutRenderer`, see `docs/OVERVIEW.md:481`.)
- Port SSIM and MS-SSIM from the scikit-image reference rather than vendoring fennec, because fennec has an 8x8 window and an off-centre kernel bug; the port is validated against scikit-image, ImageMagick, and `kornelski/dssim`.
- go-colorful dE scaling: `Lab` L and `DistanceCIEDE2000` are 0..1, so the code multiplies by 100, with a Sharma-table golden (skipping the two `L=50, a=2.5` rows go-colorful misses).
- Legibility deferred to the Stage 2 matcher: the bespoke nearest-glyph matcher's exact-match strength needs crisp glyph cells that Stage 0 block text does not provide, so it grades the Stage 2 overlay instead. The pure-Go matcher and its `ocr.CER` gate are the live ruler; the gosseract OCR path that once measured legibility on the rendered image has since been removed.
- Dynamic viewer sizing versus the fixed sweep grid: the interactive viewer best-fits the terminal, while the sweep pins 120x40 so golden frames and numbers stay reproducible.

## Acceptance criteria and how each was met

- Each backend renders both test images correctly on a supporting terminal: met. The owner verified Sixel in `foot` and Kitty in `kitty` and WezTerm during review.
- The block backend renders correctly on a bare TTY (the KMSCON bar, no Sixel or Kitty): met. Block is the universal floor and renders on any TTY; the owner confirmed it.
- Switching backend changes only the chosen `Renderer`, nothing above the seam: met. The viewer swaps the `Renderer` value behind the interface; a test renders the same `Frame` across backends.
- The xterm-256 output is a close match to the truecolour output by eye: met. The `c` / `C` hotkey toggles the two so the converter can be eyeballed against the truth.

## Risks identified and how they were resolved or carried forward

- RGBA versus BGRA byte order: resolved. RGBA holds across the render and metrics pipeline; channel order is asserted on known pixels and never swapped. This stays the repo-wide invariant.
- cgo under Nix: resolved. The legibility package is pure Go, so the shipped `nyala` binary stays static and cgo-free. The gosseract OCR path that needed `tesseract` and `leptonica` has since been removed.
- Oracle algorithm choice: resolved. The oracle splits sub-cells by Otsu luma and averages fg and bg in linear-light RGB; the choice and rationale are recorded in the `oracle.go` doc comment.
- SSIM validation: resolved. The port is validated against scikit-image, ImageMagick, and `kornelski/dssim` to a fixed tolerance, gating every later structural threshold.
- Half-cell positional floor: resolved. A best-of-small-shift variant (plus or minus one cell per axis) measures the floor directly instead of guessing a margin, and the half-block scheme stays available as the positional floor.
