# Stage 1 - Capability detection

Stage 1 makes Nyala pick its render backend and colour mode by itself, on the real target terminals, with manual override kept. It has two halves. The graphics and geometry half probes the tty with a DA1-fenced handshake to find Sixel, Kitty, and the cell pixel size, failing closed to block. The colour half detects the colour mode from env plus terminfo, adds a monochrome path, and resolves a `--color` flag against `NO_COLOR` and detection. A bare TTY still gets the block floor; a capable terminal now starts on its best backend with the right colour depth, no flag or hotkey needed. There is still no browser; the input is the static PNG from Stage 0. This document is the durable record of the completed work.

## Status summary

Stage 1 is code-complete and committed at `725ff81` (HEAD). It landed across three commits: the graphics and geometry detector (`d4d44df`), the colour half plus geometry hardening (`5988a9a`), and the backend-nomination follow-up (`725ff81`).

All 14 proposal acceptance criteria are met, each with a test, validated by independent review. `just check` passes end to end: tidy-check, fmt-check, lint, test, build, release-check, `nix flake check`, and vuln. The shipped binary stays cgo-free; `colorprofile.Detect` is pure Go and the handshake is pure terminal I/O.

What remains for later stages: the crisp text overlay arrives in Stage 2, the chromedp screencast replaces the PNG in Stage 3, and Bubble Tea chrome wraps the viewer in Stage 4. OSC 11 light/dark theming and the per-multiplexer background quirks belong to Stage 4. Multiplexer graphics passthrough and the DCS colour round-trip are not planned for any near stage (see Out of scope).

## Objective and scope

The objective: remove the last manual step in backend and colour selection on the real target terminals (KMSCON and standalone emulators), while keeping a flag override. Stage 0 chose the backend by flag and the colour by hotkey; Stage 1 chooses both at startup.

In scope, all delivered:

- A DA1-fenced startup handshake that probes Kitty, Sixel, and the real cell pixel size, and fails closed to block (`d4d44df`).
- A pure `detectColour` over env plus terminfo via `colorprofile.Detect`, with a `kmscon` and truecolour-token override.
- A third `ColourMono` colour mode: a 1-bit block silhouette on the terminal default.
- A `--color=auto|truecolor|256|none` flag, with precedence flag > `NO_COLOR` > detection.
- Focus-multiplexer detection (tmux, zellij, herdr) that gates graphics to block-only and skips the handshake; screen stays a block-only fallback.
- Geometry hardening: the existing resolution chain, an 8x16 default, and a winsize sanity bound.
- Backend nomination: the enabled cycle is ordered ceiling-first, so the default active backend is the best the terminal supports.

Out of scope, deferred:

- The DCS/OSC truecolour round-trip. Dropped; not planned.
- OSC 11 light/dark theming and the per-multiplexer background quirks (Stage 4 chrome).
- Multiplexer graphics passthrough, Kitty or Sixel through tmux, zellij, or herdr (Overview Option C); never auto-enabled.
- The character-density ramp and the browsh-style text-only monochrome variant; text-only needs the Stage 2 overlay.
- Bubble Tea v2 typed-message capability handling (Stage 4); Stage 1 runs on the raw tty before Bubble Tea exists.
- Per-terminal hardcoded geometry overrides; the sanity bound plus the 8x16 default cover the in-scope terminals.

The target architecture and the later-stage research are in [`OVERVIEW.md`](./OVERVIEW.md).

## Graphics and geometry detection

The detector runs one startup handshake on the raw tty before the full-screen switch and the key reader, so the replies are not consumed by the key goroutine. It returns which graphics backends work plus the real cell pixel size (`internal/viewer/detect.go`).

📌 KEY: DA1 is the fence. One write batches the Kitty graphics query (`a=q`), the three cell-size queries (`CSI 16t`, `14t`, `18t`), then the DA1 request, in that order. A terminal answers DA1 after any earlier replies, so DA1 arrival means the whole batch is buffered. A bare TTY never answers DA1, so the read times out at 200 ms and detection fails closed to block only. The read loop uses `unix.Poll`, not `SetReadDeadline`, because the tty returns `ErrNoDeadline`.

- Kitty support: the terminal answers `a=q` with an APC introduced by `\x1b_G` containing `;OK`.
- Sixel support: the DA1 attribute list contains the element exactly `"4"`, matched as a split element so `64` and `14` do not false-match.
- The parse functions (`parseKittySupport`, `parseDA1Sixel`, `parseCellPixels`) are pure and unit-tested; the I/O is isolated in `readUntilDA1`.

⚠️ CAVEAT: the `CSI t` replies order parameters height-then-width, the opposite of `GetSize`. The parser handles this, but it is a known footgun. A test in `internal/viewer/detect_test.go` (`TestParseCellPixels`, the `16t direct` case) pins a `16t` reply of height 16, width 8 resolving to 8 wide by 16 tall.

## Colour detection

The colour decision is `detectColour` in `internal/viewer/detect_colour.go`, a pure function over an injected `environ`. It wraps `colorprofile.Detect`, which reads `COLORTERM`, terminfo `Tc`/`RGB`, a `-direct` `TERM` suffix, and a known-emulator list, all pure Go with no live query.

📌 KEY: colour detection is env plus terminfo, no DCS round-trip. This supersedes the `docs/OVERVIEW.md` rule that mandated a DCS query/response and forbade `COLORTERM` sniffing; that rule was updated in `5988a9a`. The premise of the old rule (KMSCON does not set `COLORTERM`) is outdated: KMSCON now sets `COLORTERM=kmscon` or `truecolor`, and the DCS round-trip is a no-op on KMSCON's libtsm (it answers an OSC 11 query but does not implement OSC colour set, and treats DCS as a no-op, so an exact-echo probe gives a false negative on a terminal that genuinely renders 24-bit SGR). Env plus terminfo already covers KMSCON and every modern emulator.

The profile maps exhaustively: `TrueColor` to `ColourTrue`; `ANSI256`, `ANSI`, `ASCII`, `NoTTY`, `Unknown` to `ColourXTerm256`.

📌 KEY: `detectColour` is hermetic. It always passes `io.Discard` to `Detect`, never `os.Stdout`. When `isTTY` is true it appends `TTY_FORCE=1` to a copy of the injected environ, so `Detect` honours terminfo without a live terminal. When `isTTY` is false, `io.Discard` with no `TTY_FORCE` yields `NoTTY`, so `detectColour(nil, false)` returns `ColourXTerm256` with no exec. This is a deviation from the proposal's first description, which passed a live `term.File` when `isTTY` was true. The shipped function is purer and testable without a terminal; the `isTTY` flag drives `Detect` through the env, not through a live writer.

📌 KEY: a present `COLORTERM` in `{kmscon, truecolor, 24bit}` is a trusted positive and short-circuits to `ColourTrue` before `Detect` runs. `colorprofile` upgrades on `COLORTERM` only when `TERM` is also set and non-dumb, and it caps `kmscon` at ANSI. Stage 1 treats the present token as truecolour regardless of `TERM`. This is broader than the overview's literal "kmscon override only", and it matches the proposal rule "treat a present `COLORTERM` as a trusted positive". This is the one place Nyala must not defer to `colorprofile`.

⚠️ CAVEAT: under tmux, `colorprofile.Detect` execs `tmux info` once to read the user's `Tc`/`RGB` override. This startup exec is accepted as the only reliable way to honour a tmux truecolour override. It runs on the colour path, which is multiplexer-agnostic, so it fires under tmux even when graphics are gated off. Injecting `environ` keeps the unit tests off the exec.

## Monochrome mode

📌 KEY: monochrome is a third value of the block colour mode, not a new backend. `ColourMono` joins `ColourTrue` and `ColourXTerm256` on the `ColourMode` enum (`internal/viewer/viewer.go`). It keeps the quadrant or half-block glyph the existing luma mask already picks, but suppresses the fg/bg colour SGR and emits the terminal default, rendering a 1-bit silhouette.

The change lives in the viewer's `writeGrid`, `fgSGR`, and `bgSGR`, not in `block.Renderer.Render`. The block renderer's own `Render` stays truecolour-only and is not the colour-toggle path; the live viewer block path runs through `writeGrid`, which carries the `ColourMode`. In `ColourMono`, `fgSGR` and `bgSGR` return the empty string, so each cell writes its glyph alone and each row ends with one `\x1b[0m\r\n` reset. The block binarisation and glyph selection are untouched. The character-density ramp and the text-only readout are later refinements; text-only needs the Stage 2 overlay to be useful.

## Override and precedence

Stage 1 adds a `--color=auto|truecolor|256|none` flag to `app.CLI` (`internal/app/app.go`), default `auto`, with a kong `enum` that rejects an unknown value. It mirrored the then-current `--all-renderers` bool flag (since removed; the viewer now always auto-detects) and gives a non-interactive escape hatch for mis-detection.

📌 KEY: precedence is flag > `NO_COLOR` > detection, resolved in `resolveColour` (`internal/viewer/detect_colour.go`). An explicit `truecolor`, `256`, or `none` flag wins, mapping to `ColourTrue`, `ColourXTerm256`, or `ColourMono`. Under `auto`, a present `NO_COLOR` key forces `ColourMono`. Otherwise `auto` returns the detected mode.

⚠️ CAVEAT: the viewer owns the `NO_COLOR` rule, not `colorprofile`. `resolveColour` scans `environ` for a `NO_COLOR` key directly and treats any value as set, including empty and `0`, per no-color.org. `colorprofile` parses `NO_COLOR` with `strconv.ParseBool`, so it honours `NO_COLOR=1` but ignores `NO_COLOR=` and `NO_COLOR=foo`. The direct presence check wins over that floor. Tests pin `NO_COLOR=` (empty) and `NO_COLOR=0`.

⚠️ CAVEAT: the `--color=truecolor` value uses American spelling on purpose, to match the `COLORTERM=truecolor` token and the `colorprofile` profile name. Go identifiers stay British: `ColourMode`, `ColourTrue`, `ColourXTerm256`, `ColourMono`. The flag value must not be "corrected" to `truecolour`, or it stops matching the env token.

The `c` hotkey cycles all three modes at runtime: truecolour, then xterm-256, then monochrome, then back (`ToggleColour`), the most-local user intent.

## Focus multiplexers

📌 KEY: multiplexer detection gates graphics only; colour stays multiplexer-agnostic. `colorprofile` reads env and terminfo, which work inside any multiplexer that inherits or forwards them, so the colour decision is the same everywhere. Under a detected multiplexer the viewer offers block-only graphics and skips the tty handshake entirely (no 200 ms wait, no false DA1 parse). Cell pixel size is irrelevant under block-only, so nothing is lost.

`multiplexed()` (`internal/viewer/viewer.go`) returns true for the focus multiplexers and the screen fallback:

| Multiplexer | Detect by | Why graphics stay off |
|---|---|---|
| tmux | `TMUX` set | Sixel needs an `--enable-sixel` build plus a Sixel host plus DA1; Kitty needs DCS passthrough, Unicode placeholders, and `allow-passthrough on` (off by default); native tmux Kitty is unreleased |
| zellij | `ZELLIJ` present (value `0`; test presence) | zellij advertises Sixel in DA1 unconditionally and swallows Kitty, so a DA1/`a=q` probe is untrustworthy |
| herdr | `HERDR_ENV=1` with `HERDR_PANE_ID` present | no Sixel; only experimental, off-by-default Kitty that fails for in-pane TUIs |
| screen | `STY` set | block-only fallback |

⚠️ CAVEAT: a bare `ZELLIJ=0` is a presence signal, not a truth value, so zellij is detected by key presence. herdr needs both `HERDR_ENV=1` and `HERDR_PANE_ID`, so a stray var does not gate graphics off.

## Geometry hardening

Geometry keeps the existing resolution chain: `TIOCGWINSZ` ioctl first (when `ws_xpixel` and `ws_ypixel` are both non-zero), then `CSI 16t` direct, then `CSI 14t` divided by the cell counts from `CSI 18t` or the winsize, then a hardcoded default.

📌 KEY: the default when all reports fail is 8x16 px, aspect 1:2. It matches the project's pinned Route A raster (`docs/STAGE-0.md`) and the fbcon default, so the bitmap-to-cell mapping does not distort. On a GUI terminal that reports nothing, 8x16 errs slightly narrow and short, which under-fills rather than over-draws, so the overlay cannot drift past a cell boundary.

⚠️ CAVEAT: winsize gives total pixels, so cell px is `xpixel/cols` by `ypixel/rows`; some WSL and telnet paths report `0` or garbage like `32000x0`. Stage 1 adds a sanity bound: a derived cell px outside 4..32 wide or 8..64 tall is rejected and the caller falls through. The bound is a heuristic, not a measured constant, but it only ever rejects to the safe 8x16 default, which under-fills rather than over-draws.

The winsize derivation is split into pure helpers for testability: `winsizeCellPixels` does the ioctl, `deriveCellPixels` does the zero-guard and the maths, and `cellPxInBounds` applies the sanity window (constants `winsizeMinCellW` 4, `winsizeMaxCellW` 32, `winsizeMinCellH` 8, `winsizeMaxCellH` 64). `deriveCellPixels` returns `ok=false` when any winsize field is zero or the derived size is out of bounds, so the caller falls through to the handshake or the default.

## Backend nomination

📌 KEY: the enabled cycle is ordered ceiling-first, so the default active backend is the best the terminal supports. `configureBackends` builds the `Enabled` slice with Kitty first, then Sixel, then Block as the floor. `SetEnabled` sets the active backend to `enabled[0]`, the best available, and `Tab` cycles down. Block is always present and last.

Before `725ff81` the default active backend was the floor (block). The follow-up reordered the slice so a Kitty or Sixel terminal opens on its highest-fidelity backend without a `Tab` press. `NewState` seeds `Enabled` with `{Kitty, Sixel, Block}` as the pre-detection default; `configureBackends` narrows it after the handshake, dropping any backend the terminal does not confirm, then `SetEnabled` nominates the best survivor.

`CycleBackend` steps only enabled backends and wraps. With block only it stays on block.

## Work completed

Stage 1 landed across three commits.

- `d4d44df` feat(viewer): detect terminal graphics capabilities and select enabled backends. Added the DA1-fenced handshake that probes Kitty (`a=q`), the three cell-size queries, and DA1, parses Sixel and Kitty support, and resolves the real cell pixel size, with best-fit graphics scaling. Added `State.SetEnabled` to configure the `Tab` cycle and updated `CycleBackend` to step only enabled backends. Added the `--all-renderers` flag to force every backend (since removed; the viewer now always auto-detects). Offers block only under tmux or screen; runs full detection on a bare TTY. Moved viewer state init before the full-screen entry so detection runs early. This detector landed in the Stage 0 era but is Stage 1 scope. Rationale: an early, conservative detector improves the default before the full Stage 1 work, and gives Stage 1 working code to extend rather than a blank page.
- `5988a9a` feat(viewer): detect colour mode and harden geometry. Added the colour half: `detectColour` and `resolveColour` in a new `detect_colour.go`, the `ColourMono` mode and its `writeGrid`/`fgSGR`/`bgSGR` branch, the three-mode `ToggleColour`, the `--color` flag with `NO_COLOR` precedence, and the zellij and herdr multiplexer keys. Hardened geometry: the winsize sanity bound (`deriveCellPixels`, `cellPxInBounds`) and the 8x16 default. Promoted `colorprofile` to a direct require. Updated the superseded `docs/OVERVIEW.md` truecolour rule. Added colour, precedence, multiplexer, monochrome, and geometry tests. Rationale: env plus terminfo is correct on the real targets and avoids the fragile DCS round-trip.
- `725ff81` feat(viewer): reorder backends to prioritise highest fidelity (HEAD). Reordered the `Enabled` slice to Kitty, Sixel, Block, so the default active backend is the best detected, not the floor. Updated `configureBackends`, `NewState`, and the `SetEnabled` doc; added cycle tests for the new order; updated `README.md`. Rationale: a capable terminal should open on its best backend without a `Tab` press.

## Decisions recorded with rationale

- Colour detection is env plus terminfo via `colorprofile.Detect`, no DCS round-trip. The round-trip is fragile (DECRQSS no-op on KMSCON; zellij can leak the reply into stdin; herdr stops answering after a child sets a background) and unnecessary, since env plus terminfo covers the targets. This supersedes the `docs/OVERVIEW.md` DCS-mandate rule, updated in `5988a9a`.
- `detectColour` is hermetic: always `io.Discard`, with `TTY_FORCE=1` appended to a copied environ when `isTTY` is true. It never reads `os.Stdout`. This deviates from the proposal's first description (a live `term.File`); the shipped form is chosen for testability and purity, and `detectColour(nil, false)` runs no exec.
- A present `COLORTERM` in `{kmscon, truecolor, 24bit}` is a trusted positive and short-circuits to `ColourTrue` before `Detect`. This is broader than the overview's "kmscon override only", because `colorprofile` upgrades on `COLORTERM` only with a non-dumb `TERM`. It matches the proposal's "treat a present `COLORTERM` as a trusted positive".
- Three colour modes: truecolour, xterm-256, monochrome. Monochrome is a third value of the block colour mode, not a new backend. The toggle lives in the viewer's `writeGrid`/`fgSGR`/`bgSGR`; `block.Renderer.Render` stays truecolour-only.
- Precedence is flag > `NO_COLOR` (any value, including empty and `0`) > detection. The viewer owns the `NO_COLOR` rule and checks presence directly, beating `colorprofile`'s `strconv.ParseBool` floor.
- The `--color=truecolor` value is deliberately American to match the `COLORTERM` token; Go identifiers stay British.
- Focus multiplexers are tmux (`TMUX`), zellij (`ZELLIJ` present), and herdr (`HERDR_ENV=1` with `HERDR_PANE_ID`); screen (`STY`) is a block-only fallback. A detected multiplexer gates graphics to block-only and skips the handshake. Colour stays multiplexer-agnostic.
- Geometry keeps the existing chain, defaults to 8x16 (1:2) when all reports fail, and rejects winsize-derived cell px outside ~4..32 wide or 8..64 tall via a sanity bound, extracted into the pure `deriveCellPixels`/`cellPxInBounds` helpers.
- Backend nomination orders the `Enabled` slice ceiling-first (Kitty, Sixel, Block), so the default active backend is the best the terminal supports and `Tab` cycles down. Block is always the floor, last.

Deferred to later stages: the DCS round-trip (not planned); OSC 11 light/dark theming and the per-multiplexer background quirks (Stage 4); multiplexer graphics passthrough (Option C); the character-density and text-only monochrome variants; Bubble Tea v2 typed-message capability handling (Stage 4); `colorprofile.NewWriter` chrome colour downsampling (Stage 4).

## Acceptance criteria and how each was met

All 14 are met, each with a test.

1. `detectColour` returns `ColourTrue` for `COLORTERM=truecolor`, `24bit`, and `kmscon`; returns `ColourXTerm256` when the profile is below TrueColor and `COLORTERM` is absent: met. Table test over injected `environ` (`detect_colour_test.go`).
2. `detectColour(nil, false)` returns `ColourXTerm256`, never panics, runs no exec: met. The `io.Discard` writer with no `TTY_FORCE` yields `NoTTY`; asserted by test.
3. Precedence flag > `NO_COLOR` (including empty and `0`) > detection: met. `resolveColour` table test, with `NO_COLOR=` and `NO_COLOR=0` rows proving the viewer owns the "any value" rule.
4. `ToggleColour` cycles `ColourTrue` -> `ColourXTerm256` -> `ColourMono` -> `ColourTrue`: met. Sequence test.
5. `writeGrid` with `ColourMono` emits each glyph with no `\x1b[38;` and no `\x1b[48;` and one reset per row: met. Substring test on the written bytes.
6. `multiplexed` is true for `TMUX`, `ZELLIJ`, `HERDR_ENV=1`+`HERDR_PANE_ID`, and `STY`, false when none is set: met. Env-injected table test, including the bare-`ZELLIJ=0` and herdr two-key cases.
7. Under a detected multiplexer, `configureBackends` enables block only and does not call `probeTerminal`: met. Test sets a mux env var and checks the enabled set with no handshake side effect.
8. With all geometry reports absent, the resolved cell pixel size is 8x16: met. Test drives the chain with empty replies and cites the source.
9. `winsizeCellPixels` returns `ok=false` for a derived cell px outside the bound (for example `32000x0`): met. Boundary table test on `deriveCellPixels`/`cellPxInBounds`.
10. The height-then-width `CSI t` parse stays pinned: met. The existing `TestParseCellPixels` `16t direct` case stays green; no code change.
11. `--color` parses with default `auto`, accepts `truecolor`, `256`, `none`, and rejects an unknown value: met. `app_test.go` parse cases.
12. `go.mod` lists `colorprofile` as a direct require; `just tidy-check` passes: met.
13. The default build is cgo-free: `CGO_ENABLED=0 go build ./...` succeeds: met.
14. `just check` passes: met, end to end.

## Risks identified and how they were resolved or carried forward

- Superseded rule conflict: resolved. The `docs/OVERVIEW.md` DCS-mandate line was updated in `5988a9a` to point at the Stage 1 env-plus-terminfo decision, so it no longer contradicts the code.
- `colorprofile` API surface: resolved. Pinned at the vendored version; the mapping switch is exhaustive over `TrueColor`, `ANSI256`, `ANSI`, `ASCII`, `NoTTY`, `Unknown`.
- tmux exec side effect: carried forward as accepted. `colorprofile.Detect` execs `tmux info` once at startup under tmux. The `environ` injection keeps tests off the exec; the function comment records it.
- Two SGR-emission sites: resolved. Colour SGR exists in both `block.Renderer.Render` (truecolour-only, harness path) and the viewer `writeGrid` (mode-aware, live path). Stage 1 changed only `writeGrid`; the block renderer stays truecolour-only and is recorded as not the toggle path.
- Multiplexer env false positives: resolved. zellij is detected by key presence (a bare `ZELLIJ=0` counts), and herdr needs both `HERDR_ENV=1` and `HERDR_PANE_ID`, asserted by test.
- Geometry bound chosen by hand: carried forward as accepted. The 4..32 / 8..64 window is a heuristic, not a measured constant. It only rejects to the safe 8x16 default, which under-fills rather than over-draws.
- cgo-free constraint: resolved. `colorprofile.Detect` is pure Go, so promoting it to a direct dependency does not pull cgo into the shipped binary; OCR `gosseract` stays the only cgo code, behind its build tag.
