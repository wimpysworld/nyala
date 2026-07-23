# Block modes - sextant selection and rendering

Nyala's block backend renders the page bitmap with Unicode block mosaics. It ships
three schemes: quadrant (2x2 sub-cells, the default floor), half-block (1x2), and
sextant (2x3, the sharpest). The `--blocks` flag selects the scheme, and on the
interactive path `auto` resolves to sextant when the terminal is known to draw
sextants cleanly, else quadrant. This document is the durable record of the block-mode
selection work and the sextant render: the flag surface, the terminal-identity
detector, the character set, and the sampling model. The target architecture is in
[`OVERVIEW.md`](./OVERVIEW.md).

## Flag surface and resolution

`--blocks=auto|sextant|quadrant|half`, default `auto`. `quadrant` and `half` force the
named scheme. `sextant` and `auto` resolve through the terminal allow-list.

Interactive path (`internal/viewer`):

- `auto` and `sextant` both resolve through `SextantSupported(os.Environ())`: a
  supported terminal gets `SchemeSextant`, an unsupported one gets `SchemeQuadrant`.
  They resolve the same way; `auto` picks sextant when supported because sextant beats
  quadrant by eye (see below).
- The fallback ladder is sextant to quadrant only. Half stays reachable through
  `--blocks=half`.
- Fallback is silent. An in-app toast for the sextant-to-quadrant degrade is a separate
  later task; see `docs/TODO.md`.

Capture path: resolution is deterministic, not terminal-dependent, so a bundle is
comparable across runs. `auto` resolves to quadrant; an explicit `sextant` or `half` is
honoured.

Sextant is the resolution winner. It beats quadrant on the page by eye, sharper with no
colour cost that matters, so `auto` defaults to it on any allow-listed terminal. No
corpus colour-error gate was added.

## Launch flags first, live cycles later

Block-mode selection starts from a launch flag. The prior live scheme toggle
(`s`) was broken: the stream key handler acted only on the backend-cycle key, so `s`
flipped state but triggered no redraw or restart, and the capture sizing was hardcoded
to quadrant, so a half-block toggle sampled a quadrant-sized bitmap. Removing it lost no
working behaviour, and the launch flag gives one selection model that is testable
offline. Live switching returned later on the shared backend-switch repaint seam:
Alt+R cycles the backend (block, Sixel, Kitty), Alt+B cycles the block scheme, and
Alt+C cycles the colour mode, each clearing and recomposing like a backend switch, so
a cycle now redraws correctly. Alt+B and Alt+C act only while block is the active
backend. Both cycles only affect block rendering, so a cycle taken under Kitty or
Sixel would change hidden state with no visible feedback, and the next block view
would arrive in an unexpected mode; the key dispatcher drops those chords instead.

## Capability detection: terminal identity, not a glyph probe

Sextant support cannot be probed by rendering a glyph. A cursor-position probe (write the
glyph, read the advance with CSI 6n) measures cell width, not glyph presence: a tofu box,
a `?`, and a real sextant each advance exactly one cell, so the probe returns a false
positive for tofu. Cursor-position probes also corrupt under a multiplexer and return
nothing on a headless backend. The only host-side method that proves glyph presence is
reading the active font file directly, which fails over SSH where the font is not on the
host.

The proven model, as notcurses does, is to identify the terminal, then gate on a static
allow-list. Nyala uses environment-variable identity as the primary detector. A pure
function reads the environment and returns a terminal name; a static allow-list keyed by
that name maps it to a supported bool. Env identity needs no escape-sequence probe and no
multiplexer passthrough. The allow-list is name-only, with no version gating.

### Allow-list

A terminal matches on any of its markers. An unmatched terminal is unsupported.

| Terminal | Env marker | Supported |
|----------|-----------|-----------|
| kitty | `TERM=xterm-kitty`, `KITTY_*` | yes |
| wezterm | `TERM_PROGRAM=WezTerm`, `WEZTERM_*` | yes |
| ghostty | `TERM_PROGRAM=ghostty`, `GHOSTTY_*` | yes |
| foot | `TERM=foot` / `foot-extra` | yes |
| alacritty | `ALACRITTY_*` | yes |
| konsole | `KONSOLE_*` | yes |
| vte (gnome-terminal) | `VTE_VERSION` | yes |
| contour | `TERM_PROGRAM=contour` | yes |
| rio | `TERM_PROGRAM=rio` | yes |
| iTerm2 | `TERM_PROGRAM=iTerm.app` | no |

iTerm2 defaults to unsupported to avoid its 3.5.x vertical-gap seam. Name-only matching
cannot exclude the bad version alone, so the whole terminal is excluded. Widening the
list and adding version gating are open follow-ups; see `docs/TODO.md`.

### Multiplexer handling

Three multiplexers are in scope: tmux, zellij, and herdr. Presence markers are `$TMUX`,
`$ZELLIJ`, and herdr's `HERDR_ENV` / `HERDR_SOCKET_PATH` (also `HERDR_PANE_ID`,
`HERDR_TAB_ID`, `HERDR_WORKSPACE_ID`). A multiplexer forwards the block glyphs to the
outer terminal, so sextant renders fine inside one when the outer terminal supports it.
Env identity survives the multiplexer: `TERM_PROGRAM` and `KITTY_*` both persist inside
herdr, so detection trusts the env-var allow-list inside tmux, zellij, and herdr rather
than bailing. `TERM` is unreliable inside a multiplexer (tmux rewrites it to
`screen`/`tmux-256color`), so prefer terminal-specific vars (`KITTY_*`) over `TERM` for a
terminal whose only bare marker is `TERM`.

## The engine SetScheme seam

The engine holds `blockScheme` and sizes the block capture per scheme through
`render.BlockCaptureSize` (`cols*2, rows*3` for sextant, `cols*2, rows*4` for quadrant,
`cols, rows*2` for half). `SetScheme` is the reconcile point: a scheme change costs a
screencast restart, not a process restart. This is the exact call a future live UI toggle
would use.

## Sextant character set

The 2x3 sextants are Unicode 13.0 (2020), in the Symbols for Legacy Computing block
`U+1FB00-1FB3B`, 60 contiguous glyphs (BLOCK SEXTANT-1 through BLOCK SEXTANT-23456). They
are not the Unicode 16 octants, which are a separate 2x4 block (`U+1CC00-1CEBF`).

A cell splits into six sub-cells, so 2^6 = 64 on/off masks. The block encodes 60 glyphs;
the four omitted masks reuse existing characters:

| Mask | Sub-cells on | Reused code point |
|------|--------------|-------------------|
| 0 | none | `U+0020` space |
| 21 | left column | `U+258C` left half block |
| 42 | right column | `U+2590` right half block |
| 63 | all six | `U+2588` full block |

So 60 sextant glyphs plus four borrowed characters cover all 64 masks. Every non-reused
mask maps to `U+1FB00 + offset`, skipping the reused masks below it:

```
0x1FB00 + (mask - 1 - (mask > 21 ? 1 : 0) - (mask > 42 ? 1 : 0))
```

### Bit order

bit0 = top-left, bit1 = top-right, bit2 = mid-left, bit3 = mid-right, bit4 = bottom-left,
bit5 = bottom-right, row-major. Four independent references agree (ratatui `pixel.rs`,
the ratatui tui-widgets PR, microsoft/terminal PR #19841, kenshaw/blocked). This is
LSB-at-top-left, the opposite end from the quadrant table's `TL<<3` convention, so the
sextant mask is built separately; do not copy the quadrant bit assembly. The 64-entry
table is hand-rolled from this reference and cross-checked byte-for-byte against
kenshaw/blocked `SextantsRunes` in a test, so a transcription error fails the gate.

Sextant touches only the bitmap layer (`Cell.Glyph`). The crisp text overlay (`Cell.Text`)
is unchanged, so the scheme does not re-enter the text-placement work.

## Sampling and binarisation

Sextant samples `cols*2` by `rows*3`, one source pixel per sub-cell (unlike quadrant's
2x4, which averages 1x2 pairs). `binarizeSextant` mirrors `binarizeQuadrant`: luma each
sub-cell (Rec.601), split at the cell midpoint `min + (max-min)/2`, set a mask bit for
each bright sub-cell, average the bright sub-cells into the foreground and the dark into
the background. Each cell carries exactly one foreground and one background colour, the
constraint every block tier shares; more sub-cells means more colour-quantisation error
against a sharper shape.

Flat-cell quirk: a uniform cell has no bright/dark split, so every sub-cell reads dark and
the mask is 0, never 63. Mask 0 emits a space with foreground equal to background, which
is visually identical to a full block with the same two equal colours. So a uniform bright
cell paints correctly despite the mask reading 0.

`applyOverlay` recomposes an overlaid cell's background from its source columns down to a
half-cell floor. The floor assumes two source columns per cell, which still holds for
sextant; only the vertical band count changes from four to three.

## Terminal support matrix

"Built-in" means the terminal draws the 2x3 mosaic itself from cell metrics, so the font
does not gate support and cells tile without gaps. This finding is decisive for Nyala: the
embedded FiraCode Nerd Font is forced on the browser page, not used for terminal output,
and FiraCode does not cover `U+1FB00-1FB3B` (most monospace and Nerd Fonts do not). The
terminal draws the block glyphs, so sextant support depends on the terminal self-drawing
them, and the font is irrelevant on the terminals Nyala targets.

| Terminal | Single-cell sextant | Renderer | Notes |
|----------|--------------------|----------|-------|
| foot | Yes | Built-in (1.7.0+) | Default built-in; opt-out to font |
| kitty | Yes | Built-in, 4x supersampled | Tiles without gaps |
| wezterm | Yes | Built-in (`custom_block_glyphs`, default on) | Font path has a width bug |
| alacritty | Yes | Built-in (0.14+, default on) | 1px seam vs half-blocks still open |
| iTerm2 | Yes | Built-in (procedural) | 3.5.x vertical-gap regression |
| vte (gnome-terminal) | Yes | Built-in (cairo, ~0.62+) | Rows sum to full height |
| konsole | Yes | Built-in (QPainter) | Exact release unconfirmed |
| contour | Yes | Built-in (default on) | No inter-cell gaps |
| rio | Yes | Built-in (0.3.0+) | Needs `fonts.use-drawable-chars=true` |
| ghostty | Yes | Built-in (sprite face) | 100% on third-party test |
| xterm | Font only | No built-in sextants | Tofu without a covering font |
| mlterm | Font only | No built-in sextants | Tofu without a covering font |
| Windows Terminal | Font only (for sextants) | Built-in box/block, sextant PR pending | Background bleed between cells |

Seam bugs that break a clean mosaic even with a built-in renderer: iTerm2 3.5.x vertical
gaps, alacritty's 1px inner-edge seam, and Windows Terminal's inter-cell bleed (its
sextants still come from the font). Config gates: wezterm, alacritty, contour, foot,
kitty, and rio all expose a toggle; defaults favour built-in, except rio needs
`use-drawable-chars` on. The allow-list must track self-drawing terminals, not just
Unicode coverage, because font glyphs with tight bounds leak background between rows.

Quadrant (`U+2580-259F`) predates sextants and more fonts and terminals carry it, so
quadrant stays the safe floor for an unknown terminal. Octants (Unicode 16, 2x4) match
cell aspect better than sextants but have far thinner terminal support and lower colour
fidelity; a sextant table generalises to octants later. Braille (2x4, `U+2800`) is the
densest mosaic but monochrome, so it does not fit Nyala's coloured page background.

## Open follow-ups

Tracked in `docs/TODO.md`: widening the allow-list as each terminal is eyeballed for
inter-row bleed (with a possible sextant test card and version gating), and the in-app
fallback toast.

## References

[1] Symbols for Legacy Computing - Wikipedia. https://en.wikipedia.org/wiki/Symbols_for_Legacy_Computing - block range, Unicode 13.0, 60 sextants.
[2] Symbols for Legacy Computing NamesList - Unicode. https://unicode.org/charts/nameslist/n_1FB00.html - sextant names, "divided into six parts".
[3] Unicode 16.0 code chart U160-1FB00.pdf. https://unicode.org/charts/PDF/Unicode-16.0/U160-1FB00.pdf - SEXTANT-1 to SEXTANT-23456 listing.
[4] microsoft/terminal PR #19841. https://github.com/microsoft/terminal/pull/19841 - 6-bit sub-cell grid, space/258C/2590/2588 reuse, background bleed.
[5] bun-win32 glyphs.ts. https://github.com/ObscuritySRL/bun-win32/blob/main/packages/terminal/glyphs.ts - mask-to-codepoint formula, reuse tables.
[6] ratatui pixel.rs. https://github.com/ratatui/ratatui/blob/1ce29d66/ratatui-core/src/symbols/pixel.rs - 64-entry sextant table, row-major bit order; scruss img2blocks documents an alternate bit convention.
[7] Symbols for Legacy Computing Supplement - Wikipedia. https://en.wikipedia.org/wiki/Symbols_for_Legacy_Computing_Supplement - block U+1CC00-1CEBF, Unicode 16.0, 230 octants.
[8] Supplement NamesList - Unicode. https://www.unicode.org/charts/nameslist/n_1CC00.html - octant names, "divided into eight parts".
[9] FiraCode PR #1566 / issues #972, #1418. https://github.com/tonsky/FiraCode/pull/1566 - only U+1FB70-1FB8B added, sextants never added.
[10] Nerd Fonts issues #1301, #1674. https://github.com/ryanoasis/nerd-fonts/issues/1674 - Legacy Computing block not patched.
[11] WezTerm issue #5051. https://github.com/wezterm/wezterm/issues/5051 - FiraCode Nerd Font yields no sextant glyph, falls back to another font.
[12] Zed issue #56342. https://github.com/zed-industries/zed/issues/56342 - U+1FB00 tofu without font coverage; few monospace/Nerd fonts cover it.
[13] Are We Legacy Computing Yet? https://arewelegacycomputingyet.com/ - terminals self-draw; per-terminal coverage.
[14] kitty box_drawing.py. https://github.com/kovidgoyal/kitty/blob/28b84a2d/kitty/fonts/box_drawing.py - sextant() fills 2x3 grid.
[15] ghostty symbols_for_legacy_computing.zig. https://github.com/ghostty-org/ghostty/blob/65901966/src/font/sprite/draw/symbols_for_legacy_computing.zig - draw1FB00_1FB3B procedural fill.
[16] GNU Unifont. https://www.unifoundry.com/unifont/ - full block coverage in Plane 1.
[17] Cascadia Code issue #607 / PR #708. https://github.com/microsoft/cascadia-code/issues/607 - sextants added 2024.
[18] KreativeKorp Fairfax / Kreative Square. https://www.kreativekorp.com/software/fonts/fairfax/ - full sextant coverage.
[19] notcurses TERMINALS.md. https://github.com/dankamongmen/notcurses/blob/master/TERMINALS.md - per-terminal "Blocks" built-in vs font column.
[20] foot CHANGELOG. https://codeberg.org/dnkl/foot/src/branch/master/CHANGELOG.md - 1.7.0 built-in box drawing, 1.7.2 opt-out.
[21] kitty legacy-computing report. https://github.com/TyOverby/arewelegacycomputingyet.com/blob/main/terminal-emulators/reports/kitty.md - 4x supersampled, font-independent.
[22] wezterm custom_block_glyphs docs. https://wezterm.org/config/lua/config/custom_block_glyphs.html - default on since 20210814.
[23] wezterm customglyph.rs. https://github.com/wez/wezterm/blob/main/wezterm-gui/src/customglyph.rs - SEXTANT_PATTERNS over 0x1fb00..=0x1fb3b.
[24] Alacritty PR #7534. https://github.com/alacritty/alacritty/pull/7534 - sextants, milestone 0.14.0, gap fix.
[25] Alacritty issue #7422. https://github.com/alacritty/alacritty/issues/7422 - 1px seam vs half-blocks.
[26] iTerm2 box-drawing factory. https://github.com/gnachman/iTerm2/blob/ea21d790/sources/iTermBoxDrawingBezierCurveFactory.m - iTermBlockSextant fill.
[27] iTerm2 GitLab #11577. https://gitlab.com/gnachman/iterm2/-/issues/11577 - 3.5 vertical-gap regression.
[28] VTE minifont.cc. https://github.com/GNOME/vte/blob/master/src/minifont.cc - sextant() cairo fill, rows sum to full height.
[29] VTE enable commit e1162ba. https://mail.gnome.org/archives/commits-list/2020-February/msg08625.html - enables U+1FB00.. (Mar 2020).
[30] Konsole TerminalPainter.cpp. https://github.com/KDE/konsole/blob/master/src/terminalDisplay/TerminalPainter.cpp - LineBlockCharacters QPainter draw.
[31] Contour BoxDrawingRenderer.cpp. https://github.com/contour-terminal/contour/blob/master/src/vtrasterizer/BoxDrawingRenderer.cpp - blockSextant() 2x3 fill.
[32] Rio changelog / PR #1089. https://rioterm.com/changelog - sextants in 0.3.0, needs use-drawable-chars.
[33] Are We Legacy Computing Yet (method + scores). https://github.com/TyOverby/arewelegacycomputingyet.com - glyph-less font method; ghostty/kitty/vte/xterm.js 100%, urxvt 0%.
[34] Windows Terminal PR #19841 / issue #18498. https://github.com/microsoft/terminal/issues/18498 - sextants from font, inter-cell background bleed, line-height not applied.
[35] termprobe. https://docs.rs/crate/termprobe/latest - host-side glyph rendering undetectable; font-file probe + allow-list.
[36] ucs-detect. https://ucs-detect.readthedocs.io/intro.html - CSI 6n measures width against wcwidth, not glyph presence.
[37] Cursor Position Report - Terminfo.dev. https://terminfo.dev/cursor/dsr-6-cursor-position-report - CPR spec; headless returns no reply.
[38] Jeff Quast, terminal width correction tables. https://www.jeffquast.com/post/perfecting-terminal-character-width-using-correction-tables/ - per-identity tables; multiplexers excluded.
[39] notcurses_capabilities(3). https://man.archlinux.org/man/notcurses_capabilities.3.en - cansextant heuristic, Unicode 13, UTF-8 gate.
[40] notcurses USAGE.md. https://github.com/dankamongmen/notcurses/blob/master/USAGE.md - notcurses_cansextants requires Unicode 13.
[41] notcurses TERMINALS.md (identification). https://github.com/dankamongmen/notcurses/blob/master/TERMINALS.md - XTVERSION, DA1/2/3, XTGETTCAP TN, TERM binding.
[42] notcurses TERMINALS.md (font caveat). https://github.com/dankamongmen/notcurses/blob/master/TERMINALS.md - PuTTY/Cascadia font example, notcurses-info eyeball tool.
[43] chafa(1) man page. https://man.archlinux.org/man/chafa.1.en - --symbols classes incl. sextant/legacy; default block+border+space; no glyph auto-detect.
[44] ChafaSymbolMap reference. https://hpjansson.org/chafa/ref/chafa-ChafaSymbolMap.html - SEXTANT/LEGACY tags, candidate-per-cell selection.
[45] NcBlitter (Rust docs). https://docs.rs/notcurses/latest/notcurses/sys/enum.NcBlitter.html - blitter tiers, degradation chain, ranking Pixel>Sextant>Quadrant>Half.
[46] notcurses_visual(3). https://notcurses.com/notcurses_visual.3.html - blitter/scale enums, NODEGRADE flag, degradation ladder.
[47] notcurses blitset.h. https://notcurses.com/html/blitset_8h_source.html - rgba_blitter_default resolution ladder.
[48] dapple, One Canvas Seven Renderers. https://queelius.github.io/dapple/blog/one-canvas-seven-renderers/ - sextants better vertical resolution, cells taller than wide.
[49] dapple, Character Encodings. https://queelius.github.io/dapple/guide/encodings/ - one fg + one bg per cell, more sub-cells means more colour error.
[50] consoleimage braille docs. https://github.com/scottgal/mostlylucid.consoleimage/blob/master/docs/BRAILLE-RENDERING.md - 2x4 8-dot, monochrome, colour bolt-on.
[51] dapple README. https://github.com/queelius/dapple - renderer comparison, braille for structure vs blocks for tone.
</content>
</invoke>
