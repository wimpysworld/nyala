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

// forcingExclusions are the replaced media elements the size and line-height
// forcing skips, so media keeps its own intrinsic line box rather than being
// squeezed onto the text cell. These elements carry no child text node the walk
// reads, so withholding the cell size from them never moves an overlay run; it only
// stops the forced line-height from collapsing an image or embed's box. Form
// controls stay forced, because a button label and a select option are text the
// overlay must keep on the grid. The monospace family still applies to every element
// (the * rule), so any text advances uniformly. The list is used as a :not() chain on
// the sizing rule.
var forcingExclusions = []string{"img", "svg", "canvas", "video", "iframe", "embed", "object"}

// buildStylesheetScript returns the JavaScript that injects the font-forcing
// stylesheet. It embeds font.FiraCodeTTF() as a base64 data: URL @font-face,
// forces every element onto the monospace family, disables kerning, ligatures, and
// letter-spacing, and left-aligns text. It sets -webkit-text-fill-color to
// transparent on every element so the page paints no ordinary glyph ink into the
// screencast bitmap; the crisp overlay is then the sole text layer and has no
// painted ghost to sit against. The same * rule zeroes text-shadow and
// -webkit-text-stroke, so no orphan shadow or outline paints once the glyph fill is
// gone. Two follow-on restore rules bring paint back for content the text walk does
// not capture, so it still shows as block graphics rather than vanishing: one
// restores currentColor on ::before, ::after, and ::marker for CSS-generated content
// and list markers; the next restores it on input, textarea, select, option, svg,
// svg *, and ::placeholder for form-control values, placeholders, select options, and
// SVG text labels. A button label stays hidden, because it is a real text node the
// walk captures and the overlay redraws crisply. The restore rules follow the * rule
// so they win at equal specificity. It seeds the font-size to cellW/advanceRatio
// and the line-height to cellH on text-bearing elements only, excluding the replaced
// and form-control elements in forcingExclusions so media and controls keep their own
// line boxes; the uniform monospace family still applies to every element so any text
// advances one cell. cellW and cellH are the fractional CSS-pixel cell size under the
// layout scale Z, so the seed and line-height keep sub-cell precision. It runs via
// Page.addScriptToEvaluateOnNewDocument, which fires when the new document's JS
// context is created, before documentElement exists. The script appends the <style>
// as soon as documentElement appears: immediately if it is already there, otherwise
// from a MutationObserver on document that appends and disconnects the moment the root
// element lands, so the first layout is forced. It applies to the first document and
// every later one. It does not de-stick fixed or sticky positioning: the in-page walk
// takes each run's rect from Range.getClientRects(), which returns the viewport rect a
// fixed element paints at, so a fixed nav lands on its painted row without forcing it
// into flow.
func buildStylesheetScript(fontBytes []byte, cellW, cellH float64) string {
	dataURL := "data:font/ttf;base64," + base64.StdEncoding.EncodeToString(fontBytes)
	seedPx := cellW / advanceRatio
	var not strings.Builder
	for _, sel := range forcingExclusions {
		not.WriteString(":not(")
		not.WriteString(sel)
		not.WriteString(")")
	}
	css := fmt.Sprintf(`@font-face {
  font-family: "%s";
  src: url("%s") format("truetype");
}
* {
  font-family: "%s", monospace !important;
  font-kerning: none !important;
  letter-spacing: 0 !important;
  font-variant-ligatures: none !important;
  text-align: left !important;
  -webkit-text-fill-color: transparent !important;
  text-shadow: none !important;
  -webkit-text-stroke: 0 !important;
}
::before, ::after, ::marker {
  -webkit-text-fill-color: currentColor !important;
}
input, textarea, select, option, svg, svg *, ::placeholder {
  -webkit-text-fill-color: currentColor !important;
}
*%s {
  font-size: %gpx !important;
  line-height: %gpx !important;
}`, fontFamily, dataURL, fontFamily, not.String(), seedPx, cellH)

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
