# 🦌 Nyala

**A browser for the terminal.**

Nyala renders real web pages - HTML5, CSS, JavaScript - directly to a TTY. It drives headless Chromium via the Chrome DevTools Protocol and composites a Unicode text layer over half-block bitmap rendering, so text stays crisp rather than fuzzy. The primary target is a bare Linux console running [KMSCON](https://github.com/kmscon/kmscon), with no display server required.

> **Early development.** The rendering engine described above is the design goal, not yet built. The CLI currently prints a welcome message and supports `--version`. See [`docs/OVERVIEW.md`](docs/OVERVIEW.md) for the full architecture.

## Why Nyala?

A nyala is a browsing antelope - so the name is a literal pun on "browser". Five letters, easy to say, and the animal makes a natural mascot.

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

## License

[BlueOak-1.0.0](LICENSE)
