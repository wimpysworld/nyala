# Terminal compatibility

Block shows Nyala's default auto-selected glyph mode; `--blocks` can override it.
`Yes` under Sixel or Kitty means the terminal implements the protocol, or for
Alacritty that the protocol passed in the current test environment. It does not
mean every Nyala path was live-tested. Notes record incomplete compatibility.

| Terminal | Block | Sixel | Kitty | Notes |
|---|---|---|---|---|
| Alacritty | Sextant | Yes | No | Sixel works in the current test environment, but stock upstream support differs. Built-in sextants can show a one-pixel seam. |
| Contour | Sextant | Yes | Yes | Kitty graphics support is version-dependent and requires Contour 0.7 or newer. Sixel was live-tested. |
| Foot | Sextant | Yes | No | Nyala omits DEC 2026 for Foot Sixel because Foot 1.27 can cancel pending refreshes. DECSDM remains enabled. Live-tested on Foot 1.27.0 under Hyprland 0.55.4. |
| Ghostty | Sextant | No | Yes | Native Kitty graphics; no Sixel support. |
| Kitty | Sextant | No | Yes | Native protocol. Nyala uses the POSIX shared-memory fast path where available, with direct PNG as the fallback. |
| mlterm | Quadrant | Yes | No | Sixel was live-tested. Sextants depend on font coverage, so auto mode uses quadrants. |
| Rio | Sextant | Yes | Yes | Both protocols exist upstream. Nyala's Sixel rendering is currently incorrect and under investigation. |
| WezTerm | Sextant | Yes | Yes | Sixel is documented as preliminary and experimental. Nyala's Kitty placement works. |
