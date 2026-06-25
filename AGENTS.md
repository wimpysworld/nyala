# Nyala

A browser for the terminal: a TTY web browser driving headless Chromium via the Chrome DevTools Protocol. Stages 0 through 3 are built: an image-based rendering proof of concept with three swappable backends and a measurement harness (Stage 0), capability and colour detection (Stage 1), the crisp text overlay on the block path (Stage 2), and the live browser integration (Stage 3). `nyala <url>` now launches headless Chromium, streams the screencast, and renders the live page with the crisp text overlay. See `docs/OVERVIEW.md` for the target architecture and `docs/STAGE-0.md`, `docs/STAGE-1.md`, `docs/STAGE-2.md`, and `docs/STAGE-3.md` for each completed stage's contract, decisions, and measurement. The Stage 3 post-build spikes (sustained FPS, sync tolerance) are pending validation.

## Project structure

- `cmd/nyala` - entrypoint; kong CLI wiring. `app.CLI` holds the flags.
- `internal/render` - the `Frame`/`Renderer` seam, with backends in `block`, `sixel`, `kitty`, and glyph loading in `font`. `render.Frame` carries `FrameStats` for engine-phase capture timing; it is nil off the capture path, so production stays zero-cost.
- `internal/engine` - the `Engine` seam and `ChromiumEngine`: launches headless Chromium, streams the screencast, extracts the DOM text layer over CDP, and emits synced `render.Frame` values. It also reads the conventional proxy environment variables and drives Chromium's proxy (see the runtime note below). `capture_record.go` records whitelisted CDP events (no image bytes, proxy credentials stripped from `cdp-session.json`) for the capture bundle.
- `internal/capture` - rasterises a frame and decodes terminal output back to pixels, and writes the bounded-capture bundle.
- `internal/metrics` - image and performance measures over a frame.
- `internal/ocr` - text legibility scoring (see the OCR gotcha below).
- `internal/harness` - the diagnostics sweep, golden archives, and thresholds.
- `internal/viewer` - drives the terminal: the `stream.go` push consumer that draws engine frames, the `Capture` driver (sibling to `Stream`, no key reader, mirrors terminal bytes to `terminal-stream.raw` via `io.MultiWriter`), plus capability detection and backend cycling.
- `docs/OVERVIEW.md` - architecture and design decisions.
- `docs/STAGE-0.md`, `docs/STAGE-1.md`, `docs/STAGE-2.md`, `docs/STAGE-3.md` - durable records of the completed Stage 0, Stage 1, Stage 2, and Stage 3 work.

Module path is `github.com/wimpysworld/nyala`. Go 1.26.

## Setup

Tools are pinned by the Nix flake. Enter the dev shell with `nix develop`, or let direnv load it via `.envrc` (`use flake`). `just` is the task runner; prefer its recipes over ad-hoc `go` commands. Run `just` to list them.

## Build and test

- `just build` - build the `nyala` binary.
- `just run https://wimpys.world` - build `./nyala`, then run it against a URL. This is the harness for launching the browser. Always use it; never hand-roll `./nyala`, `script`, or sandbox flags. It owns the launch. It sets Chromium's no-sandbox flag when `fence` exposes `FENCE_SANDBOX`. When no TTY is present it wraps the run in `script` to allocate a pty, so the viewer and capture work headless. Behind a synthetic pty only the block backend works, because graphics probes get no reply. `fence` exposes `FENCE_SANDBOX` itself; do not set it by hand.
- `just capture=1 run https://wimpys.world` - run the same harness with `NYALA_CAPTURE=1`, which writes a one-frame bundle whose frame is the painted, text-bearing page (see `NYALA_CAPTURE` below for the bounded-capture lifecycle and `n>1`). The recipe also accepts `capture=` and `backend=` after the URL; `backend=block` forwards `--backend=block` to force the renderer.
- `just rasterise <terminal-model.json | frame-dir>` - rasterise a captured block grid to `block-render.png` beside it, for comparing against the frame's source `screenshot.png`. A block capture writes this PNG automatically; this recipe re-renders an existing bundle on demand.
- `just test` - run the fast unit tests, the routine gate. It runs no corpus sweep, no calibration, and no rasteriser-driven full-corpus measurement, so the harness package finishes in seconds.
- `just diagnostics` - run the heavy quality gate (`go test -tags diagnostics ./internal/harness/`) over the five committed fixtures (`linuxmatters.png` plus the four `testpage-*` pages) at a fixed 120x40 grid; the calibration runs over the four test pages only. It runs one corpus sweep and one calibration, shared across the tagged tests, writes its report JSON/CSV, summary, and artefact PNGs to `testdata/diagnostics/` (gitignored), gates against the committed baseline and thresholds, and runs the calibration, golden, and overlay-grade tracks. The overlay grade hard-gates the overlaid-grid golden and the matcher `CER == 0`, gates the WCAG low-contrast text-cell count against `baseline.json`, and reports the gosseract differentiator. It takes minutes, so run it deliberately, not on every change.
- `just live` - run the live-browser integration test (`go test -tags live ./internal/engine/`). It launches a real Chromium, so it stays off `just test` and off CI; run it deliberately in the dev shell.
- `just update-golden` - rerun `TestGoldenTrack` and `TestOverlayGoldenTrack` with `UPDATE_GOLDEN=1` to regenerate the committed bitmap and overlay golden archives.
- `just overlay` - regenerate the overlay artefact PNGs into `testdata/diagnostics/` (gitignored) for eyeballing, mirroring `just update-golden`.
- `just kmscon-testcard` - print the KMSCON quadrant test card to the TTY for the owner to eyeball inter-row bleed; manual and owner-verified, off CI.
- `just lint` - go vet, gocyclo, ineffassign, golangci-lint, actionlint.
- `just fmt` - format with gofmt; `just fmt-check` verifies.
- `just tidy` - `go mod tidy`; `just tidy-check` verifies go.mod/go.sum are clean.
- `just coverage` - tests with a coverage report.
- `just vuln` - govulncheck.
- `just release-check`, `just release-snapshot` - GoReleaser config check and local snapshot.
- `just check` - full local gate (tidy-check, fmt-check, lint, test, build, release-check, `nix flake check`, vuln).

The `nyala` binary opens the positional `<url>`. A bare host gains an `https://` scheme; `http`, `https`, and `file` are accepted, others are rejected. Its flags are `--browser` (path to the Chromium executable to drive), `--color` (auto, truecolor, 256, none; default auto), `--backend` (auto, block, kitty, sixel; default auto), and `--version`. `--backend=auto` keeps the detection behaviour below; a named backend forces one renderer and fails fast if the terminal cannot run it (validated after detection, before navigation). The viewer auto-detects terminal capability (Kitty, then Sixel, then Block) and Tab cycles the supported backends. The diagnostics sweep runs via `just diagnostics`; the golden archives regenerate via `just update-golden`.

`NYALA_CAPTURE=n` is the bounded-capture switch. Unset or `0` is off. `1` writes a one-frame bundle. A positive `n` captures and draws `n` usable frames (a delivered `render.Frame` with a non-nil bitmap), writes one `nyala-capture-*` temp directory bundle, prints its path, then exits; Tab cycling is off in capture mode. A usable frame is gated to the painted page: the driver waits for the first contentful paint, then folds the text layer on by waiting for the first DOM snapshot to publish (bounded by a short grace, so a text-less page proceeds rather than hangs), before it counts the first frame. So `capture=1` is the deterministic path to the painted, text-bearing frame, and `n>1` is no longer needed to skip a blank pre-paint frame. A full capture exits 0; a partial bundle exits non-zero (first-draw timeout, per-frame idle timeout, forced-backend rejection, or frame channel close). The bundle is plain files: root `manifest.json`, `report.md`, `performance.json`, `terminal-stream.raw`, `cdp-session.json`, `cdp-events.ndjson`, and per-frame `frames/frame-NNNNNN/` with `screenshot.png`, `text.txt`, `cdp-payload.json`, `text-map.json`, `terminal-model.json`, and `block-render.png` (both block backend only; `block-render.png` is the model rasterised to a PNG beside it; Kitty and Sixel omit both and the manifest notes a graphics backend has no cell grid). The bundle can hold sensitive page content, so share it only after review.

Chromium is a runtime dependency. The Nix dev shell adds it Linux-gated, and the wrapped binary sets `CHROME_PATH` to it. The binary discovers the executable via `--browser`, then `NYALA_BROWSER`, then `CHROME_PATH`, then a `PATH` lookup.

Proxy config is environment-only; there is no proxy flag. At construction the engine reads the proxy environment variables once and applies them to the Chromium launch. It sets `--proxy-server` (credential-free) from the first set of `HTTPS_PROXY`, `https_proxy`, `HTTP_PROXY`, `http_proxy`, `ALL_PROXY`, `all_proxy`, and sets `--proxy-bypass-list` from `NO_PROXY` then `no_proxy`. When the proxy URL carries credentials, the engine answers the proxy auth challenge over CDP (the Fetch domain), keeping credentials out of Chromium's process arguments. A malformed proxy URL fails loudly at launch, naming the source variable.

When you change code, `just build`, `just test`, and `just lint` must pass; this is the routine pre-commit gate. `just diagnostics` is the heavier quality gate, run deliberately rather than on every change. `just check` runs the full local gate before considering any change done.

## Code style

Format with gofumpt (`just fmt` uses gofmt; both pass `fmt-check`). Lint config lives in `.golangci.yaml`; revive runs with the `exported` rule, so add a doc comment to every exported symbol you add.

## Gotchas

- RGBA byte order everywhere in the render and metrics pipeline. The decoders and image maths assume RGBA, never BGRA. Do not swap channels.
- OCR uses cgo `gosseract`, guarded behind the `//go:build ocr` tag, so the default build and the shipped binary stay cgo-free. The OCR tests need libtesseract and leptonica from the Nix dev shell: run `nix develop`, then `go test -tags ocr ./internal/ocr/...`.
- The heavy quality tests (corpus sweep, calibration, golden track) carry the `//go:build diagnostics` tag, so they leave the default `go test ./...` run. Run them via `just diagnostics`; regenerate the golden archives via `just update-golden`. The shared sweep and calibration run once each per tagged run, so adding a tagged test that reuses them adds no extra rasteriser pass.
- The crisp text overlay lives in the block `Grid`: `block.Cell` carries `Text` and `Wide`, and `block.Compose` folds `Frame.Text` on. All three consumers prefer `Cell.Text` over `Cell.Glyph`, so the overlay travels through one grid. They differ on the `Wide` trailing cell. The two terminal consumers (`block.Renderer.Render` and the viewer `writeGrid`) emit nothing for it, because the lead cell's width-2 grapheme already fills both terminal columns; a space would add a third column and misalign the row. The Route A `capture.Rasteriser` fills the trailing cell's background only, with no glyph and no space, because each raster cell is a fixed pixel tile. The overlay goldens (`testdata/golden/overlay/*.txtar`) regenerate on amd64 only, like the bitmap goldens, since the whole-cell bg-average rounds differently on arm64; `TestOverlayGoldenTrack` skips off amd64.
- The image maths is 64-bit floating point and rounds differently across architectures (arm64 contracts to FMA, x86-64 does not). Two consequences for tests, which CI runs on `linux/amd64` and `linux/arm64`. First, never assert exact equality on metric output: identical inputs leave a sub-1e-12 residue on arm64, so compare against an epsilon (see `internal/metrics/colour_test.go`). Second, the committed golden archives are an amd64 byte snapshot; the downscale and block compose diverge by a few bytes on arm64, so `TestGoldenTrack` skips when `runtime.GOARCH != "amd64"`. Regenerate the goldens on amd64 only.

## Commit conventions

Use Conventional Commits (`feat:`, `fix:`, `chore:`, `docs:`).
