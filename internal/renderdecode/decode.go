// Package renderdecode contains correctness-only decoders for terminal graphics
// protocol output.
package renderdecode

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"os"
	"strings"

	"github.com/charmbracelet/x/ansi/kitty"
	gosixel "github.com/mattn/go-sixel"
)

// dcsPrefix opens a Device Control String. Sixel parameters may follow before
// the q final byte selects the Sixel graphics function.
const dcsPrefix = "\x1bP"

// apcIntro opens a Kitty graphics Application Programming Command: ESC _ G.
// st is the String Terminator, ESC backslash, that closes both envelopes.
const (
	apcIntro = "\x1b_G"
	st       = "\x1b\\"
)

// DecodeSixel decodes a complete Sixel DCS payload back to an *image.RGBA. It
// accepts the framed bytes sixel.Renderer writes (ESC P, optional parameter
// bytes, the q final byte, the encoder body, and ESC backslash) and reverses
// them with mattn/go-sixel. The decoder reconstructs the encoder palette, so
// the result reproduces the quantised palette the encoder chose, not the
// full-range input. The returned image is in RGBA channel order.
func DecodeSixel(payload []byte) (*image.RGBA, error) {
	if !hasSixelIntroducer(payload) {
		return nil, errors.New("renderdecode: sixel payload missing DCS introducer ESC P ... q")
	}
	var img image.Image
	if err := gosixel.NewDecoder(bytes.NewReader(payload)).Decode(&img); err != nil {
		return nil, fmt.Errorf("renderdecode: decode sixel: %w", err)
	}
	if img == nil {
		return nil, errors.New("renderdecode: sixel payload decoded to no image")
	}
	return toRGBA(img), nil
}

func hasSixelIntroducer(payload []byte) bool {
	if !bytes.HasPrefix(payload, []byte(dcsPrefix)) {
		return false
	}
	for _, b := range payload[len(dcsPrefix):] {
		if b == 'q' {
			return true
		}
		if b < 0x30 || b > 0x3f {
			return false
		}
	}
	return false
}

// DecodeKitty decodes a complete Kitty graphics APC payload back to an
// *image.RGBA. It accepts the framed bytes kitty.Renderer writes (the ESC _ G
// introducer, the comma-separated options, a semicolon, the base64 payload, and
// the ESC backslash terminator). A large image arrives as several consecutive
// APC frames carrying the m= chunk key; DecodeKitty reassembles them. It parses
// the first frame's options to learn the format, the zlib compression, and the
// pixel dimensions, concatenates the base64 bodies, base64-decodes the result,
// then reverses the payload with charmbracelet/x/ansi/kitty plus stdlib.
// A shared-memory frame (t=s) carries the POSIX object name as its payload;
// the pixel bytes are read from that named /dev/shm object instead. Driving
// the decoder from the transmitted options makes the round trip measure
// render error, not a decoder assumption. The returned image is in RGBA channel
// order.
func DecodeKitty(payload []byte) (*image.RGBA, error) {
	optsText, b64, err := reassembleKittyFrames(string(payload))
	if err != nil {
		return nil, err
	}

	var opts kitty.Options
	if err := opts.UnmarshalText([]byte(optsText)); err != nil {
		return nil, fmt.Errorf("renderdecode: parse kitty options: %w", err)
	}

	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("renderdecode: decode kitty base64: %w", err)
	}

	if opts.Transmission == kitty.SharedMemory {
		raw, err = readSHMPayload(string(raw))
		if err != nil {
			return nil, err
		}
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
		return nil, fmt.Errorf("renderdecode: decode kitty image: %w", err)
	}
	return toRGBA(img), nil
}

// readSHMPayload reads a t=s frame's pixel payload from the named POSIX
// shared-memory object, the leading-slash form the escape carries (for example
// /nyala-1234-7). It is a decoder concern only: it fetches the bytes the
// escape references so the round trip can verify them, and it neither unlinks
// the object nor composes anything. A name that is empty or escapes the flat
// /dev/shm namespace is rejected.
func readSHMPayload(name string) ([]byte, error) {
	trimmed := strings.TrimPrefix(name, "/")
	if trimmed == "" || strings.Contains(trimmed, "/") || strings.Contains(trimmed, "..") {
		return nil, fmt.Errorf("renderdecode: invalid shm object name %q", name)
	}
	raw, err := os.ReadFile("/dev/shm/" + trimmed)
	if err != nil {
		return nil, fmt.Errorf("renderdecode: read shm object %q: %w", name, err)
	}
	return raw, nil
}

// reassembleKittyFrames scans s for consecutive Kitty graphics frames and
// returns the first frame's control section and the concatenated base64 bodies.
// Each frame runs from the ESC _ G introducer to the ESC backslash terminator,
// with the control section before the first ';' and the body after it. A frame
// whose control carries m=1 has more frames following; reassembly stops at the
// first frame without m=1, including a lone frame with no m= key (the
// small-image path), so the single-frame envelope decodes as a single frame.
func reassembleKittyFrames(s string) (optsText, b64 string, err error) {
	var first string
	var bodies strings.Builder
	seen := false
	for len(s) > 0 {
		if !strings.HasPrefix(s, apcIntro) {
			return "", "", errors.New("renderdecode: kitty payload missing APC envelope ESC _ G ... ESC backslash")
		}
		end := strings.Index(s, st)
		if end < 0 {
			return "", "", errors.New("renderdecode: kitty payload missing String Terminator ESC backslash")
		}
		inner := s[len(apcIntro):end]
		s = s[end+len(st):]

		semi := strings.IndexByte(inner, ';')
		if semi < 0 {
			return "", "", errors.New("renderdecode: kitty payload missing options/payload separator")
		}
		frameControl, frameBody := inner[:semi], inner[semi+1:]
		if !seen {
			first = frameControl
			seen = true
		}
		bodies.WriteString(frameBody)

		if !frameHasMore(frameControl) {
			break
		}
	}
	if !seen {
		return "", "", errors.New("renderdecode: kitty payload missing APC envelope ESC _ G ... ESC backslash")
	}
	return first, bodies.String(), nil
}

// frameHasMore reports whether a Kitty frame's control section marks more
// frames following: true only when a comma-separated field equals exactly m=1.
// A field m=0 or no m= field means this frame is terminal. The exact key match
// avoids a substring scan matching m= inside an unrelated field value.
func frameHasMore(control string) bool {
	for _, field := range strings.Split(control, ",") {
		if field == "m=1" {
			return true
		}
	}
	return false
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
