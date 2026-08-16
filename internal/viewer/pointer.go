package viewer

import "strings"

// pointerShapeReset is the OSC 22 sequence with an empty payload, which asks
// the terminal to restore its default pointer shape. The terminator is ST,
// never BEL, matching setTitle.
const pointerShapeReset = "\x1b]22;\x1b\\"

// pointerShapeSequence returns the OSC 22 sequence that sets the terminal
// pointer shape to the named kitty shape. The terminator is ST, never BEL,
// matching setTitle.
func pointerShapeSequence(name string) string {
	return "\x1b]22;" + name + "\x1b\\"
}

// kittyPointerShapes is the set of CSS cursor keywords that map by identity
// onto kitty pointer-shape names. Any keyword outside this set, including
// "auto" and "none", falls back to "default".
var kittyPointerShapes = map[string]bool{
	"alias":         true,
	"cell":          true,
	"copy":          true,
	"crosshair":     true,
	"default":       true,
	"e-resize":      true,
	"ew-resize":     true,
	"grab":          true,
	"grabbing":      true,
	"help":          true,
	"move":          true,
	"n-resize":      true,
	"ne-resize":     true,
	"nesw-resize":   true,
	"no-drop":       true,
	"not-allowed":   true,
	"ns-resize":     true,
	"nw-resize":     true,
	"nwse-resize":   true,
	"pointer":       true,
	"progress":      true,
	"s-resize":      true,
	"se-resize":     true,
	"sw-resize":     true,
	"text":          true,
	"vertical-text": true,
	"w-resize":      true,
	"wait":          true,
	"zoom-in":       true,
	"zoom-out":      true,
}

// mapCursorToShape maps a computed CSS cursor value onto a kitty pointer-shape
// name. A computed list such as "url(x), pointer" takes its first mapped
// keyword, and "auto", "none", and any unmapped value fall back to "default".
// A mapped name absent from supported is dropped with false, never substituted,
// and "default" follows the same rule.
func mapCursorToShape(css string, supported map[string]bool) (string, bool) {
	shape := "default"
	for part := range strings.SplitSeq(css, ",") {
		keyword := strings.ToLower(strings.TrimSpace(part))
		if kittyPointerShapes[keyword] {
			shape = keyword
			break
		}
	}
	if !supported[shape] {
		return "", false
	}
	return shape, true
}
