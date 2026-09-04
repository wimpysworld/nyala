# Terminal compatibility evidence

Terminal support is a recorded fact, never an inference. Unicode assignment,
font coverage, and another scheme's allow-list are not proof that one terminal
draws a glyph set at one-cell width with clean alignment. A support claim
needs the terminal, its version, the font path, any multiplexer path, and the
rendering mode it was observed under.

## Sextant scheme

The sextant allow-list lives in `internal/viewer/sextant.go`. It proves only
the U+1FB00..U+1FB3B sextant range on the listed terminals. It is not evidence
for any other glyph range.

## Quadrant scheme

The quadrant scheme draws the U+2596..U+259F quadrant glyphs, the half blocks
U+2580, U+2584, U+258C, U+2590, and the full block U+2588.

| Terminal | Version | Font path | Multiplexer | Mode | Result |
| --- | --- | --- | --- | --- | --- |
| KMSCON | unrecorded | default | none | Quadrant scheme | Renders correctly; owner-tested 2026-09-02 |

## OSC 4 palette support

The default palette policy is adaptive. The `256` and `grey` colour modes use
adaptive palettes when the startup probe confirms palette control.
<kbd>Alt</kbd>+<kbd>P</kbd> selects a static palette for comparison or when
palette updates are slow on the current terminal path. The key works only while
Block is active in `256` or `grey` mode and the probe confirmed support. It does
nothing when one of these conditions is false. The active navigation-bar mode
does not change this gate, so the key works in chrome, URL edit, find, menu, and
About modes. The selection lasts for the current Nyala process.

When the terminal answered `OSC 4 ; 16 ; ?`, adaptive `256` writes a
scene-fitted palette and adaptive `grey` writes a scene-fitted greyscale ramp
into indexes 16..255 with OSC 4. Static `256` uses the canonical xterm palette,
and static `grey` uses the fixed xterm greyscale ramp at indexes 232..255. Both
adaptive modes restore the terminal palette with bare OSC 104. When the terminal
did not answer, Nyala uses the static palettes and the key has no effect. A
`TERM` value, a terminfo entry, and another terminal's behaviour are not proof
that a terminal accepts a written palette and restores it.

| Terminal | Version | Font path | Multiplexer | Mode | Result |
| --- | --- | --- | --- | --- | --- |

The table is empty because no terminal is recorded yet. The table records
evidence for a behaviour that ships, not a gate on it.

The reduced multiplexer handshake sends no OSC 4 query, so tmux, zellij, GNU
screen, and herdr sessions record unsupported and keep the fixed palettes.
GNU screen drops OSC 4. The Linux console has 16 palette entries and keeps the
fixed palettes.
