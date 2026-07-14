<h1 align="center">
  <img src="assets/nyala-logo.png" width="256" height="256" alt="Nyala">
  <br />
  Nyala
</h1>

<p align="center"><b>A browser for the terminal 🦌</b></p>
<p align="center">Made with 💝 for 🐧🍏</p>

Nyala is a browser for the terminal. The goal: render real web pages - HTML5, CSS, JavaScript - directly to a TTY. It will drive headless Chromium via the Chrome DevTools Protocol and composite a crisp Unicode text layer over a block bitmap, so text stays sharp rather than fuzzy. On a modern terminal it renders high-fidelity output through the Kitty graphics protocol or Sixel. And it runs anywhere, down to a bare Linux console under [KMSCON](https://github.com/kmscon/kmscon), with no display server required.

> [!IMPORTANT]
> **Early development.** The three rendering backends and the measurement harness are built (Stage 0), along with capability and colour detection (Stage 1) and the crisp text overlay on the block path (Stage 2). Stage 3 wires the browser: `nyala <url>` launches headless Chromium, streams a live screencast over the Chrome DevTools Protocol, and renders the page with the crisp text overlay. See [`docs/OVERVIEW.md`](docs/OVERVIEW.md) for the architecture and progress.

## Why Nyala?

Agentic development is moving to a terminal-first workflow. I started using [herdr](https://herdr.dev/) to organise my projects into "spaces", each with tabs for a coding agent, [lazygit](https://github.com/jesseduffield/lazygit) and [hunk](https://www.hunk.dev/), [gh dash](https://gh-dash.dev), the [fresh editor](https://getfresh.dev/), and [frogmouth](https://github.com/Textualize/frogmouth). Each space puts all my project tooling and reference at my fingertips, with one exception: a browser for project research and reference. So I made Nyala to fill that gap, taking inspiration from [browsh](https://www.brow.sh) and [carbonyl](https://github.com/fathyb/carbonyl). herdr is my preferred multiplexer for its agent integration, but [tmux](https://github.com/tmux/tmux) and [zellij](https://zellij.dev) are wildly popular, and agent-aware plugins will surely follow for both, so Nyala supports them too.

A nyala is a browsing antelope. Yes, that kind of browsing. It makes a fine mascot.

## Rendering

Three rendering backends sit behind one `Renderer` seam. Swapping backend changes only the renderer, nothing above it.

| Backend | Protocol | Fidelity | Notes |
|---|---|---|---|
| **Block** | None; plain Unicode glyphs | Universal floor | The mandatory floor. Runs on any TTY, including a bare Linux console under KMSCON with no graphics protocol. Three schemes: quadrant (2x2, the default fallback), sextant (2x3, sharper vertical resolution on terminals that self-draw it), and half-block (1x2). Live frames use JPEG quality 80 because block sampling and quantisation hide its artefacts. Truecolour where the terminal supports it, with a closed-form xterm-256 fallback. |
| **Sixel** | Sixel graphics | Higher | Higher-fidelity bitmap than blocks, for terminals that speak Sixel. It keeps the decoded adaptive PNG/JPEG stream. |
| **Kitty** | Kitty graphics | Highest | Lossless CDP PNG, passed straight to Kitty framing without an image decode or re-encode. |

Kitty is the reason to run Nyala on a capable terminal: every live Kitty stream requests PNG, then rechunks Chromium's base64 PNG for the terminal without decoding or re-encoding it. Sixel is the next step down, a high-fidelity bitmap for terminals that speak it. Block is the guarantee Nyala works on any TTY, down to a bare Linux console under KMSCON with no graphics protocol. Its sharpest mode is sextant (2x3), which Nyala uses where the terminal self-draws sextant glyphs; an unknown terminal falls back to quadrant. See [`docs/BLOCK-MODES.md`](docs/BLOCK-MODES.md). Nyala detects what the terminal supports and picks the highest-fidelity backend automatically: Kitty, then Sixel, then Block. Press Tab to cycle the supported backends. Diagnostic capture still requests and decodes PNG for every backend.

A measurement harness scores each render for quality, cost, and legibility. Run it with `just diagnostics`.

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

A bare host gains an `https://` scheme. The `http`, `https`, and `file` schemes are accepted; others are rejected. Nyala launches headless Chromium, streams the live page, and renders it to the TTY. Press Tab to cycle the supported backends.

Nyala needs a Chromium executable at runtime. It resolves the path from `--browser`, then `NYALA_BROWSER`, then `CHROME_PATH`, then a `PATH` lookup. The Nix package ships its own Chromium.

Behind a proxy? Nyala reads the standard proxy environment variables and routes Chromium through them. It honours `HTTPS_PROXY`, `HTTP_PROXY`, and `ALL_PROXY` (and their lowercase forms), with `NO_PROXY` for hosts to skip. A proxy URL with credentials works too: Nyala authenticates over the DevTools Protocol, so the credentials stay out of the process arguments.

| Flag | Default | Description |
|---|---|---|
| `<url>` | required | Page to open. |
| `--browser` | auto | Path to the Chromium executable to drive. |
| `--backend` | `auto` | Renderer backend: `auto`, `block`, `kitty`, or `sixel`. `auto` picks the terminal's best supported backend. A named backend forces one renderer and fails fast if the terminal cannot run it. |
| `--blocks` | `auto` | Block glyph mode: `auto`, `sextant`, `quadrant`, or `half`. `quadrant` (2x2) and `half` (half-block) map to the existing schemes; `sextant` renders 2x3 sextant blocks (Unicode legacy-computing) for sharper vertical resolution. `auto` picks sextant on terminals known to self-draw it (a static allow-list read from environment variables) and quadrant elsewhere. |
| `--layout-width` | `1280` | Target CSS layout width in pixels for the block backend; a wider terminal reflows to this width. Must be positive. |
| `--color` | `auto` | Viewer colour mode: `auto`, `truecolor`, `256`, or `none`. |
| `--version` | | Print the version and exit. |

### Capture mode

`NYALA_CAPTURE=n` records a run to disk for diagnosis. This is diagnostic capture, not benchmarking.

```bash
NYALA_CAPTURE=30 nyala example.com
```

Unset or `0` is off. `1` writes a first-frame bundle. A positive `n` draws `n` usable frames, writes one `nyala-capture-*` directory, prints its path, then exits. A full capture exits 0; a partial run (a timeout, a forced backend the terminal cannot run, or the engine closing early) writes what it has and exits non-zero. Pair it with `--backend` to pin the renderer under test.

The bundle holds the rendered viewport image, the text overlay, the terminal byte stream, per-frame timings, and the recorded CDP session and events. It can contain page pixels, page text, URLs, and proxy details, so review it before you share it.
