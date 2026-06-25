capture := ""
backend := ""

# List available recipes
default:
    @just --list

# Build Nyala
build:
    @go build -ldflags "-s -w" -o nyala ./cmd/nyala

# Build and run Nyala
[positional-arguments]
run url="https://wimpys.world" *flags: build
    #!/usr/bin/env bash
    set -euo pipefail

    capture={{ quote(capture) }}
    backend={{ quote(backend) }}
    url="$1"
    shift

    flags=()
    browser=""
    raw_flags=("$@")
    for ((i = 0; i < ${#raw_flags[@]}; i++)); do
        arg="${raw_flags[$i]}"
        case "$arg" in
            capture=*)
                capture="${arg#capture=}"
                ;;
            backend=*)
                backend="${arg#backend=}"
                ;;
            --browser=*)
                browser="${arg#--browser=}"
                ;;
            --browser)
                if ((i + 1 >= ${#raw_flags[@]})); then
                    flags+=("$arg")
                else
                    i=$((i + 1))
                    browser="${raw_flags[$i]}"
                fi
                ;;
            *)
                flags+=("$arg")
                ;;
        esac
    done

    if [ "${FENCE_SANDBOX:-0}" = "1" ] \
        && [ "${FENCE_DEBUG_CHROMIUM_NO_SANDBOX:-0}" != "1" ] \
        && [ "${NYALA_DEBUG_CHROMIUM_NO_SANDBOX:-0}" != "1" ]; then
        export FENCE_DEBUG_CHROMIUM_NO_SANDBOX=1
        printf '%s\n' 'FENCE_SANDBOX=1 detected, setting FENCE_DEBUG_CHROMIUM_NO_SANDBOX=1 for this run.' >&2
    fi

    if [ -n "$browser" ]; then
        flags+=("--browser" "$browser")
    fi

    args=()
    args+=("$url")
    if [ -n "$backend" ]; then
        args+=("--backend=$backend")
    fi
    args+=("${flags[@]}")

    cmd=(./nyala "${args[@]}")
    if [ -n "$capture" ]; then
        cmd=(env "NYALA_CAPTURE=$capture" "${cmd[@]}")
    fi

    if [ -t 0 ] && [ -t 1 ]; then
        "${cmd[@]}"
    else
        # No TTY (headless or fenced): wrap in script to allocate a pty so the
        # viewer has a terminal. A synthetic pty has no emulator, so graphics
        # backend probes fall back to block. stty fixes a deterministic grid
        # while still honouring caller-exported LINES/COLUMNS.
        if [ -z "$(command -v script)" ]; then
            printf '%s\n' 'script (util-linux) not found; needed for headless run' >&2
            exit 1
        fi
        wrapped="stty rows \"\${LINES:-40}\" cols \"\${COLUMNS:-120}\"; "
        for word in "${cmd[@]}"; do
            wrapped+="$(printf '%q ' "$word")"
        done
        script -qec "$wrapped" /dev/null
    fi

# Capture every corpus fixture on the block backend for a text-layout sweep
corpus:
    #!/usr/bin/env bash
    set -uo pipefail
    root="$(pwd)/testdata/corpus"
    fail=0
    for dir in "$root"/layout-*/; do
        name="$(basename "$dir")"
        printf '== %s ==\n' "$name"
        if ! just capture=1 run "file://${dir}index.html" backend=block; then
            printf 'capture failed: %s\n' "$name" >&2
            fail=1
        fi
    done
    exit "$fail"

# Run all local checks
check:
    @just tidy-check
    @just fmt-check
    @just lint
    @just test
    @just build
    @just release-check
    @nix flake check
    @just vuln

# Run the heavy quality gate: sweep, calibration, golden track, and overlay grade
diagnostics:
    @go test -v -tags diagnostics -count=1 ./internal/harness/

# Regenerate golden archives via the golden track writer
update-golden:
    @UPDATE_GOLDEN=1 go test -run 'TestGoldenTrack|TestOverlayGoldenTrack' -tags diagnostics -count=1 ./internal/harness/

# Regenerate the overlay artefact PNGs into testdata/diagnostics/ for eyeballing
overlay:
    @OVERLAY_ARTEFACTS=1 go test -run TestOverlayMatcherRoundTrip -tags diagnostics -count=1 ./internal/harness/

# Rasterise a captured terminal-model.json (or frame dir) into block-render.png beside it
rasterise path:
    @go run ./tools/rasterise {{path}}

# Print the KMSCON quadrant test card to the TTY (owner-verified, off CI)
kmscon-testcard:
    @go run ./tools/kmscon-testcard

# Rebuild the test page PNGs from the committed PDFs at 150 DPI for reproducibility
regen-fixtures:
    #!/usr/bin/env bash
    set -euo pipefail
    for name in testpage-01-grayscale testpage-02-basic testpage-03-cmyk testpage-04-color; do
        pdftoppm -png -r 150 -singlefile "testdata/${name}.pdf" "testdata/${name}"
    done

# Format Go sources
fmt:
    @gofmt -w ./cmd ./internal ./tools

# Check Go formatting
fmt-check:
    #!/usr/bin/env bash
    set -euo pipefail
    files="$(gofmt -l ./cmd ./internal ./tools)"
    if [ -n "$files" ]; then
        printf '%s\n' "$files"
        exit 1
    fi

# Run linters
lint:
    @go vet ./...
    @gocyclo -top 20 -avg -ignore '_test\.go$' .
    @ineffassign ./...
    @golangci-lint run
    @actionlint

# Check GoReleaser config
release-check:
    @goreleaser check

# Build a local snapshot release
release-snapshot:
    @goreleaser release --snapshot --clean --skip=sign

# Run tests with coverage
coverage:
    #!/usr/bin/env bash
    set -euo pipefail
    coverage_file="${TMPDIR:-/tmp}/nyala-coverage.out"
    go test -coverprofile="$coverage_file" -covermode=atomic ./...
    go tool cover -func="$coverage_file"

# Run tests
test:
    @go test ./...

# Run the live engine integration test (needs Chromium; off CI, not in `test`)
live:
    @go test -tags live ./internal/engine/

# Tidy Go modules
tidy:
    @go mod tidy

# Check Go modules are tidy
tidy-check:
    #!/usr/bin/env bash
    set -euo pipefail
    go mod tidy
    if [ -n "$(git status --porcelain -- go.mod go.sum)" ]; then
        git diff -- go.mod go.sum
        git status --short -- go.mod go.sum
        exit 1
    fi

# Run Go vulnerability checks
vuln:
    @govulncheck ./...

# Create a new release tag (requires VERSION=x.y.z)
release VERSION:
    #!/usr/bin/env bash
    set -euo pipefail

    if ! [[ "{{VERSION}}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
        echo "Error: VERSION must be in format x.y.z (e.g. 0.1.0)"
        exit 1
    fi

    if [ -n "$(git status --porcelain)" ]; then
        echo "Error: Working directory is not clean"
        exit 1
    fi

    if git show-ref --tags --verify --quiet "refs/tags/v{{VERSION}}"; then
        echo "Error: Tag v{{VERSION}} already exists"
        exit 1
    fi

    echo "Creating release v{{VERSION}}..."
    git tag -a "v{{VERSION}}" -m "v{{VERSION}}"
    echo "Tag v{{VERSION}} created"
    echo ""
    echo "To publish the release:"
    echo "  git push origin v{{VERSION}}"
    echo ""
    echo "This release is configured for GoReleaser to:"
    echo "  - Cross-compile binaries for linux and darwin (amd64, arm64)"
    echo "  - Generate changelog from commits"
    echo "  - Create GitHub release assets with bare binaries, tarballs, and checksums"
    echo "  - Build native packages (deb, rpm, apk, archlinux)"
    echo "  - Update the Homebrew tap (wimpysworld/homebrew-tap)"
    echo "  - Update the AUR package (nyala-bin)"
    echo "  - Generate the Nix package (pkgs/nyala/default.nix)"
    echo "  - Build and publish Docker images (ghcr.io/wimpysworld/nyala)"
    echo "  - Sign checksums with cosign (keyless)"
    echo "  - Sign Docker images with cosign (keyless)"
