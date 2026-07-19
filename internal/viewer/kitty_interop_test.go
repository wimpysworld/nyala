//go:build kittyinterop

package viewer

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"testing"

	"github.com/wimpysworld/nyala/internal/kittyinterop"
	"github.com/wimpysworld/nyala/internal/render"
)

// TestKittyTerminalRoundTrip sends successive frames through the production
// live viewer, decodes the Kitty APC stream with an independent terminal model,
// and checks framing, chunk continuation, image replacement, and exact pixels.
func TestKittyTerminalRoundTrip(t *testing.T) {
	state := NewState(80, 24, 16, 32)
	state.Backend = BackendKitty

	small := kittyInteropFixture(8, 6, 1)
	largePNG := kittyInteropFixture(128, 128, 2)
	largeSecondPNG := kittyInteropFixture(160, 120, 3)
	smallPNG := encodeKittyInteropPNG(t, small)
	largePNGBytes := encodeKittyInteropPNG(t, largePNG)
	largeSecondPNGBytes := encodeKittyInteropPNG(t, largeSecondPNG)

	var stream bytes.Buffer
	frames := []render.Frame{
		{EncodedPNG: base64.StdEncoding.EncodeToString(smallPNG)},
		{EncodedPNG: base64.StdEncoding.EncodeToString(largePNGBytes)},
		{EncodedPNG: base64.StdEncoding.EncodeToString(largeSecondPNGBytes)},
	}
	for i, frame := range frames {
		if err := state.RenderStreamFrame(frame, &stream); err != nil {
			t.Fatalf("RenderStreamFrame %d: %v", i+1, err)
		}
	}

	terminal, err := kittyinterop.Decode(stream.Bytes())
	if err != nil {
		t.Fatalf("kittyinterop.Decode: %v", err)
	}
	if len(terminal.Events) != 5 {
		t.Fatalf("decoded %d state changes, want 5", len(terminal.Events))
	}

	transmits := []struct {
		eventIndex int
		id         int
		format     int
		want       *image.RGBA
		pngPayload []byte
		chunked    bool
	}{
		{eventIndex: 0, id: 1, format: 100, want: small, pngPayload: smallPNG},
		{eventIndex: 1, id: 2, format: 100, want: largePNG, pngPayload: largePNGBytes, chunked: true},
		{eventIndex: 3, id: 1, format: 100, want: largeSecondPNG, pngPayload: largeSecondPNGBytes, chunked: true},
	}
	for i, tc := range transmits {
		event := terminal.Events[tc.eventIndex]
		if event.Kind != kittyinterop.EventTransmit || event.ID != tc.id || event.Format != tc.format {
			t.Fatalf("transmit %d = kind %d id %d format %d, want transmit id %d format %d", i+1, event.Kind, event.ID, event.Format, tc.id, tc.format)
		}
		assertKittyInteropImage(t, event.Image, tc.want)
		if tc.pngPayload != nil && !bytes.Equal(event.Payload, tc.pngPayload) {
			t.Fatalf("transmit %d reconstructed PNG differs from the CDP payload", i+1)
		}
		assertKittyInteropChunks(t, event.Chunks, tc.chunked)
	}

	for _, deletion := range []struct {
		eventIndex int
		id         int
	}{{2, 1}, {4, 2}} {
		event := terminal.Events[deletion.eventIndex]
		if event.Kind != kittyinterop.EventDelete || event.ID != deletion.id {
			t.Fatalf("state change %d = kind %d id %d, want delete id %d", deletion.eventIndex, event.Kind, event.ID, deletion.id)
		}
	}
	if terminal.LastPlaced != 1 || len(terminal.Images) != 1 {
		t.Fatalf("final terminal state = last id %d with %d stored images, want id 1 only", terminal.LastPlaced, len(terminal.Images))
	}
	assertKittyInteropImage(t, terminal.Images[1], largeSecondPNG)
}

func kittyInteropFixture(width, height int, seed uint32) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	state := seed
	for i := 0; i < len(img.Pix); i += 4 {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		binary.LittleEndian.PutUint32(img.Pix[i:i+4], state)
		img.Pix[i+3] = 0xff
	}
	return img
}

func encodeKittyInteropPNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatalf("encode PNG fixture: %v", err)
	}
	return out.Bytes()
}

func assertKittyInteropChunks(t *testing.T, chunks []kittyinterop.Chunk, chunked bool) {
	t.Helper()
	if !chunked {
		if len(chunks) != 1 {
			t.Fatalf("single-frame transmission has %d chunks, want 1", len(chunks))
		}
		if _, ok := chunks[0].Options["m"]; ok {
			t.Fatal("single-frame transmission carries a continuation key")
		}
		return
	}
	if len(chunks) < 2 {
		t.Fatalf("chunked transmission has %d chunks, want at least 2", len(chunks))
	}
	for i, chunk := range chunks {
		last := i == len(chunks)-1
		wantMore := "1"
		if last {
			wantMore = "0"
		}
		if chunk.Options["m"] != wantMore {
			t.Fatalf("chunk %d continuation = %q, want %q", i, chunk.Options["m"], wantMore)
		}
		if !last && chunk.PayloadBytes != 4096 {
			t.Fatalf("chunk %d has %d payload bytes, want protocol limit 4096", i, chunk.PayloadBytes)
		}
		if i > 0 && len(chunk.Options) != 1 {
			t.Fatalf("continuation chunk %d carries %d control keys, want only m", i, len(chunk.Options))
		}
	}
}

func assertKittyInteropImage(t *testing.T, got, want *image.RGBA) {
	t.Helper()
	if got == nil {
		t.Fatal("decoded image is nil")
	}
	if !got.Bounds().Eq(want.Bounds()) {
		t.Fatalf("decoded bounds = %v, want %v", got.Bounds(), want.Bounds())
	}
	for y := range want.Bounds().Dy() {
		for x := range want.Bounds().Dx() {
			gotPixel, wantPixel := got.RGBAAt(x, y), want.RGBAAt(x, y)
			if gotPixel != color.RGBA(wantPixel) {
				t.Fatalf("pixel (%d,%d) = %#v, want %#v", x, y, gotPixel, wantPixel)
			}
		}
	}
}
