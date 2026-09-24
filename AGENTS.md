# Nyala

Nyala is a terminal web browser that drives headless Chromium over CDP and
renders through Block, Kitty, or Sixel backends. The Go module is
`github.com/wimpysworld/nyala` and requires Go 1.26.

## Project structure

- `cmd/nyala` contains the entrypoint and kong CLI wiring.
- `internal/engine` launches Chromium, streams screencasts, extracts the DOM
  text layer, dispatches input, owns the page search and selection reads, and
  emits synced `render.Frame` values.
- `internal/render` owns the frame seam and the Block, Kitty, and Sixel backends.
- `internal/cellraster` rasterises Block grids for browser-free quality tests.
- `internal/renderdecode` decodes Kitty and Sixel output for correctness tests.
- `internal/metrics` owns image, placement, paint, scroll, and fixture-gate metrics.
- `internal/harness` owns the browser-free diagnostics harness and golden data.
- `internal/viewer` owns terminal capability detection, backend cycling, key
  and mouse routing, stream rendering, and the shared production Block compose
  seam.
- `testdata/corpus` contains the self-contained CDP correctness fixtures.

Read `docs/ARCHITECTURE.md` for the design record and decision rationale.

## Setup

Use the pinned Nix development shell through `nix develop` or direnv. Prefer
`just` recipes over ad-hoc commands. Run `just` to list them.
Install Tailor separately before `just alter` or `just measure`. Follow the
revision requirement in `README.md`, because the pinned shell does not supply it.

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
- `just genruns` rewrites only the foreground colours in
  `testdata/overlay/*.runs`. Write each line by hand as
  `col row fgHex rank text`; the rank column is mandatory, because an optional
  one cannot be told from text that starts with an integer.
- Customise `.golangci.yml` directly. Tailor supplies this unmarked default through
  ordinary `first-fit` and leaves the existing file unchanged.
- Do not edit the Tailor-managed `just/go.just` or `nix/go.nix` directly.
- `just check-go` checks formatting, module files, lint, vulnerabilities, full vet,
  and tests without changing source or module files.
- `just fmt` formats with the configured gofumpt formatter. Use `just fmt-check`
  and `just tidy-check` for source-preserving checks.
- `just coverage` retains atomic counters, the temporary report, and the function
  summary. Direct `just coverage-go` writes gitignored `coverage.out`.
- `just vuln` uses the full managed Go lint recipe. `just vet` runs standalone vet.
- `just build` builds all Go packages and writes executables to `bin/`.
- Use `just build-nyala` for the stripped root binary that `run`, `fps`, and `vhs` use.
- `just build`, `just test`, and `just lint-nyala` must pass after code changes.
- `just lint-nyala` runs actionlint, golangci-lint, govulncheck, and standalone
  vet without golangci-lint exclusions.
- `just complexity` prints an optional complexity report, not a failure gate.
- `just release-nyala <version>` creates a local tag and prints publication
  instructions. It does not publish the release.
- `just check` runs the local pre-commit gate. It does not replace `just diagnostics`.

Do not add a bounded recorder, diagnostic bundle API, capture environment
switch, or capture-only render path. Diagnostics must drive normal engine and
viewer seams. Keep `NYALA_CELL_PX` as the diagnostic geometry override and keep
`NYALA_DEBUG_TIMING` and `NYALA_FPS_TIMING` as live timing controls.

Run Chromium-backed diagnostics only through the supported `just` recipes.
Never set `FENCE_SANDBOX` by hand.

## Rendering rules

- Keep pixels in RGBA byte order throughout rendering and metrics code.
- Block and Sixel use JPEG q90 screencast frames in production.
  Kitty selects its transport once at startup by capability: direct passes CDP
  PNG through as `t=d`; shm captures JPEG q90, decodes it, and sends raw `f=32`
  RGBA over POSIX shared memory. Diagnostics capture stays lossless PNG.
- Block diagnostics must use `viewer.State.ComposeBlockFrame`, the same compose
  result consumed by production terminal writing. Do not add a second compositor.
- The crisp text overlay is first-writer-wins, fed front-to-back: the fold sorts
  runs by stacking rank, front-most first, and the sort must be stable so equal
  ranks keep DOM order. A wide grapheme's trailing cell carries background only;
  terminal writers emit nothing for that cell.
- Find highlights paint transparent glyph ink on Block, where the overlay is the
  only text layer, and black ink elsewhere. Keep that split when changing either.
- Use the SIMD `emmansun/base64` for every production base64 encode and
  decode. On the frame hot path, decode through the pooled wrapper in
  `internal/engine`. The standard library `encoding/base64` is acceptable
  only in the test-support decoders (`internal/kittyinterop`,
  `internal/renderdecode`).
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
