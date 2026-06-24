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

    proxy_server=""
    for name in HTTPS_PROXY https_proxy HTTP_PROXY http_proxy ALL_PROXY all_proxy; do
        if [ -n "${!name:-}" ]; then
            proxy_server="${!name}"
            break
        fi
    done

    proxy_bypass=""
    for name in NO_PROXY no_proxy; do
        if [ -n "${!name:-}" ]; then
            proxy_bypass="${!name}"
            break
        fi
    done

    proxy_has_credentials() {
        local value="$1"
        local rest="${value#*://}"
        local authority="${rest%%[/?#]*}"
        [ "$authority" != "${authority%@*}" ]
    }

    browser_wrapper_dir=""
    cleanup_browser_wrapper() {
        if [ -n "$browser_wrapper_dir" ]; then
            rm -rf "$browser_wrapper_dir"
        fi
    }
    trap cleanup_browser_wrapper EXIT

    if [ -n "$proxy_server" ]; then
        if proxy_has_credentials "$proxy_server"; then
            printf '%s\n' 'Error: proxy URLs with credentials are not supported because Chromium exposes proxy flags in process arguments.' >&2
            exit 1
        fi

        browser_path="$browser"
        if [ -z "$browser_path" ]; then
            if [ -n "${NYALA_BROWSER:-}" ]; then
                browser_path="$NYALA_BROWSER"
            elif [ -n "${CHROME_PATH:-}" ]; then
                browser_path="$CHROME_PATH"
            else
                for name in chromium chromium-browser google-chrome google-chrome-stable chrome; do
                    if browser_path="$(command -v "$name" 2>/dev/null)"; then
                        break
                    fi
                    browser_path=""
                done
            fi
        fi

        if [ -z "$browser_path" ]; then
            printf '%s\n' 'Error: proxy environment is set, but no Chromium executable was found to wrap. Set NYALA_BROWSER or CHROME_PATH.' >&2
            exit 1
        fi

        browser_wrapper_dir="$(mktemp -d "${TMPDIR:-/tmp}/nyala-browser-wrapper.XXXXXXXXXX")"
        browser_wrapper="$browser_wrapper_dir/chromium"
        {
            printf '%s\n' '#!/usr/bin/env bash'
            printf '%s\n' 'set -euo pipefail'
            printf '%s\n' ''
            printf '%s\n' 'browser="${NYALA_WRAPPED_BROWSER:?}"'
            printf '%s\n' 'proxy_server="${NYALA_PROXY_SERVER:?}"'
            printf '%s\n' 'proxy_args=("--proxy-server=$proxy_server")'
            printf '%s\n' ''
            printf '%s\n' 'if [ -n "${NYALA_PROXY_BYPASS:-}" ]; then'
            printf '%s\n' '    proxy_args+=("--proxy-bypass-list=${NYALA_PROXY_BYPASS}")'
            printf '%s\n' 'fi'
            printf '%s\n' ''
            printf '%s\n' 'exec "$browser" "${proxy_args[@]}" "$@"'
        } > "$browser_wrapper"
        chmod 700 "$browser_wrapper"

        export NYALA_WRAPPED_BROWSER="$browser_path"
        export NYALA_PROXY_SERVER="$proxy_server"
        if [ -n "$proxy_bypass" ]; then
            export NYALA_PROXY_BYPASS="$proxy_bypass"
        else
            unset NYALA_PROXY_BYPASS
        fi

        if [ -n "$browser" ]; then
            flags+=("--browser" "$browser_wrapper")
        else
            export NYALA_BROWSER="$browser_wrapper"
        fi
    elif [ -n "$browser" ]; then
        flags+=("--browser" "$browser")
    fi

    args=()
    args+=("$url")
    args+=("${flags[@]}")

    if [ -n "$capture" ]; then
        NYALA_CAPTURE="$capture" ./nyala "${args[@]}"
    else
        ./nyala "${args[@]}"
    fi

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
