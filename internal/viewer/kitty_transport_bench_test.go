//go:build kittybench

package viewer

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	stdjpeg "image/jpeg"
	"image/png"
	"math"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	cterm "github.com/charmbracelet/x/term"
	emmb64 "github.com/emmansun/base64"
	"github.com/gen2brain/jpegn"
	"golang.org/x/sys/unix"

	"github.com/wimpysworld/nyala/internal/pngdecode"
	"github.com/wimpysworld/nyala/internal/render/kitty"
)

// The benchmark drives the controlling terminal directly, so it mirrors the
// APC framing bytes the kitty package keeps unexported.
const (
	benchAPCIntro = "\x1b_G"
	benchST       = "\x1b\\"
	benchChunk    = 4096
)

// benchIDA and benchIDB are the two alternating double-buffer image ids. They
// avoid the production ids 1 and 2 and the startup probe ids 31 and 32, so a
// benchmark run never disturbs another Nyala session's images.
const (
	benchIDA = 61
	benchIDB = 62
)

// benchW and benchH size the fixture frame at the 2560x1500 class the live
// screencast produces, so the measured ms/frame match the production payload.
const (
	benchW = 2560
	benchH = 1500
)

// benchAckTimeout bounds the wait for one terminal reply. A terminal that
// never answers an unquieted transmission fails the variant rather than
// hanging the run.
const benchAckTimeout = 10 * time.Second

// benchJPEGQuality is the q90 candidate quality for the shm-mode codec
// decision the codec leg informs.
const benchJPEGQuality = 90

// benchFixture holds the one frame every variant transmits: the PNG bytes,
// their base64 form for the in-band paths, the decoded RGBA pixels with
// their base64 form for the raw paths, and the JPEG q90 bytes with their
// base64 form for the codec-comparison leg.
type benchFixture struct {
	pngBytes  []byte
	pngB64    string
	rawPix    []byte
	rawB64    string
	jpegBytes []byte
	jpegB64   string
}

// buildBenchFixture renders a gradient with pseudo-random noise bands, so the
// PNG compresses to a realistic ~1-2 MB rather than a trivially small file.
// Fixture preparation is not a hot path, so the stdlib PNG and JPEG encoders
// are acceptable here; the decodes use the production internal/pngdecode and
// jpegn and the base64 forms use the production emmb64, so per-frame costs
// match the live path.
func buildBenchFixture() (*benchFixture, error) {
	img := image.NewRGBA(image.Rect(0, 0, benchW, benchH))
	state := uint32(0x9E3779B9)
	for y := range benchH {
		noisy := (y/8)%12 == 0
		for x := range benchW {
			i := img.PixOffset(x, y)
			if noisy {
				state ^= state << 13
				state ^= state >> 17
				state ^= state << 5
				img.Pix[i+0] = byte(state & 0xff)
				img.Pix[i+1] = byte(state >> 8 & 0xff)
				img.Pix[i+2] = byte(state >> 16 & 0xff)
			} else {
				img.Pix[i+0] = byte(x * 255 / benchW)
				img.Pix[i+1] = byte(y * 255 / benchH)
				img.Pix[i+2] = byte((x + y) * 255 / (benchW + benchH))
			}
			img.Pix[i+3] = 0xff
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, fmt.Errorf("encode fixture PNG: %w", err)
	}
	pngBytes := buf.Bytes()
	decoded, err := pngdecode.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		return nil, fmt.Errorf("decode fixture PNG: %w", err)
	}
	rgba := image.NewRGBA(decoded.Bounds())
	draw.Draw(rgba, rgba.Bounds(), decoded, decoded.Bounds().Min, draw.Src)
	var jbuf bytes.Buffer
	if err := stdjpeg.Encode(&jbuf, rgba, &stdjpeg.Options{Quality: benchJPEGQuality}); err != nil {
		return nil, fmt.Errorf("encode fixture JPEG: %w", err)
	}
	jpegBytes := jbuf.Bytes()
	return &benchFixture{
		pngBytes:  pngBytes,
		pngB64:    emmb64.StdEncoding.EncodeToString(pngBytes),
		rawPix:    rgba.Pix,
		rawB64:    emmb64.StdEncoding.EncodeToString(rgba.Pix),
		jpegBytes: jpegBytes,
		jpegB64:   emmb64.StdEncoding.EncodeToString(jpegBytes),
	}, nil
}

// appendBenchFrame mirrors the kitty package's appendStringFrame: one APC
// frame with the given control data and payload chunk appended to dst.
func appendBenchFrame(dst []byte, control, chunk string) []byte {
	dst = append(dst, benchAPCIntro...)
	dst = append(dst, control...)
	dst = append(dst, ';')
	dst = append(dst, chunk...)
	dst = append(dst, benchST...)
	return dst
}

// kittyReplyFor scans buf for a complete APC reply whose control data carries
// i=<id> and returns its response text after the semicolon plus the
// unconsumed remainder of the buffer.
func kittyReplyFor(buf []byte, id string) (reply string, rest []byte, found bool) {
	search := buf
	for {
		i := bytes.Index(search, []byte(benchAPCIntro))
		if i < 0 {
			return "", buf, false
		}
		after := search[i+len(benchAPCIntro):]
		end := bytes.Index(after, []byte(benchST))
		if end < 0 {
			return "", buf, false
		}
		body := after[:end]
		next := after[end+len(benchST):]
		control := body
		var payload []byte
		if sep := bytes.IndexByte(body, ';'); sep >= 0 {
			control = body[:sep]
			payload = body[sep+1:]
		}
		if kittyControlHasID(control, id) {
			return string(payload), next, true
		}
		search = next
	}
}

// benchAckReader accumulates terminal bytes from the tty fd and resolves the
// APC reply for one image id per frame. It polls with unix.Poll rather than
// SetReadDeadline for the same reason as readUntilDA1: deadlines are
// unavailable on tty files. The fd is stored as the int32 unix.PollFd needs,
// range-checked at construction like readUntilDA1.
type benchAckReader struct {
	fd  int32
	buf []byte
}

// await blocks until the terminal answers the transmission for id, and
// returns an error when the reply is not OK or the timeout passes. Bytes that
// are not the awaited reply (key presses, unrelated replies) are skipped.
func (r *benchAckReader) await(id int) error {
	idStr := strconv.Itoa(id)
	deadline := time.Now().Add(benchAckTimeout)
	chunk := make([]byte, 4096)
	for {
		if reply, rest, found := kittyReplyFor(r.buf, idStr); found {
			r.buf = append(r.buf[:0], rest...)
			if reply != "OK" {
				return fmt.Errorf("terminal reply for i=%s: %q", idStr, reply)
			}
			return nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("no terminal reply for i=%s within %s", idStr, benchAckTimeout)
		}
		wait := min(remaining, time.Second)
		fds := []unix.PollFd{{Fd: r.fd, Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(wait.Milliseconds()))
		if err != nil && err != unix.EINTR {
			return fmt.Errorf("poll tty: %w", err)
		}
		if n <= 0 {
			continue
		}
		rn, rerr := unix.Read(int(r.fd), chunk)
		if rn > 0 {
			r.buf = append(r.buf, chunk[:rn]...)
		}
		if rerr != nil && rerr != unix.EINTR {
			return fmt.Errorf("read tty: %w", rerr)
		}
	}
}

// benchVariant is one transport under measurement. send transmits and places
// the fixture frame under the given image id; ack reports whether the timed
// region includes waiting for the terminal's per-frame OK reply.
type benchVariant struct {
	name    string
	payload int
	send    func(id int) error
	ack     bool
}

// benchStats returns mean, median, minimum, and maximum of durs.
func benchStats(durs []time.Duration) (mean, median, minimum, maximum time.Duration) {
	sorted := append([]time.Duration(nil), durs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var sum time.Duration
	for _, d := range sorted {
		sum += d
	}
	mean = sum / time.Duration(len(sorted))
	mid := len(sorted) / 2
	median = sorted[mid]
	if len(sorted)%2 == 0 {
		median = (sorted[mid-1] + sorted[mid]) / 2
	}
	return mean, median, sorted[0], sorted[len(sorted)-1]
}

// benchMS renders a duration as milliseconds for the report line.
func benchMS(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// benchCodecLeg measures the Nyala-side decode-to-plane cost for the two
// screencast codec candidates in shm mode, where the terminal receives raw
// f=32 pixels and the codec choice only changes the engine-side decode. The
// PNG path mirrors the engine's decodeKittyPlane: emmb64 base64 decode into a
// reused scratch buffer, then pngdecode.DecodeInto into a reused RGBA plane.
// The JPEG path has no production equivalent yet, so it is composed here from
// the same production codecs: emmb64 base64 decode, jpegn.Decode, then a
// straightforward YCbCr-to-RGBA conversion loop into the same plane. jpegn
// also offers Options.ToRGBA for direct RGBA output; the manual loop keeps
// the two conversion costs visible separately. This leg is pure CPU and needs
// no terminal.
func benchCodecLeg(t *testing.T, fix *benchFixture, frames int) {
	plane := make([]byte, benchW*benchH*4)
	scratch := make([]byte, emmb64.StdEncoding.DecodedLen(max(len(fix.pngB64), len(fix.jpegB64))))
	reader := bytes.NewReader(nil)
	codecs := []struct {
		name    string
		payload int
		b64     []byte
		decode  func(raw []byte) error
	}{{
		name:    "codec png->plane",
		payload: len(fix.pngBytes),
		b64:     []byte(fix.pngB64),
		decode: func(raw []byte) error {
			reader.Reset(raw)
			_, _, err := pngdecode.DecodeInto(reader, plane)
			return err
		},
	}, {
		name:    "codec jpeg->plane",
		payload: len(fix.jpegBytes),
		b64:     []byte(fix.jpegB64),
		decode: func(raw []byte) error {
			reader.Reset(raw)
			img, err := jpegn.Decode(reader)
			if err != nil {
				return err
			}
			ycc, ok := img.(*image.YCbCr)
			if !ok {
				return fmt.Errorf("jpegn returned %T, want *image.YCbCr", img)
			}
			if b := ycc.Bounds(); b.Dx() != benchW || b.Dy() != benchH {
				return fmt.Errorf("jpegn bounds %v, want %dx%d", b, benchW, benchH)
			}
			for y := range benchH {
				for x := range benchW {
					ci := ycc.COffset(x, y)
					r, g, b := color.YCbCrToRGB(ycc.Y[ycc.YOffset(x, y)], ycc.Cb[ci], ycc.Cr[ci])
					i := (y*benchW + x) * 4
					plane[i+0] = r
					plane[i+1] = g
					plane[i+2] = b
					plane[i+3] = 0xff
				}
			}
			return nil
		},
	}}
	t.Logf("codec fixture %dx%d: PNG %d bytes (%d base64), JPEG q%d %d bytes (%d base64)",
		benchW, benchH, len(fix.pngBytes), len(fix.pngB64),
		benchJPEGQuality, len(fix.jpegBytes), len(fix.jpegB64))
	for _, c := range codecs {
		durs := make([]time.Duration, 0, frames)
		for f := range frames {
			start := time.Now()
			n, err := emmb64.StdEncoding.Decode(scratch, c.b64)
			if err == nil {
				err = c.decode(scratch[:n])
			}
			if err != nil {
				t.Fatalf("%s frame %d: %v", c.name, f+1, err)
			}
			durs = append(durs, time.Since(start))
		}
		mean, median, minimum, maximum := benchStats(durs)
		t.Logf("%-14s payload %6.2f MB  %-9s  mean %8.2f ms  median %8.2f ms  min %8.2f ms  max %8.2f ms",
			c.name, float64(c.payload)/1e6, "decode",
			benchMS(mean), benchMS(median), benchMS(minimum), benchMS(maximum))
	}
}

// TestKittyTransportBenchmark measures per-frame terminal write time for five
// Kitty graphics transports against the live terminal: in-band PNG through
// the production LiveEncoder (t=d), PNG by file path in /dev/shm (t=f), PNG
// through a POSIX shared-memory object (t=s), raw f=32 pixels through shared
// memory, and raw f=32 in-band as the reference ceiling. Before the transport
// variants it runs the pure-CPU codec-comparison leg (benchCodecLeg), which
// needs the env opt-in but no terminal, so a headless run still reports the
// codec rows before the TTY guard skips the transport leg.
//
// Measurement semantics: the in-band t=d variants keep the production q=2 and
// time the blocking write alone, because pty backpressure stalls the write on
// the terminal's own processing and the write time is the production cost
// accounting. The t=f and t=s variants transmit a tiny escape that cannot
// self-pace, so they leave q at its default and time write plus the
// terminal's OK reply, which is when the frame has been consumed. Both
// timings approximate frame-to-consumed and stay comparable.
//
// Guard: the test runs only with NYALA_KITTY_BENCH=1 and an openable
// controlling terminal. The env opt-in stands in for a Kitty graphics probe:
// probeTerminal would add a second raw-mode handshake here and the operator
// already chooses the terminal deliberately through the interactive-only
// `just kitty-transport-bench` recipe. It writes to /dev/tty because `go
// test` pipes the process stdout.
//
// Cleanup: both benchmark image ids are deleted with a=d,d=I after every
// variant, shared-memory objects the terminal did not consume are unlinked by
// SHMWriter.Close, and the /dev/shm payload files for t=f are removed.
func TestKittyTransportBenchmark(t *testing.T) {
	if os.Getenv("NYALA_KITTY_BENCH") != "1" {
		t.Skip("set NYALA_KITTY_BENCH=1 on a Kitty graphics terminal; use `just kitty-transport-bench`")
	}

	frames := 30
	if s := os.Getenv("NYALA_KITTY_BENCH_FRAMES"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			frames = n
		}
	}

	fix, err := buildBenchFixture()
	if err != nil {
		t.Fatalf("build fixture: %v", err)
	}

	benchCodecLeg(t, fix, frames)

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no controlling terminal: %v", err)
	}
	defer tty.Close()
	fd := tty.Fd()
	if !cterm.IsTerminal(fd) {
		t.Skip("/dev/tty is not a terminal")
	}

	writeAPC := func(control, payload string) error {
		if _, err := tty.WriteString(benchAPCIntro + control + ";" + payload + benchST); err != nil {
			return fmt.Errorf("write APC: %w", err)
		}
		return nil
	}

	enc := &kitty.LiveEncoder{}
	variants := []benchVariant{{
		name:    "t=d png",
		payload: len(fix.pngBytes),
		send: func(id int) error {
			return enc.EncodePNGID(fix.pngB64, id, tty)
		},
	}}

	if file, err := os.CreateTemp("/dev/shm", "nyala-bench-*.png"); err != nil {
		t.Logf("skipping t=f variant, /dev/shm unavailable: %v", err)
	} else {
		path := file.Name()
		if err := file.Close(); err != nil {
			t.Fatalf("close t=f payload file: %v", err)
		}
		// Kitty does not delete t=f files (only t=t files), so removal here
		// is the cleanup.
		defer os.Remove(path)
		variants = append(variants, benchVariant{
			name:    "t=f png",
			payload: len(fix.pngBytes),
			ack:     true,
			send: func(id int) error {
				if err := os.WriteFile(path, fix.pngBytes, 0o600); err != nil {
					return err
				}
				control := fmt.Sprintf("f=100,t=f,a=T,C=1,i=%d", id)
				return writeAPC(control, emmb64.StdEncoding.EncodeToString([]byte(path)))
			},
		})
	}

	shmw := kitty.NewSHMWriter()
	defer shmw.Close()
	if _, err := shmw.WriteFrame(benchIDA, []byte{0}); err != nil {
		t.Logf("skipping t=s variants, shared memory unavailable: %v", err)
	} else {
		variants = append(variants, benchVariant{
			name:    "t=s png",
			payload: len(fix.pngBytes),
			ack:     true,
			send: func(id int) error {
				name, err := shmw.WriteFrame(id, fix.pngBytes)
				if err != nil {
					return err
				}
				control := fmt.Sprintf("f=100,t=s,a=T,C=1,i=%d", id)
				return writeAPC(control, emmb64.StdEncoding.EncodeToString([]byte(name)))
			},
		}, benchVariant{
			name:    "t=s raw f=32",
			payload: len(fix.rawPix),
			ack:     true,
			send: func(id int) error {
				name, err := shmw.WriteFrame(id, fix.rawPix)
				if err != nil {
					return err
				}
				control := fmt.Sprintf("f=32,t=s,a=T,C=1,s=%d,v=%d,i=%d", benchW, benchH, id)
				return writeAPC(control, emmb64.StdEncoding.EncodeToString([]byte(name)))
			},
		})
	}

	var rawFrame []byte
	variants = append(variants, benchVariant{
		name:    "t=d raw f=32",
		payload: len(fix.rawPix),
		send: func(id int) error {
			control := fmt.Sprintf("f=32,t=d,a=T,q=2,C=1,s=%d,v=%d,i=%d", benchW, benchH, id)
			data := fix.rawB64
			rawFrame = rawFrame[:0]
			if len(data) <= benchChunk {
				rawFrame = appendBenchFrame(rawFrame, control, data)
			} else {
				first := control + ",m=1"
				for len(data) > benchChunk {
					rawFrame = appendBenchFrame(rawFrame, first, data[:benchChunk])
					data = data[benchChunk:]
					first = "m=1"
				}
				rawFrame = appendBenchFrame(rawFrame, "m=0", data)
			}
			if _, err := tty.Write(rawFrame); err != nil {
				return fmt.Errorf("write raw APC frame: %w", err)
			}
			return nil
		},
	})

	// Raw mode stops the terminal echoing replies to the screen and buffering
	// them behind a newline the replies never carry.
	oldState, err := cterm.MakeRaw(fd)
	if err != nil {
		t.Fatalf("raw mode: %v", err)
	}
	restored := false
	restore := func() {
		if !restored {
			restored = true
			_ = cterm.Restore(fd, oldState)
		}
	}
	defer restore()

	deleteBoth := func() error {
		if _, err := tty.WriteString(kitty.DeleteImage(benchIDA) + kitty.DeleteImage(benchIDB)); err != nil {
			return fmt.Errorf("delete benchmark images: %w", err)
		}
		return nil
	}
	defer deleteBoth() //nolint:errcheck // best-effort repeat of the per-variant cleanup

	ackFD := int(fd)
	if ackFD < 0 || ackFD > math.MaxInt32 {
		t.Fatalf("tty fd %d exceeds the poll fd range", fd)
		return
	}
	acks := &benchAckReader{fd: int32(ackFD)}
	results := make([][]time.Duration, len(variants))
	for vi, v := range variants {
		durs := make([]time.Duration, 0, frames)
		for f := range frames {
			id := benchIDA
			if f%2 == 1 {
				id = benchIDB
			}
			start := time.Now()
			if err := v.send(id); err != nil {
				t.Fatalf("%s frame %d: %v", v.name, f+1, err)
			}
			if v.ack {
				if err := acks.await(id); err != nil {
					t.Fatalf("%s frame %d: %v", v.name, f+1, err)
				}
			}
			durs = append(durs, time.Since(start))
		}
		if err := deleteBoth(); err != nil {
			t.Fatalf("%s cleanup: %v", v.name, err)
		}
		results[vi] = durs
	}
	restore()

	t.Logf("fixture %dx%d, PNG %.2f MB, raw %.2f MB, %d frames per variant",
		benchW, benchH, float64(len(fix.pngBytes))/1e6, float64(len(fix.rawPix))/1e6, frames)
	for vi, v := range variants {
		mean, median, minimum, maximum := benchStats(results[vi])
		timed := "write"
		if v.ack {
			timed = "write+ack"
		}
		t.Logf("%-14s payload %6.2f MB  %-9s  mean %8.2f ms  median %8.2f ms  min %8.2f ms  max %8.2f ms",
			v.name, float64(v.payload)/1e6, timed,
			benchMS(mean), benchMS(median), benchMS(minimum), benchMS(maximum))
	}
}
