package viewer

import "testing"

// allShapes returns a supported set that contains every kitty pointer shape.
func allShapes() map[string]bool {
	supported := make(map[string]bool, len(kittyPointerShapes))
	for name := range kittyPointerShapes {
		supported[name] = true
	}
	return supported
}

func TestMapCursorToShape(t *testing.T) {
	t.Parallel()
	all := allShapes()
	cases := []struct {
		name      string
		css       string
		supported map[string]bool
		want      string
		ok        bool
	}{
		{"identity pointer", "pointer", all, "pointer", true},
		{"identity text", "text", all, "text", true},
		{"identity nwse-resize", "nwse-resize", all, "nwse-resize", true},
		{"identity vertical-text", "vertical-text", all, "vertical-text", true},
		{"list takes first mapped keyword", "url(x), pointer", all, "pointer", true},
		{"list skips unmapped head", "url(a), url(b), grab", all, "grab", true},
		{"auto falls back to default", "auto", all, "default", true},
		{"none falls back to default", "none", all, "default", true},
		{"unknown falls back to default", "spinning", all, "default", true},
		{"unsupported name dropped", "pointer", map[string]bool{"default": true}, "", false},
		{"unsupported default dropped", "auto", map[string]bool{"pointer": true}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := mapCursorToShape(tc.css, tc.supported)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("mapCursorToShape(%q) = (%q, %v), want (%q, %v)", tc.css, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestPointerShapeSequence(t *testing.T) {
	t.Parallel()
	if got, want := pointerShapeSequence("pointer"), "\x1b]22;pointer\x1b\\"; got != want {
		t.Fatalf("pointerShapeSequence(\"pointer\") = %q, want %q", got, want)
	}
	if got := pointerShapeSequence("grab"); got[len(got)-1] == '\a' {
		t.Fatalf("sequence must terminate with ST, not BEL: %q", got)
	}
	if pointerShapeReset != "\x1b]22;\x1b\\" {
		t.Fatalf("pointerShapeReset = %q, want %q", pointerShapeReset, "\x1b]22;\x1b\\")
	}
}
