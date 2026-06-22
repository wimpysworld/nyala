<h1 align="center">
  <img src="assets/nyala-logo.png" width="256" height="256" alt="Nyala">
  <br />
  Nyala
</h1>

<p align="center"><b>A browser for the terminal 🦌</b></p>

<p align="center">Made with 💝 for 🐧🍏</p>

Nyala is a browser for the terminal. The goal: render real web pages - HTML5, CSS, JavaScript - directly to a TTY. It will drive headless Chromium via the Chrome DevTools Protocol and composite a crisp Unicode text layer over a quadrant-block bitmap, so text stays sharp rather than fuzzy. On a modern terminal it renders high-fidelity output through the Kitty graphics protocol or Sixel. And it runs anywhere, down to a bare Linux console under [KMSCON](https://github.com/kmscon/kmscon), with no display server required.

> [!IMPORTANT]
> **Early development.** The three rendering backends and the measurement harness are built (Stage 0); they render static images today. The browser pipeline and the crisp text overlay are the design goal, not yet wired. See [`docs/OVERVIEW.md`](docs/OVERVIEW.md) for the architecture and progress.

## Why Nyala?

Agentic development is moving to a terminal-first workflow. I started using [herdr](https://herdr.dev/) to organise my projects into "spaces", each with tabs for a coding agent, [lazygit](https://github.com/jesseduffield/lazygit) and [hunk](https://www.hunk.dev/), [gh dash](https://gh-dash.dev), the [fresh editor](https://getfresh.dev/), and [frogmouth](https://github.com/Textualize/frogmouth). Each space puts all my project tooling and reference at my fingertips, with one exception: a browser for project research and reference. So I made Nyala to fill that gap, taking inspiration from [browsh](https://www.brow.sh) and [carbonyl](https://github.com/fathyb/carbonyl). herdr is my preferred multiplexer for its agent integration, but [tmux](https://github.com/tmux/tmux) and [zellij](https://zellij.dev) are wildly popular, and agent-aware plugins will surely follow for both, so Nyala supports them too.

A nyala is a browsing antelope. Yes, that kind of browsing. It makes a fine mascot.

## Rendering

Three rendering backends sit behind one `Renderer` seam. Swapping backend changes only the renderer, nothing above it. They render static images today; the browser pipeline comes later.

| Backend | Protocol | Fidelity | Notes |
|---|---|---|---|
| **Block** | None; plain Unicode glyphs | Universal floor | The mandatory floor. Runs on any TTY, including a bare Linux console under KMSCON with no graphics protocol. Quadrant (2x2) blocks by default, half-block (1x2) as the floor. Truecolour where the terminal supports it, with a closed-form xterm-256 fallback. |
| **Sixel** | Sixel graphics | Higher | Higher-fidelity bitmap than blocks, for terminals that speak Sixel. |
| **Kitty** | Kitty graphics | Highest | Lossless RGBA, the sharpest path, for terminals that speak the Kitty protocol. |

Kitty is the reason to run Nyala on a capable terminal: lossless RGBA, the sharpest path. Sixel is the next step down, a high-fidelity bitmap for terminals that speak it. Block is the guarantee Nyala works on any TTY, down to a bare Linux console under KMSCON with no graphics protocol. Nyala detects what the terminal supports and picks the highest-fidelity backend automatically: Kitty, then Sixel, then Block. Press Tab to cycle the supported backends, or pass `--all-renderers` to force all three into the cycle and skip detection.

A measurement harness scores each render for quality, cost, and legibility. Run it with `nyala --diagnostics`.

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
