# Nyala

A browser for the terminal: a TTY web browser driving headless Chromium via the Chrome DevTools Protocol. Stages 0 through 3 are built: an image-based rendering proof of concept with three swappable backends and a measurement harness (Stage 0), capability and colour detection (Stage 1), the crisp text overlay on the block path (Stage 2), and the live browser integration (Stage 3). `nyala <url>` now launches headless Chromium, streams the screencast, and renders the live page with the crisp text overlay. See `docs/OVERVIEW.md` for the target architecture and `docs/STAGE-0.md`, `docs/STAGE-1.md`, `docs/STAGE-2.md`, and `docs/STAGE-3.md` for each completed stage's contract, decisions, and measurement. The Stage 3 post-build spikes (sustained FPS, sync tolerance) are pending validation.

## Project structure

- `cmd/nyala` - entrypoint; kong CLI wiring. `app.CLI` holds the flags.
- `internal/render` - the `Frame`/`Renderer` seam, with backends in `block`, `sixel`, `kitty`, and glyph loading in `font`. `render.Frame` carries `FrameStats` for engine-phase capture timing; it is nil off the capture path, so production stays zero-cost.
- `internal/engine` - the `Engine` seam and `ChromiumEngine`: launches headless Chromium, streams the screencast, extracts the DOM text layer over CDP, and emits synced `render.Frame` values. On the block backend it forces the page onto the cell grid before capture: a document-creation script (`stylesheet.go`) injects the embedded FiraCode Nerd Font Mono as a base64 `@font-face` and forces every element to the monospace family, a seed `font-size` of `cellW/advanceRatio`, `line-height` `cellH`, no kerning, no ligatures, `text-align: left`, and de-sticks fixed and sticky positioning; a one-shot runtime calibration (`calibrate.go`) then measures an offscreen probe after `document.fonts.ready` and corrects the size once, so ASCII glyphs land at `cellW` px. On the block backend the page lays out at `min(cols*cellW, --layout-width)` CSS pixels via a continuous device scale `Z = max(1, cols*cellW / --layout-width)`, so a wide terminal reflows to the target width. The forcing, calibration, and box-to-cell mapping run on the fractional CSS cell `cellW/Z`. The graphics backends (Sixel, Kitty) stay at device scale 1 and draw the bitmap alone, so they take no forcing and no calibration and render the page with its own fonts and layout. `syncFontForcing` (`forcing.go`) reconciles the forcing to the active backend on the snapshot goroutine: a Tab from block to a graphics backend removes the registered script and the live style so the page reflows to its natural layout, and a Tab back re-registers, re-injects, and recalibrates. Capture and corpus force the block backend, so their forcing is unchanged. The snapshot goroutine clocks off the `firstContentfulPaint` lifecycle event, not per frame; the decoded text layer is sticky and re-emitted on update, so a static page shows text, and re-snapshots fire on navigation, resize, a debounced scroll-offset change, and a debounced `networkAlmostIdle` reflow. The decode requests computed `color`, `visibility`, and `opacity` plus the blended text opacities, and drops hidden boxes before they compose. A box with `visibility:hidden` or `collapse`, an effective opacity (own times ancestor-blended) below 0.05, or a height below a quarter cell emits no run. Absent style data reads as visible. The box-to-cell mapping (`mapBoxToCells` in `snapshot.go`) round-snaps both axes, trims all trailing whitespace the soft-wrap layout adds (a space or a newline), and clips at the grid's right edge. Live and capture share this path, so text lands the same way in both. It also reads the conventional proxy environment variables and drives Chromium's proxy (see the runtime note below). `capture_record.go` records whitelisted CDP events (no image bytes, proxy credentials stripped from `cdp-session.json`) for the capture bundle.
- `internal/capture` - rasterises a frame and decodes terminal output back to pixels, and writes the bounded-capture bundle.
- `internal/metrics` - image and performance measures over a frame.
- `internal/ocr` - the pure-Go nearest-glyph matcher and its CER/WER scoring; the Stage 2 diagnostics gate calls `ocr.NewMatcher` and `ocr.CER`.
- `internal/harness` - the diagnostics sweep, golden archives, and thresholds.
- `internal/viewer` - drives the terminal: the `stream.go` push consumer that draws engine frames, the `Capture` driver (sibling to `Stream`, no key reader, mirrors terminal bytes to `terminal-stream.raw` via `io.MultiWriter`), plus capability detection and backend cycling.
- `docs/OVERVIEW.md` - architecture and design decisions.
- `docs/STAGE-0.md`, `docs/STAGE-1.md`, `docs/STAGE-2.md`, `docs/STAGE-3.md` - durable records of the completed Stage 0, Stage 1, Stage 2, and Stage 3 work.

Module path is `github.com/wimpysworld/nyala`. Go 1.26.

## Setup

Tools are pinned by the Nix flake. Enter the dev shell with `nix develop`, or let direnv load it via `.envrc` (`use flake`). `just` is the task runner; prefer its recipes over ad-hoc `go` commands. Run `just` to list them.

## Build and test

### Launching nyala

`just run` and `just corpus` are the only supported way to run nyala. Use them for every run, above all when you capture metrics or data. Never hand-roll: do not run `./nyala` directly, and do not hand-roll `script`, `stty`, sandbox flags, or `NYALA_CAPTURE`.

The recipe owns the launch, and a bare run misses every step below:

- It allocates a pty with `script` when no TTY is present, so the viewer and capture work headless. Without it a headless run has no terminal.
- It fixes a deterministic grid with `stty rows`/`cols` from `LINES`/`COLUMNS` (default 40x120). Without it the grid varies, so capture data is not comparable across runs.
- It sets Chromium's no-sandbox flag only from `FENCE_SANDBOX`; `fence` exposes that variable itself, so never set it by hand.
- It gates the capture lifecycle through `NYALA_CAPTURE` (see below). Hand-rolling the variable skips the recipe's wrapping and yields a broken bundle.

Behind a synthetic pty only the block backend works, because graphics probes get no reply, so a hand-rolled run silently falls back to block. A hand-rolled run yields broken or non-comparable capture data.

Recipes:

- `just run https://wimpys.world` - build `./nyala`, then run it against a URL.
- `just capture=1 run https://wimpys.world` - run the same harness with `NYALA_CAPTURE=1`, which writes a one-frame bundle whose frame is the painted, text-bearing page (see `NYALA_CAPTURE` below for the bounded-capture lifecycle and `n>1`). The recipe also accepts `capture=` and `backend=` after the URL; `backend=block` forwards `--backend=block` to force the renderer.
- `just corpus` - capture every `testdata/corpus` fixture on the block backend in one sweep (each via `just capture=1 run ... backend=block`), each as its own bundle, for comparing each fixture's `screenshot.png` against its `block-render.png`. It sweeps each of the ten fixtures at two grids, 120x40 and 240x48 (the wide leg exports `COLUMNS=240 LINES=48`), and names the leg in any failure. After each capture it gates the fixture with `placement-score -gate -thresholds <fixture.json>` against the bundle frame, reading the recorded `cell_px`: mean right-edge error under 0.5 cells, max under 1.0, drifted-box count 0, same-row collisions 0, off-grid boxes 0. Six fixtures carry committed `placement_gate` overrides in `fixture.json`. `layout-02` and `layout-03` cover their shaped-script runs plus the wide-leg collisions, `layout-04` a deferred wrong-column collision fault, `layout-07` and `layout-08` trailing-whitespace max and drifted artefacts, and `layout-10` a tightened mean of 0.25. It continues past a failed capture or a gate breach and exits nonzero if any fixture fails.

### Recipes

- `just build` - build the `nyala` binary.
- `just rasterise <terminal-model.json | frame-dir>` - rasterise a captured block grid to `block-render.png` beside it, for comparing against the frame's source `screenshot.png`. A block capture writes this PNG automatically; this recipe re-renders an existing bundle on demand.
- `just placement-score <terminal-model.json | frame-dir>` - score text placement from a captured `text-map.json`: horizontal right-edge error, vertical row error, same-row cell collisions, and off-grid box count.
- `just test` - run the fast unit tests, the routine gate. It runs no corpus sweep, no calibration, and no rasteriser-driven full-corpus measurement, so the harness package finishes in seconds.
- `just diagnostics` - run the heavy quality gate (`go test -tags diagnostics ./internal/harness/`) over the five committed fixtures (`linuxmatters.png` plus the four `testpage-*` pages) at a fixed 120x40 grid; the calibration runs over the four test pages only. It runs one corpus sweep and one calibration, shared across the tagged tests, writes its report JSON/CSV, summary, and artefact PNGs to `testdata/diagnostics/` (gitignored), gates against the committed baseline and thresholds, and runs the calibration, golden, and overlay-grade tracks. The overlay grade hard-gates the overlaid-grid golden and the matcher `CER == 0`, gates the WCAG low-contrast text-cell count against `baseline.json`. It takes minutes, so run it deliberately, not on every change.
- `just live` - run the live-browser integration test (`go test -tags live ./internal/engine/`). It launches a real Chromium, so it stays off `just test` and off CI; run it deliberately in the dev shell.
- `just update-golden` - rerun `TestGoldenTrack` and `TestOverlayGoldenTrack` with `UPDATE_GOLDEN=1` to regenerate the committed bitmap and overlay golden archives.
- `just overlay` - regenerate the overlay artefact PNGs into `testdata/diagnostics/` (gitignored) for eyeballing, mirroring `just update-golden`.
- `just genruns` - regenerate the committed `testdata/overlay/*.runs` fixtures.
- `just kmscon-testcard` - print the KMSCON quadrant test card to the TTY for the owner to eyeball inter-row bleed; manual and owner-verified, off CI.
- `just lint` - go vet, gocyclo, ineffassign, golangci-lint, actionlint.
- `just fmt` - format with gofmt; `just fmt-check` verifies.
- `just tidy` - `go mod tidy`; `just tidy-check` verifies go.mod/go.sum are clean.
- `just coverage` - tests with a coverage report.
- `just vuln` - govulncheck.
- `just release-check`, `just release-snapshot` - GoReleaser config check and local snapshot.
- `just check` - full local gate (tidy-check, fmt-check, lint, test, build, release-check, `nix flake check`, vuln).

The `nyala` binary opens the positional `<url>`. A bare host gains an `https://` scheme; `http`, `https`, and `file` are accepted, others are rejected. Its flags are `--browser` (path to the Chromium executable to drive), `--color` (auto, truecolor, 256, none; default auto), `--backend` (auto, block, kitty, sixel; default auto), `--layout-width` (target CSS layout width for the block backend, default `1280`, a value of zero or below is rejected), and `--version`. `--backend=auto` keeps the detection behaviour below; a named backend forces one renderer and fails fast if the terminal cannot run it (validated after detection, before navigation). The viewer auto-detects terminal capability (Kitty, then Sixel, then Block) and Tab cycles the supported backends. The diagnostics sweep runs via `just diagnostics`; the golden archives regenerate via `just update-golden`.

`NYALA_CAPTURE=n` is the bounded-capture switch. Unset or `0` is off. `1` writes a one-frame bundle. A positive `n` captures and draws `n` usable frames (a delivered `render.Frame` with a non-nil bitmap), writes one `nyala-capture-*` temp directory bundle, prints its path, then exits; Tab cycling is off in capture mode. A usable frame is gated to the painted, text-bearing page: the driver waits for the first contentful paint, then a `textReady` latch holds the first counted frame until the engine publishes its first DOM snapshot and re-emits the held bitmap with the runs folded on. The publish trails paint by the calibration pass, so the latch is what keeps the first counted frame from landing text-free. The latch is capture-only; production pays only the nil checks. So `capture=1` is the deterministic path to the painted, text-bearing frame, and `n>1` is no longer needed to skip a blank pre-paint frame. A full capture exits 0; a partial bundle exits nonzero (first-draw timeout, per-frame idle timeout, forced-backend rejection, or frame channel close). `performance.json` carries `layout_scale` when the block layout scale exceeds 1 (absent means 1), and `placement-score` divides `cell_px` by it. The bundle is a set of plain files: root `manifest.json`, `report.md`, `performance.json`, `terminal-stream.raw`, `cdp-session.json`, and `cdp-events.ndjson`. Each frame directory (`frames/frame-NNNNNN/`) holds `screenshot.png`, `text.txt`, `cdp-payload.json`, `text-map.json`, `terminal-model.json`, and `block-render.png` (the last two exist on the block backend only; `block-render.png` is the model rasterised to a PNG beside it; Kitty and Sixel omit both and the manifest notes a graphics backend has no cell grid). Each `text-map.json` run carries `Col,Row,Text,FG`, the `X,Y,W,H` box bounds in CSS pixels, and `CellW`, the CSS-pixel cell width the run was snapped with. The bundle can hold sensitive page content, so share it only after review.

Chromium is a runtime dependency. The Nix dev shell adds it Linux-gated, and the wrapped binary sets `CHROME_PATH` to it. The binary discovers the executable via `--browser`, then `NYALA_BROWSER`, then `CHROME_PATH`, then a `PATH` lookup.

Proxy config is environment-only; there is no proxy flag. At construction the engine reads the proxy environment variables once and applies them to the Chromium launch. It sets `--proxy-server` (credential-free) from the first set of `HTTPS_PROXY`, `https_proxy`, `HTTP_PROXY`, `http_proxy`, `ALL_PROXY`, `all_proxy`, and sets `--proxy-bypass-list` from `NO_PROXY` then `no_proxy`. When the proxy URL carries credentials, the engine answers the proxy auth challenge over CDP (the Fetch domain), keeping credentials out of Chromium's process arguments. A malformed proxy URL fails loudly at launch, naming the source variable.

When you change code, `just build`, `just test`, and `just lint` must pass; this is the routine pre-commit gate. `just diagnostics` is the heavier quality gate, run deliberately rather than on every change. `just check` runs the full local gate before considering any change done.

## Code style

Format with gofumpt (`just fmt` uses gofmt; both pass `fmt-check`). Lint config lives in `.golangci.yaml`; revive runs with the `exported` rule, so add a doc comment to every exported symbol you add.

## Gotchas

- RGBA byte order everywhere in the render and metrics pipeline. The decoders and image maths assume RGBA, never BGRA. Do not swap channels.
- `internal/ocr` is pure Go and cgo-free. It holds the nearest-glyph matcher and its CER/WER scoring; the Stage 2 diagnostics gate calls `ocr.NewMatcher` and `ocr.CER`. Test it with `go test ./internal/ocr/`.
- The heavy quality tests (corpus sweep, calibration, golden track) carry the `//go:build diagnostics` tag, so they leave the default `go test ./...` run. Run them via `just diagnostics`; regenerate the golden archives via `just update-golden`. The shared sweep and calibration run once each per tagged run, so adding a tagged test that reuses them adds no extra rasteriser pass.
- The crisp text overlay lives in the block `Grid`: `block.Cell` carries `Text` and `Wide`, and `block.Compose` folds `Frame.Text` on. All three consumers prefer `Cell.Text` over `Cell.Glyph`, so the overlay travels through one grid. They differ on the `Wide` trailing cell. The two terminal consumers (`block.Renderer.Render` and the viewer `writeGrid`) emit nothing for it, because the lead cell's width-2 grapheme already fills both terminal columns; a space would add a third column and misalign the row. The Route A `capture.Rasteriser` fills the trailing cell's background only, with no glyph and no space, because each raster cell is a fixed pixel tile. The overlay is first-writer-wins: a claimed cell (overlay `Text` or a `Wide` trailing cell) is never overwritten, so an earlier run keeps the cell (paint-order z-ordering stays deferred). An overlaid or boundary cell recomposes its background from the source columns the run's box does not cover, down to a half-cell floor (the quadrant grid is two source columns per cell). The overlay goldens (`testdata/golden/overlay/*.txtar`) regenerate on amd64 only, like the bitmap goldens, since the whole-cell bg-average rounds differently on arm64; `TestOverlayGoldenTrack` skips off amd64.
- The image maths is 64-bit floating point and rounds differently across architectures (arm64 contracts to FMA, x86-64 does not). Two consequences for tests, which CI runs on `linux/amd64` and `linux/arm64`. First, never assert exact equality on metric output: identical inputs leave a sub-1e-12 residue on arm64, so compare against an epsilon (see `internal/metrics/colour_test.go`). Second, the committed golden archives are an amd64 byte snapshot; the downscale and block compose diverge by a few bytes on arm64, so `TestGoldenTrack` skips when `runtime.GOARCH != "amd64"`. Regenerate the goldens on amd64 only.

## Commit conventions

Use Conventional Commits (`feat:`, `fix:`, `chore:`, `docs:`).
