# Block modes

Nyala supports two schemes for the Block backend. The `--blocks` flag accepts `auto`, `sextant`, or `quadrant`.

| Scheme | Flag value | Source pixels per cell | Selection |
| --- | --- | --- | --- |
| Quadrant | `quadrant` | 2x4, reduced to a 2x2 mask | Explicit choice and fallback |
| Sextant | `sextant` | 2x3 | Used when the existing sextant check passes, otherwise Quadrant |

`auto` uses Sextant when the existing sextant check passes. It uses Quadrant otherwise. The interactive Block-mode cycle contains Quadrant and Sextant. An experimental Shaded scheme existed and was rejected on A/B evidence; `docs/ARCHITECTURE.md` records the findings.

## Colour modes

The `--color` flag accepts `auto`, `truecolor`, `256`, or `grey`. These modes affect only the Block backend.

Colour mode precedence is an explicit non-`auto` `--color` value, then `NO_COLOR` for `auto`, then detection. Detection treats `COLORTERM=kmscon`, `COLORTERM=truecolor`, or `COLORTERM=24bit` as `truecolor`, even without a TTY. Every other profile maps to `256`.

| Mode | Output | `auto` selection |
| --- | --- | --- |
| `truecolor` | Exact 24-bit foreground and background colours | A truecolour profile or recognised truecolour `COLORTERM` value |
| `256` | Adaptive scene-fitted indexes 16..255 by default when palette control is available, otherwise canonical xterm indexes 16..255 | A 256-colour profile |
| `grey` | Adaptive scene-fitted indexes 16..255 by default when palette control is available, otherwise fixed xterm greyscale indexes 232..255 | `NO_COLOR` |

Automatic selection maps every profile below truecolour to `256`, the floor mode: every remaining colour mode builds on 256-colour support, and every modern terminal renders indexed colour. A present `NO_COLOR` key selects `grey`; greyscale output still emits SGR colour codes. On a real TTY the stored capability is floored at `256`. A non-TTY keeps its detected capability.

<kbd>Alt</kbd>+<kbd>C</kbd> cycles `truecolor` -> `256` -> `grey`, then wraps. The cycle skips modes above the effective capability. An explicit mode asserts support for that mode and can raise a lower detected capability. Terminal safety restrictions remain hard caps. `NO_COLOR` selects the automatic starting mode only, so the cycle can still use detected colour modes.

The `256` mode maps each RGB colour to the CIEDE2000-nearest canonical xterm entry in indexes 16..255. The `grey` mode maps the Rec.601 key `77R + 150G + 29B` to the nearest grey in its active ramp. On the fixed xterm ramp in indexes 232..255, the reference levels are 8, 18, through 238 in steps of 10, and an exact midpoint selects the lighter grey.

The default palette policy is adaptive. The `256` and `grey` modes use it when terminal palette control is available. <kbd>Alt</kbd>+<kbd>P</kbd> switches between adaptive and static palettes only while Block is active in either mode. The key does nothing without palette control, under another backend, or in `truecolor` mode. The static policy is a session fallback for output comparison or a terminal path where palette updates are slow. The selection stays set through <kbd>Alt</kbd>+<kbd>C</kbd>, <kbd>Alt</kbd>+<kbd>B</kbd>, <kbd>Alt</kbd>+<kbd>R</kbd>, reloads, and resizes. A new Nyala process starts adaptive again.

The handshake probe sends `OSC 4 ; 16 ; ?`. Support is the probe's verdict, never a `TERM` heuristic. On a terminal that did not answer, the `256` mode keeps the canonical xterm palette and the `grey` mode keeps the fixed xterm greyscale ramp. On a terminal that answered, adaptive `256` quantises each Block frame to a 240-entry palette fitted to that frame, and adaptive `grey` fits a greyscale ramp to the page. Both modes write the palette into the terminal. Indexes 0..15 stay the user's theme. The scene-fitted entries live in indexes 16..255. A palette change writes the palette as a series of short OSC 4 sequences of at most seven entries each, paired with a full repaint, so the cells and the palette always describe the same frame. The sequences are short because terminal parsers cap the number of OSC parameters they accept: Alacritty and Rio stop at sixteen and then drop an oversized sequence entirely, so a single batched write sets no register at all on those terminals. A frame that reuses the palette writes no palette bytes. A completed re-fit also writes no palette bytes when its active length, emitted entries, and complete LUT are unchanged.

The scene-fitted greyscale ramp holds every luma level that the page uses when the page uses 240 or fewer. Above that, the fit merges the closest-populated neighbours down to 240 entries. A frame reuses the ramp while the mean squared luma error stays within the reuse threshold, so small changes write no palette bytes. On a re-fit, a register that moves by two luma levels or fewer keeps its old value, which damps flicker. On the browser-free harness, the fitted ramp scores higher structural similarity and lower colour error than the fixed ramp on greyscale, text, and dark gradient pages, at a small extra compose cost per frame with no allocations.

The reduced multiplexer handshake sends no OSC 4 query, so tmux, zellij, GNU screen, and herdr sessions record unsupported and keep the fixed palettes. GNU screen drops OSC 4. The Linux console has 16 palette entries, so it keeps the fixed palettes too. Nyala writes bare `OSC 104` to restore the terminal palette when <kbd>Alt</kbd>+<kbd>P</kbd> selects static, on exit, on every backend switch, and on a colour cycle to another mode, including a cycle between `256` and `grey`. WezTerm freezes its configured colours from the first OSC 4 until that reset, so the restore is unconditional rather than WezTerm-specific.

## NO_COLOR

With `--color=auto`, a present `NO_COLOR` key selects `grey` output regardless of its value. Any explicit non-auto `--color` value overrides `NO_COLOR`. Greyscale output still emits SGR colour codes. No colourless mode exists. `NO_COLOR` changes the starting mode only, not the default palette policy. On a terminal that answered the startup probe, `grey` starts with its scene-fitted ramp. <kbd>Alt</kbd>+<kbd>P</kbd> can select the static ramp for the session. Kitty and Sixel ignore the colour mode.
