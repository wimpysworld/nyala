# Bottom Bar Overview

## Summary

Nyala gains a persistent one-row bottom bar rendered by a thin layer inside `internal/viewer`, with no TUI framework. The bar carries back, forward, reload, and home buttons, a clickable backend indicator, the URL (which expands into an inline editor), a menu button, and the relocated FPS badge. The design is settled. 📌 KEY: the viewer already owns raw mode, mouse tracking, DEC 2026 synchronized output, and a dedicated writer goroutine, so the bar is a set of bytes folded into the existing frame assembly, not a second renderer.

## Context

Nyala drives headless Chromium over CDP and renders through Block, Kitty, or Sixel backends. `internal/viewer` owns the terminal directly: raw mode via `charmbracelet/x/term` (`internal/viewer/stream.go:301`), SGR mouse modes 1002+1006 (`internal/viewer/mouse.go:167`), and a writer goroutine with ping-pong buffers (`streamSession.loop`, `stream.go:401`) emitting a 20-30fps escape stream.

Constraints that shaped the design:

- DECSDM (`ESC[?80h`, comment at `internal/viewer/viewer.go:657`) anchors the Sixel image at the screen origin. When the engine grid shrinks by one row, the free row is the bottom one. Kitty and Block home to 1;1 and are unaffected. Bottom is the only bar position that costs no render-path changes across all three backends, and Alt+R cycles backends at runtime.
- The existing tmux+Sixel bottom-row reservation `engineRows` (`stream.go:1004`) generalises to an unconditional one-row reservation. `resizeEngineOp` (`stream.go:1028`) already flows the reduced row count to the Chromium viewport.
- No tabs. One Nyala instance per terminal or terminal tab, at roughly 150-300MB of Chromium each, accepted. A shared browser process behind a socket is out of scope.
- Dependencies are already in `go.mod`: `charmbracelet/x/term`, `charmbracelet/x/ansi`, `charmbracelet/colorprofile`, `mattn/go-runewidth`. No bubbletea, lipgloss, or tcell. The bar uses raw SGR string constants like the rest of the viewer.

## Findings

### Layout and glyphs

Layout is `← │ → │ ⟳ │ ⌂ │ <indicator> <url> │ [fps │] ☰`. Button order matches the Chromium toolbar: Back, Forward, Reload, Home, address bar. ☰ is the rightmost element, anchored to the corner as a stable click target, and a │ separator sits before it like the other separators: between the FPS segment and ☰ when FPS is present, and between the URL region and ☰ when it is not. The FPS segment, when enabled, sits immediately left of ☰ so toggling it never moves the menu button; the URL region absorbs the width change. Glyph click targets pad to 3 cells. The backend indicator sits before the URL and clicking it cycles backends, mirroring Alt+R. Indicator glyphs: ⬒ U+2B12 Block half, ⬔ U+2B14 Block quadrant, ▦ U+25A6 Block sextant, ⬣ U+2B23 Sixel, ✪ U+272A Kitty. The Block glyphs echo their scheme's subcell shape with rising fill density and track the live scheme, so an Alt+B scheme cycle updates the glyph.

📌 KEY glyph rules: Basic Multilingual Plane only, nothing above U+FFFF (SMP candidates 🖽 U+1F5BD, 🅺 U+1F17A, 🞕 U+1F795 were rejected for tofu and width risk). Never emit U+FE0F. Avoid emoji-presentation lookalikes ⬅ U+2B05 and ➡ U+27A1; use ← U+2190 and → U+2192. Menu is ☰ U+2630, reload is ⟳ U+27F3, home is ⌂ U+2302.

⚠️ CAVEAT: several chosen glyphs are East-Asian-ambiguous width. Derive every segment position, mouse hit-test span, and the FPS right-align padding from `mattn/go-runewidth` measurement, so a wide rendering shifts the layout without breaking hit-testing. Truncate the URL with an ellipsis from measured remaining width.

### Colour

Default foreground/background plus the base 16 ANSI colours only; they follow the user's theme. Reverse video (SGR 7) is the base bar style and bold marks the focused element. State accents from the 16 (red for load error, yellow while loading) are v2: they wait for the loading-state field on `CurrentURL()`, so v1 ships without accents. The fixed 240 are excluded because they ignore the theme. Sixel colour registers are a private per-image namespace on modern terminals and never touch the SGR palette; terminals with shared registers already corrupt all text under Nyala's screencast, a pre-existing defect the bar cannot affect.

### Rendering

Fold the bar bytes into `assemble` (`stream.go:858`) inside the existing DEC 2026 bracket, so bar and frame present atomically. Repaint after every `clearScreen` (first draw, resize, backend switch). The bar is always exactly one row; it never grows. It draws only when the terminal is at least 2 rows tall (one bar row plus at least one page row); a one-row terminal gets the page only.

Startup: clear and draw the bar immediately at launch, with a blank page region. The backend indicator slot shows a braille spinner (`⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏`, Braille Patterns U+2800, BMP) on an 80-100ms ticker in the `waitForNavigate` select loop, and the URL segment shows the launch URL. The first frame swaps the spinner for the backend glyph. This replaces the plain-text loading cue (`stream.go:201`) and deletes its do-not-wipe guards on pre-frame resize and backend cycle, because the bar repaints after any clear. The spinner covers the initial launch only in v1; later navigations get it when a loading-state field rides `CurrentURL()`.

⚠️ CAVEAT: making `engineRows` unconditional shrinks the engine grid everywhere, and the corpus placement, paint, and scroll gates plus the committed goldens are geometry-sensitive. Check whether the diagnostics seams inherit the reservation and regenerate goldens (amd64 only) if capture geometry shifts.

The FPS badge moves from the frame buffer (`stream.go:912`) into the bar as the segment left of ☰. Its per-terminal compatibility gates (`kittyFPSBadgeAllowed`, Rio suppression) are deleted, because the bar row is outside the page image on every backend. Keep `kittyFPSPageZ` (`internal/viewer/kittyframe.go:184`): crisp DOM text on Kitty still depends on terminal text sitting above the image. Reuse the `fpsBadgeInterval` idle heartbeat to repaint the FPS segment when no page frames arrive. FPS enablement stays env-only (`NYALA_FPS_TIMING`, read once per stream); the segment exists exactly when the meter exists, and a runtime toggle is deferred until the meter lifecycle is worth touching. The segment format is `↓ NN.n │ ↑ NN.n │ ≌ N` with │ separators inside the segment: ↓ is the ingest rate (frames arriving from the engine), ↑ the output rate (frames written to the terminal), and ≌ U+224C prefixes the cumulative frame total. Rates carry one decimal place and read `--` when idle. Without an ingest reporter the ↓ field is absent.

### Input and mouse

Mouse routing is a row check: bar-row events go to bar hit-testing, everything else forwards to CDP. `decodeSGRMouse` (`mouse.go:84`) already decodes motion reports and buttons 8-11 (side back/forward) and drops both; wire side buttons to navigation. Hover feedback is deferred: press feedback only for v1, motion reports stay dropped.

Existing bindings the bar sits beside: Tab and Enter forward to Chromium for focus traversal and activation, Alt+R cycles backends (Alt+Shift+R reverse), Alt+B and Alt+C cycle Block scheme and colour mode, Alt+Left and Alt+Right step history, Alt+Home goes home, Ctrl+R and F5 reload, and Ctrl+Q or Ctrl+C quits. Plain printable bytes are dropped today, reserved for future page form input. Ctrl+L is unbound and free for URL edit mode.

URL edit mode: click the URL region or press Ctrl+L. The editor expands to the whole bar; chrome and the FPS segment hide until blur. Visual: reverse video with a leading ❯. Enter commits and navigates; Esc or a click on the page cancels. While editing, all key input routes to the editor and none to CDP, including the Alt chords (row routing plus a focus flag). Enable bracketed paste (`ESC[?2004h`) at least during edit mode and strip newlines from pastes. Schemeless input: prepend `https://` when a dot is present, otherwise error; a search-engine fallback, if it ever arrives, is a launch flag rather than stored configuration, keeping Nyala stateless. Hand-rolled single-line editor, roughly 150 lines, with a horizontal viewport offset that follows the cursor. It prefills the current URL with the cursor at the end and supports a readline subset: Ctrl+A/Ctrl+E for start/end, Ctrl+U clear, Ctrl+K kill to end, Ctrl+W delete word, plus arrows, Home, End, Delete, and Backspace. Esc closes edit and menu modes, and Esc also prefixes every Alt chord, so the parser needs lone-Esc detection (a short read timeout distinguishing bare Esc from an ESC-letter sequence). The terminal cursor is hidden at startup (`?25l`); edit mode re-shows it (`?25h`) and parks it at the edit position at the end of each assembled frame, inside the DEC 2026 bracket, because page frames keep flowing and move the cursor every paint. Blur hides it again.

⚠️ CAVEAT input gaps: the stdin parser hands over raw bytes, so multi-byte UTF-8 must be assembled into runes before insertion. `decodeCSI` (`internal/viewer/keys.go:42`) recognises only arrows and PageUp/PageDown; edit mode needs Home, End, Delete, and printable-rune routing.

### Plumbing

`internal/engine/nav.go` defines `SendNav` (`nav.go:62`) with `NavReload`, `NavBack`, `NavForward`, `NavHome`, and it is already wired into the viewer through the Alt and Ctrl bindings above; the bar buttons reuse those actions. `Navigate` is single-shot by design (`chromium.go:331`), so committing a typed URL needs a new engine method, `OpenURL(url string)`, following the `NavHome` raw `Page.navigate` path through the bounded dispatch seam and resetting the held scroll offset. ⌂ Home is the launch URL, permanently. There is no home-page configuration and never will be: a shell alias codifies a preferred home page, which fits terminal behaviour and keeps Nyala stateless.

The URL display comes from the engine. The engine holds only the immutable launch URL today, so extend the existing `Page.EventFrameNavigated` listener (`recordCommit`, `recovery.go:301`) to record the committed main-frame URL and expose it as `CurrentURL()` on the `Engine` interface. The viewer reads it on each bar repaint; page activity always produces frames, so pull-per-repaint stays fresh. ⚠️ CAVEAT: `Page.navigatedWithinDocument` is not subscribed, so SPA and fragment URL changes lag until that event joins the same handler. Loading-state accents later ride the same method as a second field.

☰ enters an in-bar menu mode, the same bar-takes-a-mode pattern as URL editing: the row content swaps to `About │ Quit │ ✕`, and Esc or ✕ restores chrome. The menu opens by mouse or Alt+F (Chromium's menu key; F10 is often grabbed by terminal emulators, so it is avoided); Esc or ✕ closes. In menu mode, Left/Right or Tab moves the highlight and Enter activates, so the menu works without a mouse. The palette is the keyboard path for the full command registry later. Nothing draws over the page region, so no overlay machinery exists yet. About is itself an in-bar mode: a single line showing the Nyala and browser versions with a Close button, `Nyala dev │ Chromium 138.0.7204.49 │ ✕`. The Nyala string is `main.version` (`cmd/nyala/main.go:14`, ldflags-set, feeds only kong today, so it needs plumbing to the viewer); the browser string comes from CDP `Browser.getVersion`, fetched once at session start, cached, and exposed by the engine, and its product field reports Chrome or Chromium as built. Quit reuses the existing teardown path. A VSCode-style command palette comes later, reuses the same command registry (fuzzy match via `sahilm/fuzzy` or the existing `agnivade/levenshtein` dependency), and is the point where overlay machinery gets built.

## Options

The framework decision, resolved:

| Option | Fit | Why rejected or chosen |
| --- | --- | --- |
| Thin viewer layer | Best | Reuses the writer goroutine, DEC 2026 bracket, and mouse parser; no second writer competing with the 20-30fps escape stream |
| Bubbletea | Poor | Renderer owns stdin/stdout; `tea.WithoutRenderer` leaves only an event loop the viewer already has |
| tcell | Poor | `tcell.Screen` owns the cell buffer; its input parser is not cleanly separable |
| vaxis | Poor | Would replace the render backends rather than sit beside them |
| Standalone bubbles widgets | Poor | Require faking `tea.Msg` values to reuse a text input that is ~150 lines to hand-roll |

→ Thin layer inside `internal/viewer`. This is final.

## Next Steps

1. Generalise `engineRows` (`stream.go:1004`) to an unconditional one-row reservation and confirm `resizeEngineOp` flows the reduced grid to Chromium on all backends.
2. Add `OpenURL(url string)` and `CurrentURL()` to the engine: record committed main-frame URLs in `recordCommit`, subscribe `Page.navigatedWithinDocument`, and route typed URLs through the `NavHome` dispatch path. Wire the bar buttons to the existing `SendNav` actions and mouse buttons 8-11 to back/forward.
3. Build the bar segment model on `go-runewidth` measurement: positions, hit spans, URL truncation, and the right-anchored ☰ plus FPS segment.
4. Fold bar bytes into `assemble` (`stream.go:858`); repaint on `clearScreen`; move the FPS badge into the bar and delete its compatibility gates, keeping `kittyFPSPageZ`. Draw the bar at launch with the braille spinner, replacing the loading cue (`stream.go:201`) and its do-not-wipe guards.
5. Route bar-row mouse events to hit-testing; add backend-indicator click cycling (the Alt+R action).
6. Extend `decodeCSI` with Home, End, Delete, and printable-rune routing; assemble UTF-8 runes from raw bytes.
7. Implement the single-line URL editor with focus routing, bracketed paste, and the schemeless-input rule.
8. Implement the in-bar ☰ menu mode on a command registry with About and Quit. About is a one-line bar mode showing `main.version` and the cached `Browser.getVersion` product with a ✕ Close. Defer the palette, overlay machinery, and search fallback.
9. Add browser-free unit tests for the pure bar logic (segment model, hit-testing, editor, mode transitions), and run `just diagnostics` to confirm the fixture gates hold with the reserved row, regenerating goldens on amd64 if geometry shifted.
