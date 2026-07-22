package kitty

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"image/png"
	"io"
	"strings"
	"testing"

	xkitty "github.com/charmbracelet/x/ansi/kitty"

	"github.com/wimpysworld/nyala/internal/renderdecode"
)

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
	if err := e.EncodePNGID(data, 7, 80, 24, &out); err != nil {
		t.Fatalf("EncodePNGID() = %v", err)
	}
	frames := splitAPCFrames(t, out.String())
	if len(frames) < 3 {
		t.Fatalf("PNG emitted %d chunks, want at least 3", len(frames))
	}
	for _, opt := range []string{"f=100", "t=d", "a=T", "q=2", "C=1", "c=80", "r=24", "i=7", "m=1"} {
		if !hasControlOpt(frames[0].control, opt) {
			t.Errorf("first control %q missing %s", frames[0].control, opt)
		}
	}
	if hasControlOpt(frames[0].control, "z=-1073741825") {
		t.Fatal("ordinary live encode unexpectedly carries the FPS z-index")
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

	var gridless bytes.Buffer
	if err := e.EncodePNGID(data, 7, 0, 0, &gridless); err != nil {
		t.Fatalf("EncodePNGID() gridless = %v", err)
	}
	gridlessFrames := splitAPCFrames(t, gridless.String())
	for _, opt := range []string{"c=0", "r=0"} {
		if hasControlOpt(gridlessFrames[0].control, opt) {
			t.Fatalf("gridless control %q carries %s, want the grid keys omitted", gridlessFrames[0].control, opt)
		}
	}

	var layered bytes.Buffer
	if err := e.EncodePNGIDAtZ(data, 7, -1073741825, 80, 24, &layered); err != nil {
		t.Fatalf("EncodePNGIDAtZ() = %v", err)
	}
	layeredFrames := splitAPCFrames(t, layered.String())
	if !hasControlOpt(layeredFrames[0].control, "z=-1073741825") {
		t.Fatalf("first layered control %q missing negative z-index", layeredFrames[0].control)
	}
	for _, opt := range []string{"c=80", "r=24"} {
		if !hasControlOpt(layeredFrames[0].control, opt) {
			t.Fatalf("first layered control %q missing %s", layeredFrames[0].control, opt)
		}
	}
	var layeredPayload strings.Builder
	for i, frame := range layeredFrames {
		if i > 0 && strings.Contains(frame.control, "z=") {
			t.Fatalf("continuation control %d carries z-index: %q", i, frame.control)
		}
		layeredPayload.WriteString(frame.body)
	}
	if layeredPayload.String() != data {
		t.Fatal("layered chunked PNG payload differs from the supplied base64 bytes")
	}
}

// TestLiveEncoderSHMRaw checks the raw shared-memory path emits exactly one
// unchunked f=32 t=s escape carrying the base64 object name, with the caller's
// dimensions, image id, optional z-index, and quiet mode, and never an m= key.
func TestLiveEncoderSHMRaw(t *testing.T) {
	const name = "/nyala-99-7"
	payload := base64.StdEncoding.EncodeToString([]byte(name))

	var e LiveEncoder
	cases := []struct {
		label  string
		encode func(w *bytes.Buffer) error
		want   string
	}{
		{
			label:  "q=2",
			encode: func(w *bytes.Buffer) error { return e.EncodeSHMID(name, 128, 72, 7, 2, 80, 24, w) },
			want:   apcIntro + "f=32,t=s,s=128,v=72,a=T,C=1,c=80,r=24,i=7,q=2;" + payload + st,
		},
		{
			label:  "q=1",
			encode: func(w *bytes.Buffer) error { return e.EncodeSHMID(name, 128, 72, 7, 1, 80, 24, w) },
			want:   apcIntro + "f=32,t=s,s=128,v=72,a=T,C=1,c=80,r=24,i=7,q=1;" + payload + st,
		},
		{
			label:  "gridless omits c and r",
			encode: func(w *bytes.Buffer) error { return e.EncodeSHMID(name, 128, 72, 7, 2, 0, 0, w) },
			want:   apcIntro + "f=32,t=s,s=128,v=72,a=T,C=1,i=7,q=2;" + payload + st,
		},
		{
			label: "z and q=2",
			encode: func(w *bytes.Buffer) error {
				return e.EncodeSHMIDAtZ(name, 128, 72, 7, -1073741825, 2, 80, 24, w)
			},
			want: apcIntro + "f=32,t=s,s=128,v=72,a=T,C=1,c=80,r=24,i=7,z=-1073741825,q=2;" + payload + st,
		},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			var out bytes.Buffer
			if err := tc.encode(&out); err != nil {
				t.Fatalf("encode shm frame: %v", err)
			}
			if out.String() != tc.want {
				t.Fatalf("escape = %q, want %q", out.String(), tc.want)
			}
			frames := splitAPCFrames(t, out.String())
			if len(frames) != 1 {
				t.Fatalf("shm path emitted %d escapes, want 1", len(frames))
			}
			for opt := range strings.SplitSeq(frames[0].control, ",") {
				if strings.HasPrefix(opt, "m=") {
					t.Fatalf("shm control %q carries chunking key %q", frames[0].control, opt)
				}
			}
		})
	}
}

// TestLiveEncoderSHMRawAllocs checks the shm encode reuses its frame buffer
// and allocates nothing per call at steady state.
func TestLiveEncoderSHMRawAllocs(t *testing.T) {
	const name = "/nyala-99-8"
	var e LiveEncoder
	allocs := testing.AllocsPerRun(100, func() {
		if err := e.EncodeSHMID(name, 2560, 1440, 2, 1, 190, 45, io.Discard); err != nil {
			t.Fatalf("EncodeSHMID() = %v", err)
		}
	})
	if allocs != 0 {
		t.Fatalf("EncodeSHMID allocated %.1f objects per run, want 0", allocs)
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
			err := e.EncodePNGID(data, 1, 80, 24, &out)
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
