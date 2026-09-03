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

The `256` colour mode selects its palette technique automatically from the
startup probe's verdict. When the terminal answered `OSC 4 ; 16 ; ?`, the mode
writes a scene-fitted palette into indexes 16..255 with OSC 4 and restores the
terminal palette with bare OSC 104. When the terminal did not answer, the mode
keeps the canonical xterm palette. A `TERM` value, a terminfo entry, and
another terminal's behaviour are not proof that a terminal accepts a written
palette and restores it.

| Terminal | Version | Font path | Multiplexer | Mode | Result |
| --- | --- | --- | --- | --- | --- |

The table is empty because no terminal is recorded yet. The table records
evidence for a behaviour that ships, not a gate on it.

The reduced multiplexer handshake sends no OSC 4 query, so tmux, zellij, GNU
screen, and herdr sessions record unsupported and keep the canonical palette.
GNU screen drops OSC 4. The Linux console has 16 palette entries and keeps the
canonical palette.
