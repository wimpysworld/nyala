package kitty

import (
	"bytes"
	"encoding/base64"
	"image"
	"strings"
	"testing"

	xkitty "github.com/charmbracelet/x/ansi/kitty"
)

// TestLiveEncoderRoundTrips checks the LiveEncoder payload decodes back to the
// source RGB pixels through the upstream Kitty decoder, over both the single-frame
// and the chunked path.
func TestLiveEncoderRoundTrips(t *testing.T) {
	cases := []struct {
		name string
		img  *image.RGBA
	}{
		{"small single frame", knownFrame(t).Img},
		{"large chunked frame", largeFrame(t).Img},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var e LiveEncoder
			var buf bytes.Buffer
			if err := e.EncodeID(tc.img, 1, &buf); err != nil {
				t.Fatalf("EncodeID() = %v", err)
			}

			out := buf.String()
			if !strings.HasPrefix(out, "\x1b_G") || !strings.HasSuffix(out, "\x1b\\") {
				t.Fatalf("payload not APC framed: prefix/suffix wrong")
			}
			if !strings.Contains(out, "f=24") {
				t.Errorf("payload missing 24-bit RGB format key f=24")
			}
			if !strings.Contains(out, "i=1") {
				t.Errorf("payload missing image id key i=1")
			}

			raw := concatChunkBodies(t, out)
			b := tc.img.Bounds()
			dec := xkitty.Decoder{Decompress: true, Format: xkitty.RGB, Width: b.Dx(), Height: b.Dy()}
			img, err := dec.Decode(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("decode RGB payload: %v", err)
			}
			got, ok := img.(*image.RGBA)
			if !ok {
				t.Fatalf("decoded image is %T, want *image.RGBA", img)
			}
			for y := range b.Dy() {
				for x := range b.Dx() {
					want := tc.img.RGBAAt(x, y)
					have := got.RGBAAt(x, y)
					if have.R != want.R || have.G != want.G || have.B != want.B {
						t.Fatalf("pixel (%d,%d) rgb = %v, want %v", x, y, have, want)
					}
				}
			}
		})
	}
}

// TestLiveEncoderReuseDeterministic checks a reused encoder emits byte-identical
// output for the same frame and id across successive calls, so buffer reuse
// carries no stale state.
func TestLiveEncoderReuseDeterministic(t *testing.T) {
	img := largeFrame(t).Img
	var e LiveEncoder
	var a, b bytes.Buffer
	if err := e.EncodeID(img, 1, &a); err != nil {
		t.Fatalf("EncodeID() a = %v", err)
	}
	// A differently sized frame in between must not leak into the reused buffers.
	if err := e.EncodeID(knownFrame(t).Img, 2, new(bytes.Buffer)); err != nil {
		t.Fatalf("EncodeID() interposed = %v", err)
	}
	if err := e.EncodeID(img, 1, &b); err != nil {
		t.Fatalf("EncodeID() b = %v", err)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatalf("reused encoder output differs across calls on the same frame")
	}
}

// TestLiveEncoderReuseLowAlloc checks a warm LiveEncoder holds its pixel and
// payload scratch across frames: the per-frame allocations stay a small constant
// (only the short option-key strings), not the per-pixel millions the upstream
// encoder made. It guards the buffer reuse against a regression that reallocates
// the pack, zlib, base64, or frame buffer each frame.
func TestLiveEncoderReuseLowAlloc(t *testing.T) {
	img := benchFrame(320, 200)
	var e LiveEncoder
	var buf bytes.Buffer
	// Warm the scratch buffers.
	if err := e.EncodeID(img, 1, &buf); err != nil {
		t.Fatalf("warm EncodeID() = %v", err)
	}
	avg := testing.AllocsPerRun(20, func() {
		buf.Reset()
		if err := e.EncodeID(img, 1, &buf); err != nil {
			t.Fatalf("EncodeID() = %v", err)
		}
	})
	const maxAllocs = 32
	if avg > maxAllocs {
		t.Fatalf("warm EncodeID allocates %.0f times per frame, want <= %d", avg, maxAllocs)
	}
}

// concatChunkBodies extracts and concatenates the base64-decoded bodies of every
// APC frame in out, so a chunked transmission reassembles into the raw payload.
func concatChunkBodies(t *testing.T, out string) []byte {
	t.Helper()
	var b64 strings.Builder
	for frame := range strings.SplitSeq(out, "\x1b_G") {
		if frame == "" {
			continue
		}
		frame = strings.TrimSuffix(frame, "\x1b\\")
		_, body, ok := strings.Cut(frame, ";")
		if !ok {
			continue
		}
		b64.WriteString(body)
	}
	raw, err := base64.StdEncoding.DecodeString(b64.String())
	if err != nil {
		t.Fatalf("payload not valid base64: %v", err)
	}
	return raw
}
