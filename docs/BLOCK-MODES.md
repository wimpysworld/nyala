# Block modes

Nyala supports three schemes for the Block backend. The `--blocks` flag accepts `auto`, `sextant`, `quadrant`, or `half`.

| Scheme | Flag value | Source pixels per cell | Selection |
| --- | --- | --- | --- |
| Quadrant | `quadrant` | 2x4, reduced to a 2x2 mask | Explicit choice and fallback |
| Half-block | `half` | 1x2 | Explicit choice |
| Sextant | `sextant` | 2x3 | Used when the existing sextant check passes, otherwise Quadrant |

`auto` uses Sextant when the existing sextant check passes. It uses Quadrant otherwise. The interactive Block-mode cycle contains Half-block, Quadrant, and Sextant.

## Colour modes

The `--color` flag accepts `auto`, `truecolor`, `256`, `ansi-grey`, `ansi-tonal`, or `none`. These modes affect only the Block backend.

Colour mode precedence is an explicit non-`auto` `--color` value, then `NO_COLOR` for `auto`, then detection. Detection treats `COLORTERM=kmscon`, `COLORTERM=truecolor`, or `COLORTERM=24bit` as `truecolor`, even without a TTY. Without one of these values, a non-TTY selects `none`.

| Mode | Output | `auto` selection |
| --- | --- | --- |
| `truecolor` | Exact 24-bit foreground and background colours | A truecolour profile or recognised truecolour `COLORTERM` value |
| `256` | Canonical xterm indexes 16..255 | A 256-colour profile |
| `ansi-grey` | Fixed xterm greyscale indexes 232..255 | Never |
| `ansi-tonal` | Basic ANSI indexes 0..15 | Never |
| `none` | No foreground or background colour sequences | `NO_COLOR`, ANSI, ASCII, unknown, or non-TTY output without a recognised truecolour `COLORTERM` value |

Automatic selection maps a detected ANSI profile to `none`, and it never selects `ansi-tonal` or `ansi-grey` at startup. On a real TTY the stored capability is floored at `256`, so the cycle can raise colour on an ANSI or unknown terminal after user action. A non-TTY keeps its detected capability.

<kbd>Alt</kbd>+<kbd>C</kbd> cycles `truecolor` -> `256` -> `ansi-grey` -> `ansi-tonal` -> `none`, then wraps. The cycle skips modes above the effective capability. From an automatically selected `none` on an ANSI profile, the first forward cycle reaches `256`. This user action is separate from automatic startup. `ansi-grey` remains available through `--color=ansi-grey` or the cycle when the effective capability permits it. An explicit mode asserts support for that mode and can raise a lower detected capability. Terminal safety restrictions remain hard caps. `NO_COLOR` selects the automatic starting mode only, so the cycle can still use detected colour modes.

The `256` mode maps each RGB colour to the CIEDE2000-nearest canonical xterm entry in indexes 16..255. Both degraded modes use the Rec.601 key `77R + 150G + 29B`. The `ansi-grey` mode maps that key to the nearest fixed xterm grey in indexes 232..255. Its reference levels are 8, 18, through 238 in steps of 10. An exact midpoint selects the lighter grey.

The `ansi-tonal` mode maps Rec.601 luma to the nearest canonical xterm ANSI colour. From dark to bright, the reference index order is `0, 4, 12, 1, 5, 2, 9, 6, 13, 3, 8, 10, 14, 7, 11, 15`. An exact tie selects the darker reference entry.

Terminals can redefine ANSI indexes 0..15. The visible `ansi-tonal` order can differ from the reference xterm order. Nyala does not query or replace the user's terminal palette.

## Monochrome output

`--color=none` selects monochrome directly. With `--color=auto`, a present `NO_COLOR` key selects monochrome regardless of its value. Any explicit non-auto `--color` value overrides `NO_COLOR`.

Monochrome keeps each composed Block glyph and suppresses foreground and background colour sequences, so the page renders as a 1-bit silhouette in the terminal's default colours. The crisp text overlay stays. Kitty and Sixel ignore the mode.
