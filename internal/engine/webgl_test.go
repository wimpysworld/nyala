package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
)

// TestMapWebGLResult asserts the probe answer to About state mapping: an
// evaluate error or a nil result is unknown, version 0 is unavailable with no
// label, versions 1 and 2 report their tier with the compact vendor label, and
// any other version is unknown because webgl.js never returns it. No case
// depends on the GPU of the test host, because every answer is a fixture.
func TestMapWebGLResult(t *testing.T) {
	cases := []struct {
		name string
		res  *webGLResult
		err  error
		want render.WebGLState
	}{
		{
			name: "nil result",
			want: render.WebGLState{Tier: render.WebGLUnknown},
		},
		{
			name: "evaluate error wins over a result",
			res:  &webGLResult{Version: 2, Vendor: "Intel"},
			err:  errors.New("context deadline exceeded"),
			want: render.WebGLState{Tier: render.WebGLUnknown},
		},
		{
			name: "version 0 is unavailable without a label",
			res:  &webGLResult{Version: 0, Vendor: "Intel", Renderer: "ANGLE (Intel, x, y)"},
			want: render.WebGLState{Tier: render.WebGLUnavailable},
		},
		{
			name: "version 1 with vendor",
			res:  &webGLResult{Version: 1, Vendor: "Intel", Renderer: "Mesa Intel(R) Xe Graphics"},
			want: render.WebGLState{Tier: render.WebGL1, Label: "Intel"},
		},
		{
			name: "version 2 with vendor",
			res:  &webGLResult{Version: 2, Vendor: "NVIDIA Corporation", Renderer: "NVIDIA GeForce RTX 3080/PCIe/SSE2"},
			want: render.WebGLState{Tier: render.WebGL2, Label: "NVIDIA Corporation"},
		},
		{
			name: "version 2 falls back to the ANGLE vendor",
			res:  &webGLResult{Version: 2, Renderer: "ANGLE (Google, Vulkan 1.3.0 (SwiftShader Device (Subzero) (0x0000C0DE)), SwiftShader driver)"},
			want: render.WebGLState{Tier: render.WebGL2, Label: "Google"},
		},
		{
			name: "version 3 is unknown",
			res:  &webGLResult{Version: 3, Vendor: "Intel"},
			want: render.WebGLState{Tier: render.WebGLUnknown},
		},
		{
			name: "negative version is unknown",
			res:  &webGLResult{Version: -1, Vendor: "Intel"},
			want: render.WebGLState{Tier: render.WebGLUnknown},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mapWebGLResult(c.res, c.err); got != c.want {
				t.Fatalf("mapWebGLResult(%+v, %v) = %+v, want %+v", c.res, c.err, got, c.want)
			}
		})
	}
}

// TestCompactWebGLLabel asserts the vendor label About shows: the unmasked
// vendor wins when present, an empty vendor falls back to the first token
// inside the ANGLE wrapper of the renderer, escape sequences are removed
// whole, non-printable runes drop out, whitespace runs collapse to one space,
// and an over-wide label is cut at a whole word or is empty when no whole
// word fits.
func TestCompactWebGLLabel(t *testing.T) {
	cases := []struct {
		name     string
		vendor   string
		renderer string
		want     string
	}{
		{
			name:     "unmasked vendor wins",
			vendor:   "NVIDIA Corporation",
			renderer: "ANGLE (NVIDIA, NVIDIA GeForce RTX 3080, OpenGL 4.6)",
			want:     "NVIDIA Corporation",
		},
		{
			name:     "empty vendor takes the ANGLE vendor",
			renderer: "ANGLE (Intel, Mesa Intel(R) Xe Graphics (TGL GT2), OpenGL 4.6)",
			want:     "Intel",
		},
		{
			name:     "SwiftShader renderer names Google",
			renderer: "ANGLE (Google, Vulkan 1.3.0 (SwiftShader Device (Subzero) (0x0000C0DE)), SwiftShader driver)",
			want:     "Google",
		},
		{
			name:     "whitespace-only vendor falls back to the renderer",
			vendor:   " \t\n",
			renderer: "ANGLE (Apple, Apple M2, OpenGL 4.1 Metal)",
			want:     "Apple",
		},
		{
			name:     "renderer without an ANGLE wrapper gives no label",
			renderer: "Mesa Intel(R) Xe Graphics (TGL GT2)",
			want:     "",
		},
		{
			name: "both empty",
			want: "",
		},
		{
			name:   "control bytes drop out",
			vendor: "Intel\x00\x07\x1b\x7f Corp",
			want:   "Intel Corp",
		},
		{
			name:   "CSI sequence is removed whole",
			vendor: "\x1b[31mIntel\x1b[0m",
			want:   "Intel",
		},
		{
			name:   "OSC sequence is removed whole",
			vendor: "\x1b]8;;https://example.com\x07Intel\x1b]0;title\x1b\\ Corp",
			want:   "Intel Corp",
		},
		{
			name:   "bare ESC takes one following rune",
			vendor: "In\x1bXtel",
			want:   "Intel",
		},
		{
			name:   "unterminated CSI takes the rest of the text",
			vendor: "Intel\x1b[31",
			want:   "Intel",
		},
		{
			name:     "control bytes drop out of the ANGLE vendor",
			renderer: "ANGLE (In\x00tel\r\n, Mesa, OpenGL 4.6)",
			want:     "Intel",
		},
		{
			name:   "space runs collapse",
			vendor: "  Intel   Open  Source  ",
			want:   "Intel Open Source",
		},
		{
			name:   "tab and newline become spaces before the collapse",
			vendor: "Intel\tOpen\r\nSource",
			want:   "Intel Open Source",
		},
		{
			name:   "over-wide label is cut at a whole word",
			vendor: "Advanced Micro Devices Incorporated Graphics",
			want:   "Advanced Micro Devices",
		},
		{
			name:   "label at the width limit is kept whole",
			vendor: strings.Repeat("a", webGLLabelMaxWidth),
			want:   strings.Repeat("a", webGLLabelMaxWidth),
		},
		{
			name:   "over-wide single word is empty",
			vendor: strings.Repeat("a", webGLLabelMaxWidth+1),
			want:   "",
		},
		{
			name:   "width counts cells not runes",
			vendor: strings.Repeat("英", webGLLabelMaxWidth/2+1),
			want:   "",
		},
		{
			name:   "wide runes at the width limit are kept whole",
			vendor: strings.Repeat("英", webGLLabelMaxWidth/2),
			want:   strings.Repeat("英", webGLLabelMaxWidth/2),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := compactWebGLLabel(c.vendor, c.renderer); got != c.want {
				t.Fatalf("compactWebGLLabel(%q, %q) = %q, want %q", c.vendor, c.renderer, got, c.want)
			}
		})
	}
}

// TestWebGLUnknownBeforeSession asserts the engine reports an unknown tier
// with an empty label before any session has run the probe, so About shows
// "?" rather than a claim the engine cannot support.
func TestWebGLUnknownBeforeSession(t *testing.T) {
	e := New("")
	got := e.WebGL()
	if got.Tier != render.WebGLUnknown {
		t.Fatalf("WebGL().Tier = %v before a session, want %v", got.Tier, render.WebGLUnknown)
	}
	if got.Label != "" {
		t.Fatalf("WebGL().Label = %q before a session, want empty", got.Label)
	}
}
