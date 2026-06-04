# Nyala

A browser for the terminal: a TTY web browser driving headless Chromium via the Chrome DevTools Protocol. Early development; the rendering engine is not built yet. See `docs/OVERVIEW.md` for the target architecture.

## Project structure

- `cmd/nyala` - entrypoint; kong CLI wiring (`--version`).
- `internal/` - library code (not importable outside this module).
- `docs/OVERVIEW.md` - architecture and design decisions.

Module path is `github.com/wimpysworld/nyala`. Go 1.26.

## Setup

Tools are pinned by the Nix flake. Enter the dev shell with `nix develop`, or let direnv load it via `.envrc` (`use flake`). `just` is the task runner; prefer its recipes over ad-hoc `go` commands. Run `just` to list them.

## Build and test

- `just build` - build the `nyala` binary.
- `just test` - run tests.
- `just lint` - go vet, gocyclo, ineffassign, golangci-lint, actionlint.
- `just fmt` - format with gofmt; `just fmt-check` verifies.
- `just tidy` - `go mod tidy`; `just tidy-check` verifies go.mod/go.sum are clean.
- `just coverage` - tests with a coverage report.
- `just vuln` - govulncheck.
- `just release-check`, `just release-snapshot` - GoReleaser config check and local snapshot.
- `just check` - full local gate (tidy-check, fmt-check, lint, test, build, release-check, `nix flake check`, vuln).

Run `just check` before considering any change done.

## Code style

Format with gofumpt (`just fmt` uses gofmt; both pass `fmt-check`). Lint config lives in `.golangci.yaml`; revive runs with the `exported` rule, so add a doc comment to every exported symbol. The existing `app.Message` is currently undocumented and trips this rule; document new exported symbols you add.

## Commit conventions

Use Conventional Commits (`feat:`, `fix:`, `chore:`, `docs:`).
