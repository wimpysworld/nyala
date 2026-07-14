package viewer

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"strings"
	"testing"

	"github.com/wimpysworld/nyala/internal/capture"
	"github.com/wimpysworld/nyala/internal/render"
)

// kittyCommands splits a kitty render's bytes into its APC command option
// strings, one per ESC _ G ... ESC backslash envelope, dropping the payload
// after the first ';'. It lets a test inspect the transmit and delete commands a
// frame emits in order, so the double-buffer swap is asserted on the wire bytes.
func kittyCommands(out string) []string {
	var cmds []string
	rest := out
	for {
		i := strings.Index(rest, "\x1b_G")
		if i < 0 {
			break
		}
		rest = rest[i+3:]
		j := strings.Index(rest, "\x1b\\")
		if j < 0 {
			break
		}
		body := rest[:j]
		if k := strings.IndexByte(body, ';'); k >= 0 {
			body = body[:k]
		}
		cmds = append(cmds, body)
		rest = rest[j+2:]
	}
	return cmds
}

// hasOpt reports whether a comma-separated kitty option string carries the exact
// key=value token, so a test matches "a=T" without a substring false positive.
func hasOpt(cmd, opt string) bool {
	for _, o := range strings.Split(cmd, ",") {
		if o == opt {
			return true
		}
	}
	return false
}

// solidImageSize builds a w x h solid RGBA image, so a test can vary the frame
// size and assert the transmit reports the new dimensions across a resize.
func solidImageSize(w, h int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i] = c.R
		img.Pix[i+1] = c.G
		img.Pix[i+2] = c.B
		img.Pix[i+3] = c.A
	}
	return img
}

// renderKittyFrame draws one kitty frame to a fresh buffer and returns its APC
// commands, so each frame's bytes are inspected in isolation.
func renderKittyFrame(t *testing.T, s *State, img *image.RGBA) []string {
	t.Helper()
	var buf bytes.Buffer
	if err := s.RenderStreamFrame(render.Frame{Img: img}, &buf); err != nil {
		t.Fatalf("render kitty frame: %v", err)
	}
	return kittyCommands(buf.String())
}

func encodedPNG(t *testing.T, img image.Image) string {
	t.Helper()
	var raw bytes.Buffer
	if err := png.Encode(&raw, img); err != nil {
		t.Fatalf("encode PNG: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw.Bytes())
}

// assertTransmit fails unless cmd is a transmit-and-place (a=T) under the given
// image id.
func assertTransmit(t *testing.T, cmd string, id string) {
	t.Helper()
	if !hasOpt(cmd, "a=T") {
		t.Fatalf("command %q is not a transmit-and-place (a=T)", cmd)
	}
	if !hasOpt(cmd, "i="+id) {
		t.Fatalf("transmit %q missing image id i=%s", cmd, id)
	}
}

// assertDelete fails unless cmd deletes the given image id and frees its data
// (a=d,d=I,i=<id>).
func assertDelete(t *testing.T, cmd string, id string) {
	t.Helper()
	if !hasOpt(cmd, "a=d") {
		t.Fatalf("command %q is not a delete (a=d)", cmd)
	}
	if !hasOpt(cmd, "d=I") {
		t.Fatalf("delete %q does not free data (want d=I)", cmd)
	}
	if !hasOpt(cmd, "i="+id) {
		t.Fatalf("delete %q missing image id i=%s", cmd, id)
	}
}

// TestKittyDoubleBufferAlternatesIDs asserts the kitty live path double-buffers
// the image id: frame 1 transmits under id 1 with no delete, frame 2 transmits
// under id 2 then deletes id 1, and frame 3 transmits under id 1 then deletes
// id 2, so the placement always lands before the old image is freed and no frame
// blanks the screen.
func TestKittyDoubleBufferAlternatesIDs(t *testing.T) {
	s := NewState(8, 4, 16, 16)
	s.Backend = BackendKitty
	img := solidImage(color.RGBA{R: 0x40, G: 0x80, B: 0xc0, A: 0xff})

	f1 := renderKittyFrame(t, &s, img)
	if len(f1) != 1 {
		t.Fatalf("frame 1 emitted %d commands, want 1 (transmit only)", len(f1))
	}
	assertTransmit(t, f1[0], "1")

	f2 := renderKittyFrame(t, &s, img)
	if len(f2) != 2 {
		t.Fatalf("frame 2 emitted %d commands, want 2 (transmit then delete)", len(f2))
	}
	assertTransmit(t, f2[0], "2")
	assertDelete(t, f2[1], "1")

	f3 := renderKittyFrame(t, &s, img)
	if len(f3) != 2 {
		t.Fatalf("frame 3 emitted %d commands, want 2 (transmit then delete)", len(f3))
	}
	assertTransmit(t, f3[0], "1")
	assertDelete(t, f3[1], "2")
}

// TestKittyDoubleBufferResize asserts a resize does not break the swap: the next
// frame still alternates the id and carries the new image dimensions, so the
// double-buffer survives a live resize.
func TestKittyDoubleBufferResize(t *testing.T) {
	s := NewState(8, 4, 16, 16)
	s.Backend = BackendKitty

	f1 := renderKittyFrame(t, &s, solidImage(color.RGBA{A: 0xff}))
	assertTransmit(t, f1[0], "1")

	s.Resize(16, 8)
	f2 := renderKittyFrame(t, &s, solidImageSize(32, 24, color.RGBA{G: 0xff, A: 0xff}))
	if len(f2) != 2 {
		t.Fatalf("post-resize frame emitted %d commands, want 2", len(f2))
	}
	assertTransmit(t, f2[0], "2")
	assertDelete(t, f2[1], "1")
	if !hasOpt(f2[0], "s=32") || !hasOpt(f2[0], "v=24") {
		t.Fatalf("post-resize transmit %q missing new dimensions s=32,v=24", f2[0])
	}
}

func TestKittyDirectPNGUsesRawPayloadAndIDs(t *testing.T) {
	s := NewState(8, 4, 16, 16)
	s.Backend = BackendKitty
	data := encodedPNG(t, solidImageSize(8, 8, color.RGBA{R: 0x31, G: 0x72, B: 0xb4, A: 0xff}))

	var first bytes.Buffer
	if err := s.RenderStreamFrame(render.Frame{EncodedPNG: data}, &first); err != nil {
		t.Fatalf("render direct PNG: %v", err)
	}
	cmds := kittyCommands(first.String())
	if len(cmds) != 1 {
		t.Fatalf("first direct PNG emitted %d commands, want 1", len(cmds))
	}
	for _, opt := range []string{"f=100", "t=d", "a=T", "q=2", "i=1"} {
		if !hasOpt(cmds[0], opt) {
			t.Errorf("direct PNG command %q missing %s", cmds[0], opt)
		}
	}
	_, body, ok := strings.Cut(strings.TrimSuffix(strings.TrimPrefix(first.String(), "\x1b_G"), "\x1b\\"), ";")
	if !ok || body != data {
		t.Fatal("direct PNG terminal body differs from retained CDP payload")
	}

	s.Resize(16, 8)
	var second bytes.Buffer
	if err := s.RenderStreamFrame(render.Frame{EncodedPNG: data}, &second); err != nil {
		t.Fatalf("render direct PNG after resize: %v", err)
	}
	cmds = kittyCommands(second.String())
	if len(cmds) != 2 {
		t.Fatalf("second direct PNG emitted %d commands, want transmit and delete", len(cmds))
	}
	assertTransmit(t, cmds[0], "2")
	assertDelete(t, cmds[1], "1")
}

func TestRetainedPNGFallsBackAfterBackendSwitch(t *testing.T) {
	s := NewState(8, 4, 16, 16)
	s.Backend = BackendSixel
	data := encodedPNG(t, solidImageSize(8, 8, color.RGBA{G: 0xff, A: 0xff}))
	var out bytes.Buffer
	if err := s.RenderStreamFrame(render.Frame{EncodedPNG: data}, &out); err != nil {
		t.Fatalf("render retained PNG through Sixel: %v", err)
	}
	if !strings.HasPrefix(out.String(), "\x1bPq") || !strings.HasSuffix(out.String(), "\x1b\\") {
		t.Fatalf("fallback output is not a framed Sixel image")
	}
}

func TestRetainedPNGBlockFallbackMatchesDecodedPath(t *testing.T) {
	img := solidImageSize(16, 16, color.RGBA{R: 0x21, G: 0x84, B: 0xc6, A: 0xff})
	data := encodedPNG(t, img)
	rawState := NewState(8, 4, 16, 16)
	rawState.Backend = BackendBlock
	var rawOut bytes.Buffer
	if err := rawState.RenderStreamFrame(render.Frame{EncodedPNG: data}, &rawOut); err != nil {
		t.Fatalf("render retained PNG through Block: %v", err)
	}

	decodedState := NewState(8, 4, 16, 16)
	decodedState.Backend = BackendBlock
	var decodedOut bytes.Buffer
	if err := decodedState.RenderStreamFrame(render.Frame{Img: img}, &decodedOut); err != nil {
		t.Fatalf("render decoded PNG through Block: %v", err)
	}
	if !bytes.Equal(rawOut.Bytes(), decodedOut.Bytes()) {
		t.Fatal("retained PNG Block fallback differs from decoded pixel path")
	}
}

func TestInvalidDirectPNGFallsBackToDecodedPixels(t *testing.T) {
	s := NewState(8, 4, 16, 16)
	s.Backend = BackendKitty
	img := solidImageSize(8, 8, color.RGBA{R: 0xff, A: 0xff})
	var out bytes.Buffer
	if err := s.RenderStreamFrame(render.Frame{Img: img, EncodedPNG: "not-a-png"}, &out); err != nil {
		t.Fatalf("render fallback pixels: %v", err)
	}
	cmds := kittyCommands(out.String())
	if len(cmds) != 1 || !hasOpt(cmds[0], "f=24") {
		t.Fatalf("fallback command = %v, want decoded f=24 transmit", cmds)
	}
}

func TestNativeJPEGGraphicsOutputMatchesRGBAPath(t *testing.T) {
	yc := image.NewYCbCr(image.Rect(0, 0, 16, 16), image.YCbCrSubsampleRatio420)
	for i := range yc.Y {
		yc.Y[i] = byte(i*17 + 31)
	}
	for i := range yc.Cb {
		yc.Cb[i] = byte(i*11 + 73)
		yc.Cr[i] = byte(i*7 + 149)
	}
	rgba := image.NewRGBA(yc.Bounds())
	draw.Draw(rgba, rgba.Bounds(), yc, yc.Bounds().Min, draw.Src)

	for _, backend := range []Backend{BackendKitty, BackendSixel} {
		nativeState := NewState(8, 4, 16, 16)
		nativeState.Backend = backend
		var native bytes.Buffer
		if err := nativeState.RenderStreamFrame(render.Frame{Img: yc}, &native); err != nil {
			t.Fatalf("backend %d native render: %v", backend, err)
		}

		rgbaState := NewState(8, 4, 16, 16)
		rgbaState.Backend = backend
		var prior bytes.Buffer
		if err := rgbaState.RenderStreamFrame(render.Frame{Img: rgba}, &prior); err != nil {
			t.Fatalf("backend %d RGBA render: %v", backend, err)
		}
		if backend == BackendKitty {
			if !bytes.Equal(native.Bytes(), prior.Bytes()) {
				t.Errorf("Kitty native JPEG output differs from prior RGBA path")
			}
			continue
		}
		nativeImg, err := capture.DecodeSixel(native.Bytes())
		if err != nil {
			t.Fatalf("decode native Sixel: %v", err)
		}
		priorImg, err := capture.DecodeSixel(prior.Bytes())
		if err != nil {
			t.Fatalf("decode prior Sixel: %v", err)
		}
		for y := nativeImg.Bounds().Min.Y; y < nativeImg.Bounds().Max.Y; y++ {
			for x := nativeImg.Bounds().Min.X; x < nativeImg.Bounds().Max.X; x++ {
				if nativeImg.RGBAAt(x, y) != priorImg.RGBAAt(x, y) {
					t.Fatalf("Sixel native JPEG pixel (%d,%d) differs from prior RGBA path", x, y)
				}
			}
		}
	}
}

// TestKittyResetDeletesBothIDs asserts resetKitty frees both double-buffer image
// ids and clears the tracked id, so a return to the kitty backend rebuilds from
// id 1 with no delete. The stream session calls it when Tab cycles away from
// kitty.
func TestKittyResetDeletesBothIDs(t *testing.T) {
	s := NewState(8, 4, 16, 16)
	s.Backend = BackendKitty
	renderKittyFrame(t, &s, solidImage(color.RGBA{A: 0xff})) // places id 1

	var buf bytes.Buffer
	if err := s.resetKitty(&buf); err != nil {
		t.Fatalf("resetKitty: %v", err)
	}
	cmds := kittyCommands(buf.String())
	if len(cmds) != 2 {
		t.Fatalf("reset emitted %d commands, want 2 (delete both ids)", len(cmds))
	}
	assertDelete(t, cmds[0], "1")
	assertDelete(t, cmds[1], "2")
	if s.kittyID != 0 {
		t.Fatalf("reset left kittyID = %d, want 0", s.kittyID)
	}

	// A rebuild after reset starts fresh at id 1 with no delete.
	rebuilt := renderKittyFrame(t, &s, solidImage(color.RGBA{A: 0xff}))
	if len(rebuilt) != 1 {
		t.Fatalf("post-reset frame emitted %d commands, want 1 (transmit only)", len(rebuilt))
	}
	assertTransmit(t, rebuilt[0], "1")
}

// TestKittyResetNoImageIsNoOp asserts resetKitty writes nothing when no kitty
// image has been placed, so a Tab away from a never-drawn kitty backend is silent.
func TestKittyResetNoImageIsNoOp(t *testing.T) {
	s := NewState(8, 4, 16, 16)
	var buf bytes.Buffer
	if err := s.resetKitty(&buf); err != nil {
		t.Fatalf("resetKitty: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("resetKitty wrote %d bytes with no image placed, want 0", buf.Len())
	}
}
