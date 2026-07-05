package engine

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// floatEps is the tolerance for comparing the floating-point font sizes. The
// image maths note bars exact equality; a small epsilon absorbs the residue.
const floatEps = 1e-9

func TestSeedFontSize(t *testing.T) {
	// The seed size is cellW/advanceRatio, the width that lands one glyph advance
	// on one cell before calibration corrects it. Assert both the constant maths
	// and the font-size the emitted script carries for that cell width.
	cases := []struct {
		name  string
		cellW float64
	}{
		{"narrow cell", 6},
		{"typical cell", 9},
		{"wide cell", 12},
		{"fractional cell under Z", 8.0 / 1.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := c.cellW / advanceRatio
			script := buildStylesheetScript([]byte("fake-ttf"), c.cellW, 18)
			got := fontSizeFromScript(t, script)
			if math.Abs(got-want) > floatEps {
				t.Errorf("emitted font-size = %g, want cellW/advanceRatio = %g", got, want)
			}
		})
	}
}

func TestCalibratedFontSizeLandsTarget(t *testing.T) {
	// A proportional measured width must yield the size that lands N*cellW. Font
	// advance scales linearly with font-size, so if a probe at the seed size
	// measures measuredBoxW, the corrected size scaled by the same ratio lands the
	// probe on the target N*cellW. Verify by re-scaling: corrected/seed must equal
	// target/measuredBoxW.
	cases := []struct {
		name        string
		seed        float64
		n           int
		cellW       float64
		measuredBox float64
	}{
		{"probe too wide, correct down", 14.625, 10, 9, 100.0},
		{"probe too narrow, correct up", 14.625, 10, 9, 80.0},
		{"probe on target, no change", 14.625, 10, 9, 90.0},
		// Fractional cellW under Z: cellW = 8/1.5, seed = cellW/advanceRatio, a probe
		// off target still lands N*cellW after correction with no int cast.
		{"fractional cell corrects to target", 8.0 / 1.5 / advanceRatio, 20, 8.0 / 1.5, 100.0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			target := float64(c.n) * c.cellW
			got := calibratedFontSize(c.seed, c.n, c.cellW, c.measuredBox)
			// The corrected size scaled by the measured advance ratio lands the target.
			landed := c.measuredBox * (got / c.seed)
			if math.Abs(landed-target) > floatEps {
				t.Errorf("calibrated %g lands the probe on %g, want target %g", got, landed, target)
			}
			// Direct form: seed * target / measuredBox.
			want := c.seed * target / c.measuredBox
			if math.Abs(got-want) > floatEps {
				t.Errorf("calibratedFontSize = %g, want %g", got, want)
			}
		})
	}
}

func TestCalibratedFontSizeNonPositiveMeasuredWidth(t *testing.T) {
	// A zero or negative measured width gives no probe to correct against, so the
	// function returns the seed unchanged rather than dividing by zero.
	const seed = 14.625
	for _, measured := range []float64{0, -1, -100.5} {
		got := calibratedFontSize(seed, 10, 9, measured)
		if math.Abs(got-seed) > floatEps {
			t.Errorf("calibratedFontSize(seed=%g, measured=%g) = %g, want seed unchanged", seed, measured, got)
		}
	}
}

func TestBuildStylesheetScriptContents(t *testing.T) {
	const cellW, cellH = 9, 18
	script := buildStylesheetScript([]byte("fake-ttf"), cellW, cellH)

	seedPx := float64(cellW) / advanceRatio
	wants := []struct {
		desc     string
		fragment string
	}{
		{"font-face data URL prefix", "data:font/ttf;base64,"},
		{"forced family with !important", fmt.Sprintf(`font-family: "%s", monospace !important`, fontFamily)},
		{"ligatures disabled", "font-variant-ligatures: none !important"},
		{"left-aligned text", "text-align: left !important"},
		{"painted text hidden", "-webkit-text-fill-color: transparent !important"},
		{"orphan text-shadow zeroed", "text-shadow: none !important"},
		{"orphan text-stroke zeroed", "-webkit-text-stroke: 0 !important"},
		{"pseudo-content paint restored", "-webkit-text-fill-color: currentColor !important"},
		{"form and SVG paint restored", "input, textarea, select, option, svg, svg *, ::placeholder {"},
		{"seed font-size from cell width", fmt.Sprintf("font-size: %gpx !important", seedPx)},
		{"line-height from cell height", fmt.Sprintf("line-height: %gpx !important", float64(cellH))},
		{"immediate-append guard on existing root", "if(document.documentElement){document.documentElement.appendChild(s);}"},
		{"MutationObserver for pre-DOM append", "new MutationObserver"},
		{"observer watches document childList", "o.observe(document,{childList:true})"},
		{"observer disconnects once root lands", "o.disconnect()"},
	}
	for _, w := range wants {
		if !strings.Contains(script, w.fragment) {
			t.Errorf("script missing %s: %q not found", w.desc, w.fragment)
		}
	}
}

func TestBuildStylesheetScriptEmbedsFontBytes(t *testing.T) {
	// The @font-face src must carry the font bytes base64-encoded, so a distinct
	// input appears as its own data URL and not a stale or empty one.
	script := buildStylesheetScript([]byte("distinct-font-bytes"), 9, 18)
	// base64 of "distinct-font-bytes"
	const wantB64 = "ZGlzdGluY3QtZm9udC1ieXRlcw=="
	if !strings.Contains(script, "data:font/ttf;base64,"+wantB64) {
		t.Errorf("script does not embed the base64 font bytes %q", wantB64)
	}
}

func TestBuildStyleRemovalScript(t *testing.T) {
	// The removal script must target the injected style by its id, remove it, and be
	// null-safe so a page with no forcing style never throws.
	script := buildStyleRemovalScript()
	wants := []struct {
		desc     string
		fragment string
	}{
		{"targets the style id", jsStringLiteral(styleElementID)},
		{"looks the element up by id", "document.getElementById("},
		{"removes the element", "s.remove();"},
		{"null-safe guard", "if(s){"},
	}
	for _, w := range wants {
		if !strings.Contains(script, w.fragment) {
			t.Errorf("removal script missing %s: %q not found", w.desc, w.fragment)
		}
	}
}

func TestJSStringLiteralEscaping(t *testing.T) {
	// The CSS text embeds in a single-quoted JS literal, so backslashes, single
	// quotes, and newlines must escape or the injection script breaks at parse.
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text", "abc", `'abc'`},
		{"single quote", "a'b", `'a\'b'`},
		{"backslash", `a\b`, `'a\\b'`},
		{"newline", "a\nb", `'a\nb'`},
		{"carriage return", "a\rb", `'a\rb'`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := jsStringLiteral(c.in); got != c.want {
				t.Errorf("jsStringLiteral(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// fontSizeFromScript parses the emitted font-size value in px from the stylesheet
// script. It fails the test if the rule is absent so a format drift is loud.
func fontSizeFromScript(t *testing.T, script string) float64 {
	t.Helper()
	const marker = "font-size: "
	i := strings.Index(script, marker)
	if i < 0 {
		t.Fatalf("script has no font-size rule: %q", script)
	}
	rest := script[i+len(marker):]
	j := strings.Index(rest, "px")
	if j < 0 {
		t.Fatalf("font-size rule has no px unit: %q", rest)
	}
	var v float64
	if _, err := fmt.Sscanf(rest[:j], "%g", &v); err != nil {
		t.Fatalf("parse font-size %q: %v", rest[:j], err)
	}
	return v
}
