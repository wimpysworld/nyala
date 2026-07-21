# Nyala

Nyala is a terminal web browser that drives headless Chromium over CDP and
renders through Block, Kitty, or Sixel backends. The Go module is
`github.com/wimpysworld/nyala` and requires Go 1.26.

## Project structure

- `cmd/nyala` contains the entrypoint and kong CLI wiring.
- `internal/engine` launches Chromium, streams screencasts, extracts the DOM
  text layer, and emits synced `render.Frame` values.
- `internal/render` owns the frame seam and the Block, Kitty, and Sixel backends.
- `internal/cellraster` rasterises Block grids for browser-free quality tests.
- `internal/renderdecode` decodes Kitty and Sixel output for correctness tests.
- `internal/metrics` owns image, placement, paint, scroll, and fixture-gate metrics.
- `internal/harness` owns the browser-free diagnostics harness and golden data.
- `internal/viewer` owns terminal capability detection, backend cycling, stream
  rendering, and the shared production Block compose seam.
- `testdata/corpus` contains the self-contained CDP correctness fixtures.

Read `docs/OVERVIEW.md` for architecture and `docs/STAGE-0.md` through
`docs/STAGE-3.md` for historical stage decisions.

## Setup

Use the pinned Nix development shell through `nix develop` or direnv. Prefer
`just` recipes over ad-hoc commands. Run `just` to list them.

Chromium is a runtime dependency. The binary resolves it through `--browser`,
then `NYALA_BROWSER`, `CHROME_PATH`, and `PATH`.

## Build and test

- `just run <url>` builds and launches Nyala. Do not run `./nyala` directly or
  hand-roll PTY, terminal geometry, or sandbox flags.
- `just debug-run <url>` prints interactive load milestones through
  `NYALA_DEBUG_TIMING=1`. It requires a TTY.
- `just test` runs the fast unit-test gate.
- `just diagnostics` is the single correctness workflow. It runs the
  browser-free harness, independent Block, Kitty, and Sixel protocol checks,
  and Chromium-fed corpus placement, paint, and scroll checks. Evidence goes to
  gitignored `testdata/diagnostics/`.
- `just block-interop`, `just kitty-interop`, `just sixel-interop`, and
  `just cdp-interop` are focused development selectors, not replacement gates.
- `just live` runs the live Chromium integration tests.
- `just update-golden` regenerates committed bitmap and overlay goldens.
- `just fmt` formats with the configured gofumpt formatter.
- `just build`, `just test`, and `just lint` must pass after code changes.
- `just check` runs the full local pre-commit gate.

Do not add a bounded recorder, diagnostic bundle API, capture environment
switch, or capture-only render path. Diagnostics must drive normal engine and
viewer seams. Keep `NYALA_CELL_PX` as the diagnostic geometry override and keep
`NYALA_DEBUG_TIMING` and `NYALA_FPS_TIMING` as live timing controls.

Run Chromium-backed diagnostics only through the supported `just` recipes.
Never set `FENCE_SANDBOX` by hand.

## Rendering rules

- Keep pixels in RGBA byte order throughout rendering and metrics code.
- Block uses JPEG q90 screencast frames in production. Sixel requests PNG.
  Kitty's direct fallback passes CDP PNG through as `t=d`; its shm fast path
  captures JPEG q90, decodes it, and sends raw `f=32` RGBA over POSIX shared
  memory. Diagnostics capture stays lossless PNG.
- Block diagnostics must use `viewer.State.ComposeBlockFrame`, the same compose
  result consumed by production terminal writing. Do not add a second compositor.
- The crisp text overlay is first-writer-wins. A wide grapheme's trailing cell
  carries background only; terminal writers emit nothing for that cell.
- Keep `internal/ocr` pure Go and cgo-free.
- Use epsilon comparisons for floating-point image metrics. amd64 and arm64 can
  differ below 1e-12 because arm64 contracts operations to FMA.
- Regenerate bitmap and overlay golden archives on amd64 only. Their tests skip
  on other architectures.

## Code style

Use British English in documentation. Add a doc comment to every exported
symbol. Preserve unrelated worktree changes.

## Commit conventions

Use Conventional Commits (`feat:`, `fix:`, `chore:`, `docs:`).
