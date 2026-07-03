package engine

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// fontFamily is the family name the injected @font-face declares and the forced
// font-family rule references. It must match exactly in both places so the page
// resolves to the embedded face rather than a system fallback.
const fontFamily = "FiraCode Nerd Font Mono"

// advanceRatio is the FiraCode Nerd Font Mono glyph advance width divided by its
// em size, measured 1200/1950 from the embedded TTF. The seed font-size is
// cellW/advanceRatio so one glyph advance seeds to one cell width before
// calibration corrects it.
const advanceRatio = 0.6154

// styleElementID is the id given to the injected <style> element so calibration
// can locate it and overwrite its seed font-size with the corrected value.
const styleElementID = "nyala-font-forcing"

// buildStylesheetScript returns the JavaScript that injects the font-forcing
// stylesheet. It embeds font.FiraCodeTTF() as a base64 data: URL @font-face,
// forces every element onto the monospace grid with a seed font-size of
// cellW/advanceRatio and a line-height of cellH, disables kerning, ligatures,
// and letter-spacing, left-aligns text, and de-sticks fixed and sticky
// positioning to absolute. cellW and cellH are the fractional CSS-pixel cell size
// under the layout scale Z, so the seed and line-height keep sub-cell precision.
// It runs via Page.addScriptToEvaluateOnNewDocument, which fires when the new
// document's JS context is created, before documentElement exists. The script
// appends the <style> as soon as documentElement appears: immediately if it is
// already there, otherwise from a MutationObserver on document that appends and
// disconnects the moment the root element lands, so the first layout is forced. It
// applies to the first document and every later one.
func buildStylesheetScript(fontBytes []byte, cellW, cellH float64) string {
	dataURL := "data:font/ttf;base64," + base64.StdEncoding.EncodeToString(fontBytes)
	seedPx := cellW / advanceRatio
	css := fmt.Sprintf(`@font-face {
  font-family: "%s";
  src: url("%s") format("truetype");
}
* {
  font-family: "%s", monospace !important;
  font-size: %gpx !important;
  line-height: %gpx !important;
  font-kerning: none !important;
  letter-spacing: 0 !important;
  font-variant-ligatures: none !important;
  text-align: left !important;
}
*[style*="position: fixed"], *[style*="position:fixed"],
*[style*="position: sticky"], *[style*="position:sticky"] {
  position: absolute !important;
}`, fontFamily, dataURL, fontFamily, seedPx, cellH)

	var b strings.Builder
	b.WriteString("(function(){var s=document.createElement('style');s.id=")
	b.WriteString(jsStringLiteral(styleElementID))
	b.WriteString(";s.textContent=")
	b.WriteString(jsStringLiteral(css))
	b.WriteString(";if(document.documentElement){document.documentElement.appendChild(s);}")
	b.WriteString("else{var o=new MutationObserver(function(){if(document.documentElement){")
	b.WriteString("o.disconnect();document.documentElement.appendChild(s);}});")
	b.WriteString("o.observe(document,{childList:true});}})();")
	return b.String()
}

// buildStyleRemovalScript returns the JavaScript that removes the injected
// font-forcing <style> from the live document if present, so a Tab switch from the
// block backend to a graphics backend drops the forcing without a reload. It looks
// the element up by styleElementID and removes it; a missing element is a no-op, so
// it never throws when no forcing style is on the page.
func buildStyleRemovalScript() string {
	var b strings.Builder
	b.WriteString("(function(){var s=document.getElementById(")
	b.WriteString(jsStringLiteral(styleElementID))
	b.WriteString(");if(s){s.remove();}})();")
	return b.String()
}

// jsStringLiteral returns s as a single-quoted JavaScript string literal with
// backslashes, single quotes, and newlines escaped so the CSS text embeds safely
// in the injection script.
func jsStringLiteral(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`'`, `\'`,
		"\n", `\n`,
		"\r", `\r`,
	)
	return "'" + r.Replace(s) + "'"
}

// calibratedFontSize returns the corrected seed font-size that lands N graphemes
// on N*cellW pixels, given the measured probe box width in CSS pixels. cellW is the
// fractional CSS-pixel cell width under the layout scale Z, so the probe target is
// float64(n) * cellW. Font advance scales linearly with font-size, so the
// correction is seed * (N*cellW) / measuredBoxW. A zero or non-positive measured
// width yields no correction and returns the seed unchanged.
func calibratedFontSize(seed float64, n int, cellW, measuredBoxW float64) float64 {
	if measuredBoxW <= 0 {
		return seed
	}
	return seed * (float64(n) * cellW / measuredBoxW)
}
