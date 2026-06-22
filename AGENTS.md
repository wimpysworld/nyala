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
- `just test` - run tests.
- `just diagnostics` - run the diagnostics sweep (`nyala --diagnostics`) over the fixtures.
- `just lint` - go vet, gocyclo, ineffassign, golangci-lint, actionlint.
- `just fmt` - format with gofmt; `just fmt-check` verifies.
- `just tidy` - `go mod tidy`; `just tidy-check` verifies go.mod/go.sum are clean.
- `just coverage` - tests with a coverage report.
- `just vuln` - govulncheck.
- `just release-check`, `just release-snapshot` - GoReleaser config check and local snapshot.
- `just check` - full local gate (tidy-check, fmt-check, lint, test, build, release-check, `nix flake check`, vuln).

The `nyala` binary runs the diagnostics sweep with `--diagnostics`; `--fixture`, `--grid`, `--out`, and `--update-golden` tune the sweep, and `--version` prints the version. Without `--diagnostics` it views the positional PNG.

Run `just check` before considering any change done.

## Code style

Format with gofumpt (`just fmt` uses gofmt; both pass `fmt-check`). Lint config lives in `.golangci.yaml`; revive runs with the `exported` rule, so add a doc comment to every exported symbol you add.

## Gotchas

- RGBA byte order everywhere in the render and metrics pipeline. The decoders and image maths assume RGBA, never BGRA. Do not swap channels.
- OCR uses cgo `gosseract`, guarded behind the `//go:build ocr` tag, so the default build and the shipped binary stay cgo-free. The OCR tests need libtesseract and leptonica from the Nix dev shell: run `nix develop`, then `go test -tags ocr ./internal/ocr/...`.

## Commit conventions

Use Conventional Commits (`feat:`, `fix:`, `chore:`, `docs:`).
