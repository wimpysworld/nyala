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
	"time"

	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/renderdecode"
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
	if err := s.RenderStreamFrame(render.Frame{EncodedPNG: encodedPNG(t, img)}, &buf); err != nil {
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
	if !hasOpt(cmd, "C=1") {
		t.Fatalf("transmit %q moves the cursor, want C=1", cmd)
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
	s := NewState(8, 4)
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
	s := NewState(8, 4)
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
}

func TestKittyDirectPNGUsesRawPayloadAndIDs(t *testing.T) {
	s := NewState(8, 4)
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
	for _, opt := range []string{"f=100", "t=d", "a=T", "q=2", "C=1", "i=1"} {
		if !hasOpt(cmds[0], opt) {
			t.Errorf("direct PNG command %q missing %s", cmds[0], opt)
		}
	}
	if strings.Contains(cmds[0], "z=") {
		t.Fatalf("non-FPS direct PNG command %q carries a z-index", cmds[0])
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

// TestFPSKittyFrameOrder checks the FPS-enabled live path transmits the page at
// the text-safe z-index, deletes the old image, paints the absolute-positioned
// badge, and only then closes the synchronised update. The retained PNG body
// remains byte-for-byte unchanged.
func TestFPSKittyFrameOrder(t *testing.T) {
	st := NewState(8, 4)
	st.Backend = BackendKitty
	diff := &block.GridDiff{}
	st.SetDiff(diff)
	s := &streamSession{
		st:   &st,
		diff: diff,
		fps:  &fpsMeter{window: fpsWindow, start: time.Now()},
	}
	data := encodedPNG(t, solidImageSize(8, 8, color.RGBA{R: 0x31, G: 0x72, B: 0xb4, A: 0xff}))
	frame := render.Frame{EncodedPNG: data}

	var first bytes.Buffer
	if _, err := s.assemble(&first, frame); err != nil {
		t.Fatalf("assemble first FPS Kitty frame: %v", err)
	}
	var second bytes.Buffer
	if _, err := s.assemble(&second, frame); err != nil {
		t.Fatalf("assemble second FPS Kitty frame: %v", err)
	}
	out := second.String()
	transmit := strings.Index(out, "\x1b_Gf=100")
	deleteOld := strings.Index(out, "a=d")
	badge := strings.Index(out, "\x1b[1;1H\x1b[0;7m")
	syncClose := strings.LastIndex(out, endSyncUpdate)
	if transmit < 0 || deleteOld < 0 || badge < 0 || syncClose < 0 {
		t.Fatalf("FPS Kitty output lacks an ordered element: transmit=%d delete=%d badge=%d sync-close=%d", transmit, deleteOld, badge, syncClose)
	}
	if transmit >= deleteOld || deleteOld >= badge || badge >= syncClose {
		t.Fatalf("FPS Kitty order transmit=%d delete=%d badge=%d sync-close=%d", transmit, deleteOld, badge, syncClose)
	}
	commands := kittyCommands(out)
	if len(commands) != 2 {
		t.Fatalf("second FPS Kitty frame emitted %d commands, want transmit and delete", len(commands))
	}
	if !hasOpt(commands[0], "z=-1073741825") {
		t.Fatalf("FPS Kitty transmit %q missing negative z-index", commands[0])
	}
	if strings.Contains(out, "\x1b7") || strings.Contains(out, "\x1b8") {
		t.Fatal("FPS Kitty frame consumed a saved-cursor slot")
	}
	if !strings.Contains(out, "\x1b[0m\x1b[1;1H"+endSyncUpdate) {
		t.Fatal("FPS Kitty badge did not return home before synchronised close")
	}
	apc := out[transmit:]
	end := strings.Index(apc, "\x1b\\")
	_, body, ok := strings.Cut(apc[:end], ";")
	if !ok || body != data {
		t.Fatal("FPS Kitty terminal body differs from retained CDP payload")
	}
}

func TestRetainedPNGFallsBackAfterBackendSwitch(t *testing.T) {
	s := NewState(8, 4)
	s.Backend = BackendSixel
	img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
	colours := []color.NRGBA{
		{R: 0xff, A: 0xff},
		{G: 0xff, A: 0xff},
		{B: 0xff, A: 0xff},
		{R: 0xff, G: 0xff, B: 0xff, A: 0xff},
	}
	for y := range 8 {
		for x := range 8 {
			img.SetNRGBA(x, y, colours[(x+y)%len(colours)])
		}
	}
	img.Pix[3] = 0xfe // Keep an alpha channel in the encoded PNG.
	data := encodedPNG(t, img)
	decoded, err := decodeFramePNG(data)
	if err != nil {
		t.Fatalf("decode retained PNG: %v", err)
	}
	if _, ok := decoded.(*image.NRGBA); !ok {
		t.Fatalf("decoded retained PNG = %T, want *image.NRGBA", decoded)
	}
	var out bytes.Buffer
	if err := s.RenderStreamFrame(render.Frame{EncodedPNG: data}, &out); err != nil {
		t.Fatalf("render retained PNG through Sixel: %v", err)
	}
	roundTrip, err := renderdecode.DecodeSixel(out.Bytes())
	if err != nil {
		t.Fatalf("decode fallback Sixel: %v", err)
	}
	if roundTrip.Bounds() != img.Bounds() {
		t.Fatalf("decoded bounds = %v, want %v", roundTrip.Bounds(), img.Bounds())
	}
	for y := range 8 {
		for x := range 8 {
			want, got := img.NRGBAAt(x, y), roundTrip.RGBAAt(x, y)
			if got.R != want.R || got.G != want.G || got.B != want.B {
				t.Fatalf("decoded pixel (%d,%d) = %#v, want RGB(%d,%d,%d)", x, y, got, want.R, want.G, want.B)
			}
		}
	}
}

func TestRetainedPNGBlockFallbackMatchesDecodedPath(t *testing.T) {
	img := solidImageSize(16, 16, color.RGBA{R: 0x21, G: 0x84, B: 0xc6, A: 0xff})
	data := encodedPNG(t, img)
	rawState := NewState(8, 4)
	rawState.Backend = BackendBlock
	rawState.SetDiff(&block.GridDiff{})
	var rawOut bytes.Buffer
	if err := rawState.RenderStreamFrame(render.Frame{EncodedPNG: data}, &rawOut); err != nil {
		t.Fatalf("render retained PNG through Block: %v", err)
	}

	decodedState := NewState(8, 4)
	decodedState.Backend = BackendBlock
	decodedState.SetDiff(&block.GridDiff{})
	var decodedOut bytes.Buffer
	if err := decodedState.RenderStreamFrame(render.Frame{Img: img}, &decodedOut); err != nil {
		t.Fatalf("render decoded PNG through Block: %v", err)
	}
	if !bytes.Equal(rawOut.Bytes(), decodedOut.Bytes()) {
		t.Fatal("retained PNG Block fallback differs from decoded pixel path")
	}
}

func TestInvalidDirectPNGRejectedBeforeOutput(t *testing.T) {
	img := solidImageSize(8, 8, color.RGBA{R: 0xff, A: 0xff})
	valid := encodedPNG(t, img)
	raw, err := base64.StdEncoding.DecodeString(valid)
	if err != nil {
		t.Fatalf("decode test PNG: %v", err)
	}
	cases := map[string]string{
		"invalid prefix":   "not-a-png",
		"invalid length":   valid + "A",
		"invalid alphabet": valid[:len(valid)/2] + "!" + valid[len(valid)/2+1:],
		"invalid padding":  valid[:16] + "=" + valid[17:],
		"truncated PNG":    base64.StdEncoding.EncodeToString(raw[:len(raw)-12]),
		"invalid structure": base64.StdEncoding.EncodeToString(func() []byte {
			corrupt := bytes.Clone(raw)
			corrupt[11] = 12 // IHDR must carry exactly 13 data bytes.
			return corrupt
		}()),
		"invalid CRC": base64.StdEncoding.EncodeToString(func() []byte {
			corrupt := bytes.Clone(raw)
			corrupt[29] ^= 1 // First chunk CRC follows signature, header, and IHDR data.
			return corrupt
		}()),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			s := NewState(8, 4)
			s.Backend = BackendKitty
			var out bytes.Buffer
			if err := s.RenderStreamFrame(render.Frame{Img: img, EncodedPNG: data}, &out); err == nil {
				t.Fatal("invalid direct PNG was accepted")
			}
			if out.Len() != 0 {
				t.Fatalf("invalid direct PNG wrote %d terminal bytes, want 0", out.Len())
			}
		})
	}
}

func TestNativeJPEGSixelOutputMatchesRGBAPath(t *testing.T) {
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

	nativeState := NewState(8, 4)
	nativeState.Backend = BackendSixel
	var native bytes.Buffer
	if err := nativeState.RenderStreamFrame(render.Frame{Img: yc}, &native); err != nil {
		t.Fatalf("native render: %v", err)
	}

	rgbaState := NewState(8, 4)
	rgbaState.Backend = BackendSixel
	var prior bytes.Buffer
	if err := rgbaState.RenderStreamFrame(render.Frame{Img: rgba}, &prior); err != nil {
		t.Fatalf("RGBA render: %v", err)
	}
	nativeImg, err := renderdecode.DecodeSixel(native.Bytes())
	if err != nil {
		t.Fatalf("decode native Sixel: %v", err)
	}
	priorImg, err := renderdecode.DecodeSixel(prior.Bytes())
	if err != nil {
		t.Fatalf("decode prior Sixel: %v", err)
	}
	if !nativeImg.Bounds().Eq(priorImg.Bounds()) {
		t.Fatalf("Sixel native JPEG bounds = %v, want %v", nativeImg.Bounds(), priorImg.Bounds())
	}
	psnr := metrics.PSNR(priorImg, nativeImg)
	ssim := metrics.SSIM(priorImg, nativeImg)
	if psnr < 30 || ssim < 0.95 {
		t.Fatalf("Sixel native JPEG quality versus RGBA path: PSNR=%f SSIM=%f", psnr, ssim)
	}
}

// TestKittyResetDeletesBothIDs asserts resetKitty frees both double-buffer image
// ids and clears the tracked id, so a return to the kitty backend rebuilds from
// id 1 with no delete. The stream session calls it when Tab cycles away from
// kitty.
func TestKittyResetDeletesBothIDs(t *testing.T) {
	s := NewState(8, 4)
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
	s := NewState(8, 4)
	var buf bytes.Buffer
	if err := s.resetKitty(&buf); err != nil {
		t.Fatalf("resetKitty: %v", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("resetKitty wrote %d bytes with no image placed, want 0", buf.Len())
	}
}
