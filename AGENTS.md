# Nyala

A browser for the terminal: a TTY web browser driving headless Chromium via the Chrome DevTools Protocol. Stage 0 is built: an image-based rendering proof of concept with three swappable backends and a measurement harness. The headless-Chromium browser integration is not wired yet. See `docs/OVERVIEW.md` for the target architecture and `docs/STAGE-0.md` for the Stage 0 contract, decisions, and measurement.

## Project structure

- `cmd/nyala` - entrypoint; kong CLI wiring. `app.CLI` holds the flags.
- `internal/render` - the `Frame`/`Renderer` seam, with backends in `block`, `sixel`, `kitty`, and glyph loading in `font`.
- `internal/capture` - rasterises a frame and decodes terminal output back to pixels.
- `internal/metrics` - image and performance measures over a frame.
- `internal/ocr` - text legibility scoring (see the OCR gotcha below).
- `internal/harness` - the diagnostics sweep, golden archives, and thresholds.
- `internal/viewer` - renders a PNG to the tty.
- `docs/OVERVIEW.md` - architecture and design decisions.
- `docs/STAGE-0.md`, `docs/STAGE-1.md` - durable records of the completed Stage 0 and Stage 1 work.

Module path is `github.com/wimpysworld/nyala`. Go 1.26.

## Setup

Tools are pinned by the Nix flake. Enter the dev shell with `nix develop`, or let direnv load it via `.envrc` (`use flake`). `just` is the task runner; prefer its recipes over ad-hoc `go` commands. Run `just` to list them.

## Build and test

- `just build` - build the `nyala` binary.
- `just test` - run the fast unit tests, the routine gate. It runs no corpus sweep, no calibration, and no rasteriser-driven full-corpus measurement, so the harness package finishes in seconds.
- `just diagnostics` - run the heavy quality gate (`go test -tags diagnostics ./internal/harness/`) over the five committed fixtures (`linuxmatters.png` plus the four `testpage-*` pages) at a fixed 120x40 grid; the calibration runs over the four test pages only. It runs one corpus sweep and one calibration, shared across the tagged tests, writes its report JSON/CSV, summary, and artefact PNGs to `testdata/diagnostics/` (gitignored), gates against the committed baseline and thresholds, and runs the calibration and golden tracks. It takes minutes, so run it deliberately, not on every change.
- `just update-golden` - rerun `TestGoldenTrack` with `UPDATE_GOLDEN=1` to regenerate the committed golden archives.
- `just lint` - go vet, gocyclo, ineffassign, golangci-lint, actionlint.
- `just fmt` - format with gofmt; `just fmt-check` verifies.
- `just tidy` - `go mod tidy`; `just tidy-check` verifies go.mod/go.sum are clean.
- `just coverage` - tests with a coverage report.
- `just vuln` - govulncheck.
- `just release-check`, `just release-snapshot` - GoReleaser config check and local snapshot.
- `just check` - full local gate (tidy-check, fmt-check, lint, test, build, release-check, `nix flake check`, vuln).

The `nyala` binary views the positional PNG. Its flags are `--color` (auto, truecolor, 256, none; default auto) and `--version`. The viewer auto-detects terminal capability (Kitty, then Sixel, then Block) and Tab cycles the supported backends. The diagnostics sweep runs via `just diagnostics`; the golden archives regenerate via `just update-golden`.

When you change code, `just build`, `just test`, and `just lint` must pass; this is the routine pre-commit gate. `just diagnostics` is the heavier quality gate, run deliberately rather than on every change. `just check` runs the full local gate before considering any change done.

## Code style

Format with gofumpt (`just fmt` uses gofmt; both pass `fmt-check`). Lint config lives in `.golangci.yaml`; revive runs with the `exported` rule, so add a doc comment to every exported symbol you add.

## Gotchas

- RGBA byte order everywhere in the render and metrics pipeline. The decoders and image maths assume RGBA, never BGRA. Do not swap channels.
- OCR uses cgo `gosseract`, guarded behind the `//go:build ocr` tag, so the default build and the shipped binary stay cgo-free. The OCR tests need libtesseract and leptonica from the Nix dev shell: run `nix develop`, then `go test -tags ocr ./internal/ocr/...`.
- The heavy quality tests (corpus sweep, calibration, golden track) carry the `//go:build diagnostics` tag, so they leave the default `go test ./...` run. Run them via `just diagnostics`; regenerate the golden archives via `just update-golden`. The shared sweep and calibration run once each per tagged run, so adding a tagged test that reuses them adds no extra rasteriser pass.
- The image maths is 64-bit floating point and rounds differently across architectures (arm64 contracts to FMA, x86-64 does not). Two consequences for tests, which CI runs on `linux/amd64` and `linux/arm64`. First, never assert exact equality on metric output: identical inputs leave a sub-1e-12 residue on arm64, so compare against an epsilon (see `internal/metrics/colour_test.go`). Second, the committed golden archives are an amd64 byte snapshot; the downscale and block compose diverge by a few bytes on arm64, so `TestGoldenTrack` skips when `runtime.GOARCH != "amd64"`. Regenerate the goldens on amd64 only.

## Commit conventions

Use Conventional Commits (`feat:`, `fix:`, `chore:`, `docs:`).
