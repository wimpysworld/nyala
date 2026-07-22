//go:build kittyinterop

package viewer

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/wimpysworld/nyala/internal/kittyinterop"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/renderdecode"
)

// TestKittyTerminalRoundTrip sends successive frames through the production
// live viewer, decodes the Kitty APC stream with an independent terminal model,
// and checks framing, chunk continuation, image replacement, and exact pixels.
func TestKittyTerminalRoundTrip(t *testing.T) {
	state := NewState(80, 24)
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

	// The shm transport runs as a subtest so the kitty-interop gate covers
	// both transports under its one entry point.
	t.Run("SHMTransport", testKittySHMTerminalRoundTrip)
}

// testKittySHMTerminalRoundTrip sends successive plane frames through the
// production live viewer with the shm transport resolved, decodes each t=s
// escape by reading the named shared-memory object like a terminal would, and
// checks exact pixels, double-buffer id alternation, plane release, and object
// cleanup. The synthetic terminal side never unlinks, so the reset must.
func testKittySHMTerminalRoundTrip(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("kitty shm transport is Linux-only")
	}
	state := NewState(80, 24)
	state.Backend = BackendKitty
	state.kittyTransport = KittyTransportSHM

	first := kittyInteropFixture(64, 48, 5)
	second := kittyInteropFixture(64, 48, 6)

	released := 0
	var stream bytes.Buffer
	if err := state.RenderStreamFrame(render.Frame{Img: first, Release: func() { released++ }}, &stream); err != nil {
		t.Fatalf("RenderStreamFrame 1: %v", err)
	}
	if released != 1 {
		t.Fatalf("frame 1 released the plane %d times, want 1 (after the copy)", released)
	}
	firstName := kittySHMEscapeName(t, stream.String(), "i=1")
	got, err := renderdecode.DecodeKitty(stream.Bytes())
	if err != nil {
		t.Fatalf("DecodeKitty frame 1: %v", err)
	}
	assertKittyInteropImage(t, got, first)

	stream.Reset()
	if err := state.RenderStreamFrame(render.Frame{Img: second}, &stream); err != nil {
		t.Fatalf("RenderStreamFrame 2: %v", err)
	}
	secondName := kittySHMEscapeName(t, stream.String(), "i=2")
	got, err = renderdecode.DecodeKitty(stream.Bytes())
	if err != nil {
		t.Fatalf("DecodeKitty frame 2: %v", err)
	}
	assertKittyInteropImage(t, got, second)
	cmds := kittyCommands(stream.String())
	if len(cmds) != 2 {
		t.Fatalf("frame 2 emitted %d commands, want 2 (transmit then delete)", len(cmds))
	}
	assertDelete(t, cmds[1], "1")

	var reset bytes.Buffer
	if err := state.resetKitty(&reset); err != nil {
		t.Fatalf("resetKitty: %v", err)
	}
	for _, name := range []string{firstName, secondName} {
		if _, err := os.Stat("/dev/shm/" + strings.TrimPrefix(name, "/")); err == nil {
			t.Fatalf("unconsumed shm object %s outlived resetKitty", name)
		}
	}
}

// kittySHMEscapeName checks the first APC envelope is the single unchunked
// f=32,t=s transmit under the wanted image id and returns the POSIX object
// name its payload carries.
func kittySHMEscapeName(t *testing.T, out, wantID string) string {
	t.Helper()
	rest, ok := strings.CutPrefix(out, "\x1b_G")
	if !ok {
		t.Fatalf("output does not start with a Kitty APC: %q", out)
	}
	body, _, ok := strings.Cut(rest, "\x1b\\")
	if !ok {
		t.Fatal("APC envelope missing its String Terminator")
	}
	control, payload, ok := strings.Cut(body, ";")
	if !ok {
		t.Fatal("APC envelope missing the options/payload separator")
	}
	for _, opt := range []string{"f=32", "t=s", "a=T", "C=1", "c=80", "r=24", "q=0", wantID} {
		found := false
		for _, field := range strings.Split(control, ",") {
			if field == opt {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("shm transmit %q missing %s", control, opt)
		}
	}
	if strings.Contains(control, "m=") {
		t.Fatalf("shm transmit %q is chunked, want a single unchunked escape", control)
	}
	name, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("decode shm object name: %v", err)
	}
	return string(name)
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
	if len(chunks) == 0 || chunks[0].Options["C"] != "1" {
		t.Fatal("Kitty placement can move the cursor, want C=1 on the first chunk")
	}
	// The live viewer scales every placement to the full cell grid, so the first
	// chunk must carry the c=/r= keys for the 80x24 state driving this test.
	if chunks[0].Options["c"] != "80" || chunks[0].Options["r"] != "24" {
		t.Fatalf("first chunk grid keys = c=%q r=%q, want c=80 r=24", chunks[0].Options["c"], chunks[0].Options["r"])
	}
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
