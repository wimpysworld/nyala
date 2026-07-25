<h1 align="center">
  <img src="assets/nyala-logo.png" width="256" height="256" alt="Nyala">
  <br />
  Nyala
</h1>

<p align="center"><b>A browser for the terminal 🦌</b></p>
<p align="center">Made with 💝 for 🐧🍏</p>

Nyala is a browser for the terminal. The goal: render real web pages - HTML5, CSS, JavaScript - directly to a TTY. It will drive headless Chromium via the Chrome DevTools Protocol and composite a crisp Unicode text layer over a block bitmap, so text stays sharp rather than fuzzy. On a modern terminal it renders high-fidelity output through the Kitty graphics protocol or Sixel. And it runs anywhere, down to a bare Linux console under [KMSCON](https://github.com/kmscon/kmscon), with no display server required.

> [!IMPORTANT]
> **Early development.** The browser is live: `nyala <url>` launches headless Chromium, streams a live screencast over the Chrome DevTools Protocol, and renders the page with the crisp text overlay on all three backends. See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the design.

## Why Nyala?

Agentic development is moving to a terminal-first workflow. I started using [herdr](https://herdr.dev/) to organise my projects into "spaces", each with tabs for a coding agent, [lazygit](https://github.com/jesseduffield/lazygit) and [hunk](https://www.hunk.dev/), [gh dash](https://gh-dash.dev), the [fresh editor](https://getfresh.dev/), and [frogmouth](https://github.com/Textualize/frogmouth). Each space puts all my project tooling and reference at my fingertips, with one exception: a browser for project research and reference. So I made Nyala to fill that gap, taking inspiration from [browsh](https://www.brow.sh) and [carbonyl](https://github.com/fathyb/carbonyl). herdr is my preferred multiplexer for its agent integration, but [tmux](https://github.com/tmux/tmux) and [zellij](https://zellij.dev) are wildly popular, and agent-aware plugins will surely follow for both, so Nyala supports them too.

A nyala is a browsing antelope. Yes, that kind of browsing. It makes a fine mascot.

## Rendering

Three rendering backends sit behind one `Renderer` seam. Swapping backend changes only the renderer, nothing above it.

| Backend | Protocol | Fidelity | Notes |
|---|---|---|---|
| **Block** | None; plain Unicode glyphs | Universal floor | The mandatory floor. Runs on any TTY, including a bare Linux console under KMSCON with no graphics protocol. Three schemes: quadrant (2x2, the default fallback), sextant (2x3, sharper vertical resolution on terminals that self-draw it), and half-block (1x2). Live frames use JPEG quality 90. The matched 2560x2880 headless benchmark retains 6.39 ms median processing headroom while improving fidelity over q80. Truecolour where the terminal supports it, with a closed-form xterm-256 fallback. |
| **Sixel** | Sixel graphics | Higher | Higher-fidelity bitmap than blocks, for terminals that speak Sixel. Live frames use JPEG quality 90 with a temporally stable adaptive palette. |
| **Kitty** | Kitty graphics | Highest | Two transports. Shared memory (probed at startup): JPEG quality 90 decoded to raw RGBA and handed over POSIX shared memory. Direct fallback: lossless CDP PNG passed straight to Kitty framing without an image decode or re-encode. |

Kitty is the reason to run Nyala on a capable terminal: where the terminal supports it, Nyala streams raw pixels over POSIX shared memory; elsewhere it rechunks Chromium's base64 PNG for the terminal without decoding or re-encoding it. Sixel is the next step down, a high-fidelity bitmap for terminals that speak it. Block is the guarantee Nyala works on any TTY, down to a bare Linux console under KMSCON with no graphics protocol. Its sharpest mode is sextant (2x3), which Nyala uses where the terminal self-draws sextant glyphs; an unknown terminal falls back to quadrant. See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md). Nyala detects what the terminal supports and picks the highest-fidelity backend automatically: Kitty, then Sixel, then Block. Press Alt+R to cycle the supported backends. Block and Sixel use JPEG q90; direct Kitty uses PNG, and shared-memory Kitty uses JPEG.

A measurement harness scores each render for quality, cost, and legibility. Run it with `just diagnostics`.

## Terminal compatibility

Block shows the glyph mode Nyala auto-selects; `--blocks` overrides it. Yes under Sixel or Kitty means the protocol works in that terminal; No can also mean Nyala disables a protocol to dodge a terminal-side limit. Terminals disagree on the cell-pixel units they report, but Nyala normalises layout density, so every terminal lays out the same page at the same text size.

| Terminal | Block | Sixel | Kitty | Notes |
|---|---|---|---|---|
| Alacritty | Sextant | Yes | No | Sixel works in the tested environment; stock upstream differs. Built-in sextants can show a one-pixel seam. |
| Contour | Sextant | Yes | Yes | Kitty graphics need Contour 0.7 or newer. |
| Foot | Sextant | Yes | No | |
| Ghostty | Sextant | No | Yes | No Sixel support. |
| Kitty | Sextant | No | Yes | Native protocol. Shared-memory fast path where available, direct PNG fallback. |
| mlterm | Quadrant | Yes | No | High-cardinality Block colours can be sparse in the compact-truecolour build; use `--color=256`. |
| Rio | Sextant | No | Yes | Rio 0.4.x limits below. |
| WezTerm | Sextant | Yes | Yes | Sixel is documented as preliminary. |

**Rio 0.4.x.** Rio advertises Sixel but does not render it, so Nyala excludes Sixel from auto backend selection. Rio's style table caps truecolour, so auto colour uses the xterm-256 palette; `--color=256` and `--color=none` remain available, and `--color=truecolor` is rejected. Kitty graphics work, with the live fps badge omitted.

### Multiplexers

Host mode means the Block glyph mode follows the outer terminal. Nyala never offers the Kitty backend under a multiplexer.

| Multiplexer | Block | Sixel | Kitty | Notes |
|---|---|---|---|---|
| tmux | Host mode | Yes | No | Needs tmux 3.6+ built with `--enable-sixel` (3.6b+ recommended). Bottom row reserved; Sixel auto-selected. |
| zellij | Host mode | No | No | Block only; Sixel blocked by upstream zellij defects. |
| GNU screen | Host mode | No | No | No graphics passthrough. |
| herdr | Host mode | No | No | Consumes graphics sequences; Block only. |

Deeper design rationale lives in [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md); performance measurements in [`docs/PERFORMANCE.md`](docs/PERFORMANCE.md).

## Development

Enter the pinned environment with `nix develop`. `just test` runs the routine,
pure-Go unit suite. `just block-interop` sends two frames through the production
Block compose and live cell-diff encoder, decodes the CUP, SGR, and UTF-8 byte
stream with an independent VT emulator, and compares its final cell model with
the exact grid returned by the production compose seam.
This offline gate checks Nyala's byte stream and repaint state without a TTY. It
does not check a terminal's font choice, glyph metrics, ambiguous-width policy,
colour profile, or inter-row bleed; `just kmscon-testcard` and live terminal runs
cover those terminal-only properties.

`just sixel-interop` runs the tagged Sixel compatibility gate:
it sends Nyala's complete production DCS output through both go-sixel and
libsixel's `sixel2png`, then compares dimensions and pixels. The flake exposes the
same gate for every supported system. Libsixel is a test tool only and is not in
Nyala's package or runtime closure. This proves independent decoder agreement,
not display in a physical terminal; terminal parsing, passthrough, anchoring,
colour handling, and repaint behaviour still need terminal checks.

`just kitty-interop` sends successive PNG and RGB frames through the production
viewer and Kitty encoder, then reads the APC byte stream with an independent
offline terminal model. It checks control fields, 4096-byte chunk ordering and
continuation, base64 reconstruction, PNG and zlib decoding, dimensions, exact
pixels, double-buffer image IDs, and replacement deletes. It needs no physical
terminal or extra decoder. A live terminal is still needed to check APC parsing
and passthrough in that terminal, image anchoring, alpha blending, and repaint
timing.

`just cdp-interop` is the browser-fed compatibility gate. It loads representative
local HTML fixtures in Chromium, waits for settled production frames, sends them
through Block, Kitty, and Sixel, and checks independently decoded terminal output.
It retains correctness-only PNG and JSON evidence under
`testdata/diagnostics/cdp-interop/`. The synthetic interop gates remain the faster
protocol-edge checks. This gate has no timing assertions and does not replace
physical-terminal checks.

## Install

**Homebrew**
```bash
brew install wimpysworld/tap/nyala
```

**Nix**
```bash
nix run github:wimpysworld/nix-packages#nyala
```

**Docker**
```bash
docker run --rm ghcr.io/wimpysworld/nyala
```

**Linux packages and binaries** - `.deb`, `.rpm`, `.apk`, Arch, and bare binaries are on [GitHub Releases](https://github.com/wimpysworld/nyala/releases). An AUR package `nyala-bin` is also available.

**GitHub Actions**
```yaml
- uses: wimpysworld/nyala@v0
```

## Usage

Open a page:

```bash
nyala example.com
```

A bare host gains an `https://` scheme. The `http`, `https`, and `file` schemes are accepted; others are rejected. Nyala launches headless Chromium, streams the live page, and renders it to the TTY. Press Alt+R to cycle the supported backends. The mouse works too: the wheel scrolls the page at the pointer and clicks reach the page.

Nyala needs a Chromium executable at runtime. It resolves the path from `--browser`, then `NYALA_BROWSER`, then `CHROME_PATH`, then a `PATH` lookup. The Nix package ships its own Chromium.

Behind a proxy? Nyala reads the standard proxy environment variables and routes Chromium through them. It honours `HTTPS_PROXY`, `HTTP_PROXY`, and `ALL_PROXY` (and their lowercase forms), with `NO_PROXY` for hosts to skip. A proxy URL with credentials works too: Nyala authenticates over the DevTools Protocol, so the credentials stay out of the process arguments.

| Flag | Default | Description |
|---|---|---|
| `<url>` | required | Page to open. |
| `--browser` | auto | Path to the Chromium executable to drive. |
| `--disable-frame-rate-limit` | off | Disable Chromium's browser frame-rate limit for benchmark testing. This does not control terminal or display synchronisation. |
| `--backend` | `auto` | Renderer backend: `auto`, `block`, `kitty`, or `sixel`. `auto` picks the terminal's best supported backend. A named backend selects the starting renderer and fails fast if the terminal cannot run it; Alt+R still cycles all supported backends. |
| `--blocks` | `auto` | Block glyph mode: `auto`, `sextant`, `quadrant`, or `half`. `quadrant` (2x2) and `half` (half-block) map to the existing schemes; `sextant` renders 2x3 sextant blocks (Unicode legacy-computing) for sharper vertical resolution. `auto` picks sextant on terminals known to self-draw it (a static allow-list read from environment variables) and quadrant elsewhere. |
| `--color` | `auto` | Viewer colour mode: `auto`, `truecolor`, `256`, or `none`. |
| `--version` | | Print the version and exit. |

### Keys

| Binding | Action |
|---|---|
| Arrow keys | Scroll by one line step |
| `PgUp` / `PgDn` | Scroll by one viewport height |
| `Home` | Scroll to top |
| `End` | Scroll to bottom |
| `Tab` | Focus next page element |
| `Shift+Tab` | Focus previous page element |
| `Enter` | Activate focused element |
| `Alt+Home` | Open home page (the URL Nyala was launched with) |
| `Ctrl+R` | Reload page |
| `F5` | Reload page |
| `Alt+Left` | Go back |
| `Alt+Right` | Go forward |
| `Ctrl+Q` | Quit |
| `Alt+R` | Cycle rendering backend |
| `Alt+Shift+R` | Cycle rendering backend backwards |
| `Alt+B` | Cycle Block mode: Half -> Quadrant -> Sextant (Block backend only) |
| `Alt+Shift+B` | Cycle Block mode backwards (Block backend only) |
| `Alt+C` | Cycle colour mode: True Colour -> 256-colour -> Monochrome (Block backend only) |
| `Alt+Shift+C` | Cycle colour mode backwards (Block backend only) |

Notes:

- Plain `q` does not quit. Printable keys are reserved for page input (form fields need them), so quitting is `Ctrl+Q`, with `Ctrl+C` kept as the interrupt fallback so a wedged page can never trap you.
- `Alt+Home` returns to the URL Nyala was launched with, for the lifetime of the process. It is a normal navigation, so `Alt+Left` goes back to the page you were on.
- The Block mode and colour cycles skip modes the terminal cannot render (see [Terminal compatibility](#terminal-compatibility)).
- The Block mode and colour cycles only work while the Block backend is on screen. Both cycles only affect Block rendering, so cycling them under Kitty or Sixel would change hidden state with no visible feedback, and the next switch to Block would land in an unexpected mode.
- A multiplexer with custom Alt binds can shadow the Alt chords. The zellij defaults leave `Alt+R`, `Alt+B`, and `Alt+C` free.

### Diagnostics

`just diagnostics` is the single correctness workflow. It runs the browser-free
image-quality harness, independent Block, Kitty, and Sixel protocol checks, and
Chromium-fed corpus placement, paint, and scroll checks. It keeps gitignored
evidence under `testdata/diagnostics/` and exits non-zero when a required leg
fails.

```bash
just diagnostics
```

Use `just block-interop`, `just kitty-interop`, `just sixel-interop`, or
`just cdp-interop` only when developing one leg. Nyala has no general recording
mode or diagnostic bundle format.
