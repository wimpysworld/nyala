package capture

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"strings"

	"github.com/charmbracelet/x/ansi/kitty"
	gosixel "github.com/mattn/go-sixel"
)

// dcsIntro opens a Sixel Device Control String: ESC P, then the q final byte
// selecting the Sixel graphics function. sixel.Renderer emits this prefix.
const dcsIntro = "\x1bPq"

// apcIntro opens a Kitty graphics Application Programming Command: ESC _ G.
// st is the String Terminator, ESC backslash, that closes both envelopes.
const (
	apcIntro = "\x1b_G"
	st       = "\x1b\\"
)

// DecodeSixel decodes a complete Sixel DCS payload back to an *image.RGBA. It
// accepts the framed bytes sixel.Renderer writes (the ESC P q introducer, the
// encoder body, and the ESC backslash terminator) and reverses them with
// mattn/go-sixel. The decoder reconstructs the encoder palette, so the result
// reproduces the quantised palette the encoder chose, not the full-range input.
// The returned image is in RGBA channel order.
func DecodeSixel(payload []byte) (*image.RGBA, error) {
	if !bytes.HasPrefix(payload, []byte(dcsIntro)) {
		return nil, errors.New("capture: sixel payload missing DCS introducer ESC P q")
	}
	var img image.Image
	if err := gosixel.NewDecoder(bytes.NewReader(payload)).Decode(&img); err != nil {
		return nil, fmt.Errorf("capture: decode sixel: %w", err)
	}
	if img == nil {
		return nil, errors.New("capture: sixel payload decoded to no image")
	}
	return toRGBA(img), nil
}

// DecodeKitty decodes a complete Kitty graphics APC payload back to an
// *image.RGBA. It accepts the framed bytes kitty.Renderer writes (the ESC _ G
// introducer, the comma-separated options, a semicolon, the base64 payload, and
// the ESC backslash terminator). It parses the options to learn the format,
// the zlib compression, and the pixel dimensions, base64-decodes the body, then
// reverses the payload with charmbracelet/x/ansi/kitty plus stdlib. Driving the
// decoder from the transmitted options makes the round trip measure render
// error, not a decoder assumption. The returned image is in RGBA channel order.
func DecodeKitty(payload []byte) (*image.RGBA, error) {
	s := string(payload)
	if !strings.HasPrefix(s, apcIntro) || !strings.HasSuffix(s, st) {
		return nil, errors.New("capture: kitty payload missing APC envelope ESC _ G ... ESC backslash")
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(s, apcIntro), st)
	semi := strings.IndexByte(inner, ';')
	if semi < 0 {
		return nil, errors.New("capture: kitty payload missing options/payload separator")
	}
	optsText, b64 := inner[:semi], inner[semi+1:]

	var opts kitty.Options
	if err := opts.UnmarshalText([]byte(optsText)); err != nil {
		return nil, fmt.Errorf("capture: parse kitty options: %w", err)
	}

	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("capture: decode kitty base64: %w", err)
	}

	format := opts.Format
	if format == 0 {
		format = kitty.RGBA
	}
	dec := kitty.Decoder{
		Decompress: opts.Compression == kitty.Zlib,
		Format:     format,
		Width:      opts.ImageWidth,
		Height:     opts.ImageHeight,
	}
	img, err := dec.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("capture: decode kitty image: %w", err)
	}
	return toRGBA(img), nil
}

// toRGBA returns img as an *image.RGBA. If img already is one it is returned
// unchanged; otherwise its pixels are copied into a fresh RGBA buffer so the
// caller always sees RGBA channel order.
func toRGBA(img image.Image) *image.RGBA {
	if rgba, ok := img.(*image.RGBA); ok {
		return rgba
	}
	b := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), img, b.Min, draw.Src)
	return out
}
