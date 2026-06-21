# 🦌 Nyala

**A browser for the terminal.**

Nyala is a browser for the terminal. The goal: render real web pages - HTML5, CSS, JavaScript - directly to a TTY. It will drive headless Chromium via the Chrome DevTools Protocol and composite a crisp Unicode text layer over a quadrant-block bitmap, so text stays sharp rather than fuzzy. The primary target is a bare Linux console running [KMSCON](https://github.com/kmscon/kmscon), with no display server required.

> **Early development.** The three rendering backends and the measurement harness are built (Stage 0); they render static images today. The browser pipeline, the crisp text overlay, and terminal-capability detection are the design goal, not yet wired. See [`docs/OVERVIEW.md`](docs/OVERVIEW.md) for the architecture and [`docs/STAGE-0.md`](docs/STAGE-0.md) for what Stage 0 covers.

## Why Nyala?

A nyala is a browsing antelope - so the name is a literal pun on "browser". Five letters, easy to say, and the animal makes a natural mascot.

## Rendering

Three rendering backends sit behind one `Renderer` seam. Swapping backend changes only the renderer, nothing above it. They render static images today; the browser pipeline comes later.

| Backend | Protocol | Fidelity | Notes |
|---|---|---|---|
| **Block** | None; plain Unicode glyphs | Universal floor | The mandatory floor. Runs on any TTY, including a bare Linux console under KMSCON with no graphics protocol. Quadrant (2x2) blocks by default, half-block (1x2) as the floor. Truecolour where the terminal supports it, with a closed-form xterm-256 fallback. |
| **Sixel** | Sixel graphics | Higher | Higher-fidelity bitmap than blocks, for terminals that speak Sixel. |
| **Kitty** | Kitty graphics | Highest | Lossless RGBA, the sharpest path, for terminals that speak the Kitty protocol. |

Block is the universal path and the floor. Sixel raises fidelity where supported. Kitty is the top of the ladder. For now the backend is chosen by flag or hotkey; automatic capability detection is a later stage.

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
