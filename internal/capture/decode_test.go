package capture

import (
	"bytes"
	"image"
	"image/color"
	"testing"

	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/kitty"
	"github.com/wimpysworld/nyala/internal/render/sixel"
)

// frameFixture builds a small two-colour RGBA frame for the round-trip tests.
// It is the input the renderers encode and the decoders must reproduce.
func frameFixture() render.Frame {
	const w, h = 8, 12
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	red := color.RGBA{R: 0xff, G: 0x00, B: 0x00, A: 0xff}
	blue := color.RGBA{R: 0x00, G: 0x00, B: 0xff, A: 0xff}
	for y := range h {
		for x := range w {
			if y < h/2 {
				img.SetRGBA(x, y, red)
			} else {
				img.SetRGBA(x, y, blue)
			}
		}
	}
	return render.Frame{Img: img}
}

// TestDecodeKittyRoundTripMSEZero is the Kitty acceptance proof. The Kitty
// transport is lossless, so encode-then-decode must reproduce the input with
// mean squared error 0. A zero here certifies the decoder is exact, so any
// non-zero MSE a caller later sees is render error, not decoder error.
func TestDecodeKittyRoundTripMSEZero(t *testing.T) {
	f := frameFixture()
	var buf bytes.Buffer
	var r kitty.Renderer
	if err := r.Render(f, &buf); err != nil {
		t.Fatalf("kitty Render: %v", err)
	}

	got, err := DecodeKitty(buf.Bytes())
	if err != nil {
		t.Fatalf("DecodeKitty: %v", err)
	}

	in := f.Img.(*image.RGBA)
	if got.Bounds() != in.Bounds() {
		t.Fatalf("decoded bounds = %v, want %v", got.Bounds(), in.Bounds())
	}
	if mse := mseRGBA(in, got); mse != 0 {
		t.Fatalf("kitty round-trip MSE = %v, want 0 (lossless transport)", mse)
	}
}

// TestDecodeKittyRGBAOrder asserts the decoded image is RGBA in channel order:
// a pure-green input pixel stores G in the second byte, not the first or third.
func TestDecodeKittyRGBAOrder(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	green := color.RGBA{R: 0x00, G: 0x80, B: 0x00, A: 0xff}
	for y := range 2 {
		for x := range 2 {
			img.SetRGBA(x, y, green)
		}
	}
	var buf bytes.Buffer
	var r kitty.Renderer
	if err := r.Render(render.Frame{Img: img}, &buf); err != nil {
		t.Fatalf("kitty Render: %v", err)
	}
	got, err := DecodeKitty(buf.Bytes())
	if err != nil {
		t.Fatalf("DecodeKitty: %v", err)
	}
	i := got.PixOffset(0, 0)
	r0, g0, b0, a0 := got.Pix[i], got.Pix[i+1], got.Pix[i+2], got.Pix[i+3]
	if r0 != 0x00 || g0 != 0x80 || b0 != 0x00 || a0 != 0xff {
		t.Errorf("pixel bytes = R%02X G%02X B%02X A%02X, want R00 G80 B00 AFF", r0, g0, b0, a0)
	}
}

// TestDecodeSixelReproducesPalette is the Sixel acceptance proof. The Sixel
// transport quantises to a palette, so the round trip is a palette identity,
// not a pixel identity: every colour the decoder reports must be one the encoder
// could emit for this two-colour input. The fixture uses pure red and pure blue,
// which survive quantisation exactly, so the decoded palette must be those two
// colours and nothing else.
func TestDecodeSixelReproducesPalette(t *testing.T) {
	f := frameFixture()
	var buf bytes.Buffer
	var r sixel.Renderer
	if err := r.Render(f, &buf); err != nil {
		t.Fatalf("sixel Render: %v", err)
	}

	got, err := DecodeSixel(buf.Bytes())
	if err != nil {
		t.Fatalf("DecodeSixel: %v", err)
	}

	want := paletteOf(f.Img.(*image.RGBA))
	have := paletteOf(got)
	if len(have) == 0 {
		t.Fatal("decoded sixel image has no colours")
	}
	for c := range have {
		if !want[c] {
			t.Errorf("decoded colour %v is not in the encoder input palette %v", c, keys(want))
		}
	}
}

// TestDecodeSixelRGBAOrder asserts the decoded Sixel image is RGBA in channel
// order: a pure-green pixel stores G in the second byte.
func TestDecodeSixelRGBAOrder(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	green := color.RGBA{R: 0x00, G: 0x80, B: 0x00, A: 0xff}
	for y := range 4 {
		for x := range 4 {
			img.SetRGBA(x, y, green)
		}
	}
	var buf bytes.Buffer
	var r sixel.Renderer
	if err := r.Render(render.Frame{Img: img}, &buf); err != nil {
		t.Fatalf("sixel Render: %v", err)
	}
	got, err := DecodeSixel(buf.Bytes())
	if err != nil {
		t.Fatalf("DecodeSixel: %v", err)
	}
	i := got.PixOffset(got.Bounds().Min.X, got.Bounds().Min.Y)
	// The Sixel ramp rounds 0x80 to a near value; assert the green channel
	// dominates the red and blue channels, proving RGBA order, not BGRA.
	r0, g0, b0 := got.Pix[i], got.Pix[i+1], got.Pix[i+2]
	if g0 <= r0 || g0 <= b0 {
		t.Errorf("pixel bytes = R%02X G%02X B%02X, want green channel dominant (RGBA order)", r0, g0, b0)
	}
}

// TestDecodeKittyRejectsMalformed checks the decoder rejects a payload that is
// not framed in the APC envelope.
func TestDecodeKittyRejectsMalformed(t *testing.T) {
	if _, err := DecodeKitty([]byte("not an apc payload")); err == nil {
		t.Error("DecodeKitty accepted a malformed payload, want error")
	}
}

// TestDecodeSixelRejectsMalformed checks the decoder rejects a payload that is
// not framed in the DCS envelope.
func TestDecodeSixelRejectsMalformed(t *testing.T) {
	if _, err := DecodeSixel([]byte("not a dcs payload")); err == nil {
		t.Error("DecodeSixel accepted a malformed payload, want error")
	}
}

func TestHasSixelIntroducerAcceptsParameters(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    bool
	}{
		{name: "legacy", payload: "\x1bPq", want: true},
		{name: "parameters", payload: "\x1bP0;0;8q", want: true},
		{name: "missing DCS", payload: "P0;0;8q", want: false},
		{name: "wrong final", payload: "\x1bP0;0;8x", want: false},
		{name: "invalid parameter byte", payload: "\x1bP0Aq", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasSixelIntroducer([]byte(tc.payload)); got != tc.want {
				t.Fatalf("hasSixelIntroducer(%q) = %t, want %t", tc.payload, got, tc.want)
			}
		})
	}
}

func TestDecodeSixelAcceptsLegacyUnparameterisedDCS(t *testing.T) {
	var payload bytes.Buffer
	var renderer sixel.Renderer
	if err := renderer.Render(frameFixture(), &payload); err != nil {
		t.Fatalf("render parameterised Sixel: %v", err)
	}
	legacy := bytes.Replace(payload.Bytes(), []byte("\x1bP0;0;8q"), []byte("\x1bPq"), 1)
	decoded, err := DecodeSixel(legacy)
	if err != nil {
		t.Fatalf("DecodeSixel legacy DCS: %v", err)
	}
	if got, want := decoded.Bounds(), frameFixture().Img.Bounds(); !got.Eq(want) {
		t.Fatalf("decoded bounds = %v, want %v", got, want)
	}
}

// mseRGBA returns the mean squared error between two equal-bounds RGBA images,
// averaged over every channel of every pixel.
func mseRGBA(a, b *image.RGBA) float64 {
	bounds := a.Bounds()
	var sum float64
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			pa := a.RGBAAt(x, y)
			pb := b.RGBAAt(x, y)
			for _, d := range []int{
				int(pa.R) - int(pb.R),
				int(pa.G) - int(pb.G),
				int(pa.B) - int(pb.B),
				int(pa.A) - int(pb.A),
			} {
				sum += float64(d * d)
			}
		}
	}
	n := float64(bounds.Dx() * bounds.Dy() * 4)
	return sum / n
}

// paletteOf returns the set of distinct opaque RGB colours in img, ignoring the
// alpha channel so a palette comparison is not tripped by alpha rounding.
func paletteOf(img *image.RGBA) map[color.RGBA]bool {
	set := map[color.RGBA]bool{}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.RGBAAt(x, y)
			c.A = 0xff
			set[c] = true
		}
	}
	return set
}

// keys returns the colours of a palette set, for readable test failures.
func keys(set map[color.RGBA]bool) []color.RGBA {
	out := make([]color.RGBA, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	return out
}
