package kitty

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"image/png"
	"strings"
	"testing"

	xkitty "github.com/charmbracelet/x/ansi/kitty"

	"github.com/wimpysworld/nyala/internal/renderdecode"
)

// TestLiveEncoderRoundTrips checks the LiveEncoder payload decodes back to the
// source RGB pixels through the upstream Kitty decoder, over both the single-frame
// and the chunked path.
func TestLiveEncoderRoundTrips(t *testing.T) {
	cases := []struct {
		name string
		img  *image.RGBA
	}{
		{"small single frame", knownFrame(t).Img.(*image.RGBA)},
		{"large chunked frame", largeFrame(t).Img.(*image.RGBA)},
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
	img := largeFrame(t).Img.(*image.RGBA)
	var e LiveEncoder
	var a, b bytes.Buffer
	if err := e.EncodeID(img, 1, &a); err != nil {
		t.Fatalf("EncodeID() a = %v", err)
	}
	// A differently sized frame in between must not leak into the reused buffers.
	if err := e.EncodeID(knownFrame(t).Img.(*image.RGBA), 2, new(bytes.Buffer)); err != nil {
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

// TestLiveEncoderPNGPassThrough checks the direct PNG path preserves CDP's
// base64 payload byte for byte and emits the required first, middle, and final
// chunk controls with the caller's image id.
func TestLiveEncoderPNGPassThrough(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 128, 128))
	var state uint32 = 1
	for i := 0; i < len(src.Pix); i += 4 {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		binary.LittleEndian.PutUint32(src.Pix[i:i+4], state)
		src.Pix[i+3] = 0xff
	}
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, src); err != nil {
		t.Fatalf("encode source PNG: %v", err)
	}
	data := base64.StdEncoding.EncodeToString(pngBytes.Bytes())
	if len(data) <= xkitty.MaxChunkSize*2 {
		t.Fatalf("test PNG base64 length = %d, want more than two chunks", len(data))
	}

	var e LiveEncoder
	var out bytes.Buffer
	if err := e.EncodePNGID(data, 7, &out); err != nil {
		t.Fatalf("EncodePNGID() = %v", err)
	}
	frames := splitAPCFrames(t, out.String())
	if len(frames) < 3 {
		t.Fatalf("PNG emitted %d chunks, want at least 3", len(frames))
	}
	for _, opt := range []string{"f=100", "t=d", "a=T", "q=2", "i=7", "m=1"} {
		if !hasControlOpt(frames[0].control, opt) {
			t.Errorf("first control %q missing %s", frames[0].control, opt)
		}
	}
	for i := 1; i < len(frames)-1; i++ {
		if frames[i].control != chunkMore {
			t.Errorf("middle control %d = %q, want %q", i, frames[i].control, chunkMore)
		}
	}
	if got := frames[len(frames)-1].control; got != chunkLast {
		t.Errorf("final control = %q, want %q", got, chunkLast)
	}
	var joined strings.Builder
	for _, frame := range frames {
		joined.WriteString(frame.body)
	}
	if joined.String() != data {
		t.Fatal("chunked PNG payload differs from the supplied base64 bytes")
	}
	decoded, err := renderdecode.DecodeKitty(out.Bytes())
	if err != nil {
		t.Fatalf("decode direct PNG Kitty output: %v", err)
	}
	if !decoded.Bounds().Eq(src.Bounds()) {
		t.Fatalf("decoded bounds = %v, want %v", decoded.Bounds(), src.Bounds())
	}
	for y := src.Bounds().Min.Y; y < src.Bounds().Max.Y; y++ {
		for x := src.Bounds().Min.X; x < src.Bounds().Max.X; x++ {
			if decoded.RGBAAt(x, y) != src.RGBAAt(x, y) {
				t.Fatalf("decoded pixel (%d,%d) differs", x, y)
			}
		}
	}
}

func TestLiveEncoderPNGRejectsBeforeWrite(t *testing.T) {
	valid := encodedTestPNG(t)
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
			var e LiveEncoder
			var out bytes.Buffer
			err := e.EncodePNGID(data, 1, &out)
			if !errors.Is(err, ErrInvalidPNGData) {
				t.Fatalf("EncodePNGID() error = %v, want ErrInvalidPNGData", err)
			}
			if out.Len() != 0 {
				t.Fatalf("rejected PNG wrote %d bytes, want 0", out.Len())
			}
		})
	}
}

func encodedTestPNG(t *testing.T) string {
	t.Helper()
	var out bytes.Buffer
	if err := png.Encode(&out, knownFrame(t).Img); err != nil {
		t.Fatalf("encode test PNG: %v", err)
	}
	return base64.StdEncoding.EncodeToString(out.Bytes())
}

type apcFrame struct {
	control string
	body    string
}

func splitAPCFrames(t *testing.T, out string) []apcFrame {
	t.Helper()
	var frames []apcFrame
	for out != "" {
		if !strings.HasPrefix(out, apcIntro) {
			t.Fatalf("payload missing APC introducer: %q", out[:min(len(out), 32)])
		}
		end := strings.Index(out, st)
		if end < 0 {
			t.Fatal("payload missing APC terminator")
		}
		control, body, ok := strings.Cut(out[len(apcIntro):end], ";")
		if !ok {
			t.Fatal("payload missing control separator")
		}
		frames = append(frames, apcFrame{control: control, body: body})
		out = out[end+len(st):]
	}
	return frames
}

func hasControlOpt(control, want string) bool {
	for opt := range strings.SplitSeq(control, ",") {
		if opt == want {
			return true
		}
	}
	return false
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
