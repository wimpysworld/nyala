backend := ""
cell := ""

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

    backend={{ quote(backend) }}
    cell={{ quote(cell) }}
    url="$1"
    shift

    flags=()
    browser=""
    raw_flags=("$@")
    for ((i = 0; i < ${#raw_flags[@]}; i++)); do
        arg="${raw_flags[$i]}"
        case "$arg" in
            capture=*|scroll=*|settle=*|frames=*)
                printf '%s\n' "just run: obsolete recording argument '$arg' is not supported; use just diagnostics" >&2
                exit 2
                ;;
            backend=*)
                backend="${arg#backend=}"
                ;;
            cell=*)
                cell="${arg#cell=}"
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
    env_pairs=()
    # cell overrides the terminal cell pixel size (NYALA_CELL_PX="WxH"). It is a
    # diagnostic and harness knob, so a headless
    # pty run can pin a real terminal's cell size to reproduce device quantisation.
    if [ -n "$cell" ]; then
        env_pairs+=("NYALA_CELL_PX=$cell")
    fi
    if [ ${#env_pairs[@]} -gt 0 ]; then
        cmd=(env "${env_pairs[@]}" "${cmd[@]}")
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
        # script runs the wrapped command via $SHELL. The wrapped string is POSIX
        # sh syntax (${LINES:-40} and %q quoting), so force a POSIX shell rather
        # than the caller's login shell, which may be fish and cannot parse it.
        SHELL=/bin/sh script -qec "$wrapped" /dev/null
    fi

# Run Nyala with load-path timing. Interactive tty streams and writes the stderr
# timing to NYALA_TIMING_FILE (default nyala-timings.log). Headless use fails
# because load timing is an interactive instrument.
[positional-arguments]
debug-run url="https://wimpys.world" *flags:
    #!/usr/bin/env bash
    set -euo pipefail

    if ! { [ -t 0 ] && [ -t 1 ]; }; then
        printf '%s\n' 'just debug-run: needs an interactive terminal; use just cdp-interop for headless load-path health.' >&2
        exit 1
    fi
    {
        # Interactive tty: NYALA_DEBUG_TIMING makes the engine print one stderr line
        # per load milestone; just run forwards the ambient env to nyala. The viewer
        # draws to the tty (fd 0), so redirecting stderr (fd 2) captures the timings
        # without touching the TUI. Quit nyala to end the run.
        out="${NYALA_TIMING_FILE:-nyala-timings.log}"
        rc=0
        NYALA_DEBUG_TIMING=1 just run "$@" 2>"$out" || rc=$?
        printf '\nload timings (%s):\n' "$out" >&2
        grep '^nyala timing:' "$out" >&2 || printf '  none captured\n' >&2
        exit "$rc"
    }

# Run Nyala with the live FPS meter. Sets NYALA_FPS_TIMING so the viewer draws a
# ~1 Hz fps badge over the page and prints a summary line to stdout on quit. It
# forwards the same url and flags as just run (backend=, cell=, --browser, ...).
# Interactive tty only: the meter measures the on-screen render, so a headless run
# has nothing to draw. Quit nyala to end the run and print the summary.
[positional-arguments]
fps url="https://wimpys.world" *flags: build
    #!/usr/bin/env bash
    set -euo pipefail

    url="$1"
    shift
    if [[ "$url" == url=* ]]; then
        url="${url#url=}"
    fi

    if ! { [ -t 0 ] && [ -t 1 ]; }; then
        printf 'just fps: needs an interactive terminal; the live meter measures the on-screen render.\n' >&2
        exit 1
    fi
    NYALA_FPS_TIMING=1 just run "$url" "$@"

# Run the full local gate
check: tidy-check fmt-check lint test build release-check
    @nix flake check
    @just vuln

# Run every correctness and diagnostic leg, collecting independent failures
diagnostics:
    #!/usr/bin/env bash
    set -uo pipefail
    fail=0
    run_leg() { printf '\n== %s ==\n' "$1"; shift; "$@" || fail=1; }
    chromium_found=0
    if [ -n "${NYALA_BROWSER:-}" ] || [ -n "${CHROME_PATH:-}" ]; then chromium_found=1; fi
    for name in chromium chromium-browser google-chrome google-chrome-stable chrome; do
        if command -v "$name" >/dev/null 2>&1; then chromium_found=1; break; fi
    done
    sixel_found=0
    if command -v sixel2png >/dev/null 2>&1; then sixel_found=1; fi
    run_leg 'browser-free quality' go test -count=1 -tags diagnostics ./internal/harness/
    run_leg 'Block protocol interop' go test -count=1 -tags blockinterop ./internal/viewer/ -run '^TestBlockTerminalRoundTrip$'
    run_leg 'Kitty protocol interop' go test -count=1 -tags kittyinterop ./internal/viewer/ -run '^TestKittyTerminalRoundTrip$'
    if [ "$sixel_found" -eq 1 ]; then
        run_leg 'Sixel protocol interop' go test -count=1 -tags sixelinterop ./internal/render/sixel/ -run '^TestLibsixelRoundTrip$'
    else
        printf '\n== Sixel protocol interop ==\nSKIP: sixel2png not found\n' >&2
        fail=1
    fi
    if [ "$chromium_found" -eq 1 ]; then
        run_leg 'CDP-fed correctness' env FENCE_DEBUG_CHROMIUM_NO_SANDBOX="${FENCE_SANDBOX:-0}" go test -count=1 -tags cdpinterop ./internal/viewer/ -run '^TestCDP'
    else
        printf '\n== CDP-fed correctness ==\nSKIP: Chromium not found; set NYALA_BROWSER or CHROME_PATH, or add Chromium/Chrome to PATH\n' >&2
        fail=1
    fi
    exit "$fail"

# Run the synthetic Block terminal protocol gate
block-interop:
    @go test -count=1 -tags blockinterop ./internal/viewer/ -run '^TestBlockTerminalRoundTrip$'

# Run the synthetic Kitty terminal protocol gate
kitty-interop:
    @go test -count=1 -tags kittyinterop ./internal/viewer/ -run '^TestKittyTerminalRoundTrip$'

# Run the synthetic Sixel terminal protocol gate
sixel-interop:
    @command -v sixel2png >/dev/null || { echo 'sixel2png not found' >&2; exit 1; }
    @go test -count=1 -tags sixelinterop ./internal/render/sixel/ -run '^TestLibsixelRoundTrip$'

# Run the Chromium-fed renderer correctness matrix
cdp-interop:
    @FENCE_DEBUG_CHROMIUM_NO_SANDBOX="${FENCE_SANDBOX:-0}" go test -count=1 -tags cdpinterop ./internal/viewer/ -run '^TestCDP'

# Regenerate committed golden archives
update-golden:
    @UPDATE_GOLDEN=1 go test -count=1 -tags diagnostics ./internal/harness/ -run 'Test(GoldenTrack|OverlayGoldenTrack)'

# Regenerate overlay artefacts for visual inspection
overlay:
    @go test -count=1 -tags diagnostics ./internal/harness/ -run '^TestOverlayArtefacts$'

# Print the KMSCON quadrant test card to the TTY (owner-verified, off CI)
kmscon-testcard:
    @go run ./tools/kmscon-testcard

# Regenerate the committed testdata/overlay/*.runs fixtures
genruns:
    @go run ./tools/genruns

# Rebuild the test page PNGs from the committed PDFs at 150 DPI for reproducibility
regen-fixtures:
    #!/usr/bin/env bash
    set -euo pipefail
    for name in testpage-01-grayscale testpage-02-basic testpage-03-cmyk testpage-04-color; do
        pdftoppm -png -r 150 -singlefile "testdata/${name}.pdf" "testdata/${name}"
    done

# Format Go sources
fmt:
    @golangci-lint fmt

# Check Go formatting
fmt-check:
    @golangci-lint fmt --diff --color=never

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
    #!/usr/bin/env bash
    set -uo pipefail

    # The live tests skip themselves when no Chromium resolves, so without this
    # check the whole leg passes by running nothing. Same resolution order the
    # binary uses, mirroring just diagnostics.
    chromium_found=0
    if [ -n "${NYALA_BROWSER:-}" ] || [ -n "${CHROME_PATH:-}" ]; then chromium_found=1; fi
    for name in chromium chromium-browser google-chrome google-chrome-stable chrome; do
        if command -v "$name" >/dev/null 2>&1; then chromium_found=1; break; fi
    done
    if [ "$chromium_found" -eq 0 ]; then
        printf '%s\n' 'just live: Chromium not found; set NYALA_BROWSER or CHROME_PATH, or add Chromium/Chrome to PATH. Every live test would skip, and a skip is not a pass.' >&2
        exit 1
    fi

    log="$(mktemp)"
    trap 'rm -f "$log"' EXIT
    # -v is what makes a skip visible at all: go test prints nothing for a skipped
    # test without it. The === RUN lines are dropped again for readability.
    FENCE_DEBUG_CHROMIUM_NO_SANDBOX="${FENCE_SANDBOX:-0}" \
        go test -count=1 -tags live ./internal/engine/ -v 2>&1 \
        | tee "$log" | grep -Ev '^=== (RUN|PAUSE|CONT)'
    rc=${PIPESTATUS[0]}

    # No test in this package skips on Linux or macOS today. A skip here means a
    # Chromium that resolved but would not launch, which is a failure, not a pass.
    # A legitimate new platform skip needs an explicit exception right here.
    skipped="$(grep -E '^[[:space:]]*--- SKIP: ' "$log" | sed -E 's/^[[:space:]]*--- SKIP: ([^ ]+).*/\1/' || true)"
    if [ -n "$skipped" ]; then
        printf '\njust live: these tests skipped, which is not a pass:\n%s\n' "$skipped" >&2
        rc=1
    fi
    passed="$(grep -cE '^[[:space:]]*--- PASS: ' "$log" || true)"
    if [ "$passed" -eq 0 ]; then
        printf '\njust live: no test passed; the live build tag or the package selector is wrong.\n' >&2
        rc=1
    fi
    printf '\njust live: %s tests passed, %s skipped.\n' "$passed" "$(printf '%s' "$skipped" | grep -c . || true)"
    exit "$rc"

# Measure internal renderer-to-terminal-byte costs from production CDP frames.
internal-render-metrics:
    # The fenced Chromium wrapper owns its no-sandbox diagnostic switch.
    @FENCE_DEBUG_CHROMIUM_NO_SANDBOX="${FENCE_SANDBOX:-0}" NYALA_INTERNAL_RENDER_METRICS=1 go test -tags live -run '^TestInternalRenderingMetrics$' -v -count=1 ./internal/viewer/

# Compare Sixel palette implementations on the committed still-image matrix.
palette-bench:
    #!/usr/bin/env bash
    set -euo pipefail
    for run in 0 1 2 3 4 5; do
        NYALA_PALETTE_BENCH=1 NYALA_PALETTE_BENCH_RUN="$run" \
            go test -run '^TestPaletteBenchmark$' -v -count=1 ./internal/render/sixel/
    done
    NYALA_PALETTE_BENCH=1 \
        go test -run '^TestPaletteShimmerBenchmark$' -v -count=1 ./internal/render/sixel/

# Interactive tty only: run it inside a Kitty graphics terminal. Frames per
# variant come from NYALA_KITTY_BENCH_FRAMES (default 30).
# Benchmark Kitty transports (t=d, t=f, t=s; PNG and raw) in ms/frame on the live terminal.
kitty-transport-bench:
    #!/usr/bin/env bash
    set -euo pipefail
    if ! { [ -t 0 ] && [ -t 1 ]; }; then
        printf 'just kitty-transport-bench: needs an interactive terminal; the benchmark times writes to the live terminal.\n' >&2
        exit 1
    fi
    NYALA_KITTY_BENCH=1 go test -tags kittybench -run '^TestKittyTransportBenchmark$' -v -count=1 ./internal/viewer/

# Record the demo GIF from nyala.tape. VHS records the Block backend only: its
# xterm.js emulator cannot show Kitty or Sixel output. The nixpkgs vhs package
# bundles the ttyd and ffmpeg it needs.
vhs: build
    #!/usr/bin/env bash
    set -euo pipefail

    command -v vhs >/dev/null || {
        printf '%s\n' 'just vhs: vhs not found; the nixpkgs package bundles ttyd and ffmpeg' >&2
        exit 1
    }

    mkdir -p assets
    # The tape runs ./nyala, so it records the binary just built in this worktree.
    vhs nyala.tape

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
