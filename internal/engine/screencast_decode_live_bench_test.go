//go:build live

package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/wimpysworld/nyala/internal/metrics"
	"github.com/wimpysworld/nyala/internal/render"
	"github.com/wimpysworld/nyala/internal/render/block"
	"github.com/wimpysworld/nyala/internal/rowband"
)

const (
	screencastBenchWidth   = 2560
	screencastBenchHeight  = 2880
	screencastBenchSamples = 7
	screencastBenchDstW    = 320 // 160 columns, two sextant samples per cell.
	screencastBenchDstH    = 270 // 90 rows, three sextant samples per cell.
)

type screencastBenchFormat struct {
	name    string
	format  page.ScreencastFormat
	quality int
}

var screencastBenchFormats = []screencastBenchFormat{
	{name: "png", format: page.ScreencastFormatPng},
	{name: "jpeg-q80", format: page.ScreencastFormatJpeg, quality: 80},
	{name: "jpeg-q85", format: page.ScreencastFormatJpeg, quality: 85},
	{name: "jpeg-q90", format: page.ScreencastFormatJpeg, quality: 90},
	{name: "jpeg-q95", format: page.ScreencastFormatJpeg, quality: 95},
	{name: "jpeg-q100", format: page.ScreencastFormatJpeg, quality: 100},
}

type screencastBenchPayload struct {
	name       string
	data       string
	raw        []byte
	deliveryMS []float64
}

var (
	screencastBenchImage image.Image
	screencastBenchGrid  block.Grid
)

// BenchmarkCDPScreencastDecode captures Chromium screencast payloads from one
// unchanged 2560x2880 page, then measures three distinct production costs:
//
//   - image-decode-native: encoded bytes to the decoder's native image layout;
//   - screencast-decode-native: base64 and decoder-native image decode;
//   - block-decode+direct-compose-160x90: base64, native image decode, and direct
//     composition of the final Block grid.
//
// Chromium applies JPEG quality while encoding each captured payload. Quality is
// not passed to any decoder. cdp-p50-ms and cdp-range-ms describe StartScreencast
// to frame-event delivery, which includes Chromium encoding, base64 creation, CDP
// transport, and command/event scheduling. It does not include Nyala's decode.
// Run with:
//
//	go test -tags live -run '^$' -bench '^BenchmarkCDPScreencastDecode$' \
//		-benchmem -benchtime=2s -count=5 ./internal/engine/
func BenchmarkCDPScreencastDecode(b *testing.B) {
	payloads := collectScreencastBenchPayloads(b)
	for _, payload := range payloads {
		payload := payload
		b.Run(payload.name+"/image-decode-native", func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				img, err := decodeJPEGFast(payload.raw)
				if err != nil {
					b.Fatal(err)
				}
				screencastBenchImage = img
			}
			reportScreencastBenchMetrics(b, payload)
		})

		b.Run(payload.name+"/screencast-decode-native", func(b *testing.B) {
			e := &ChromiumEngine{backend: "kitty"}
			if _, err := e.decodeLiveFrame(payload.data); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				img, err := e.decodeLiveFrame(payload.data)
				if err != nil {
					b.Fatal(err)
				}
				screencastBenchImage = img
			}
			reportScreencastBenchMetrics(b, payload)
		})

		b.Run(payload.name+"/block-decode+direct-compose-160x90", func(b *testing.B) {
			e := &ChromiumEngine{
				backend:  "block",
				captureW: screencastBenchDstW,
				captureH: screencastBenchDstH,
			}
			pool := rowband.New()
			defer pool.Close()
			downscale := metrics.NewParallelDownscale()
			compose := block.NewParallelCompose()
			br := block.Renderer{Cols: 160, Rows: 90, Scheme: block.SchemeSextant}
			var grid block.Grid
			dst := image.NewRGBA(image.Rect(0, 0, screencastBenchDstW, screencastBenchDstH))
			if _, err := decodeBlockBenchFrame(e, payload.data, &br, &grid, dst, downscale, compose, pool); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				grid, err := decodeBlockBenchFrame(e, payload.data, &br, &grid, dst, downscale, compose, pool)
				if err != nil {
					b.Fatal(err)
				}
				screencastBenchGrid = *grid
			}
			reportScreencastBenchMetrics(b, payload)
		})
	}
}

// BenchmarkCDPBlockComposeMatched compares the former intermediate-RGBA path
// with direct native-YCbCr composition. Both arms decode the same captured JPEG
// and complete the same 160x90 sextant grid. It reports no CDP delivery metrics,
// because those are shared capture history rather than work done by either arm.
func BenchmarkCDPBlockComposeMatched(b *testing.B) {
	payloads := collectScreencastBenchPayloads(b)
	for _, payload := range payloads {
		if !strings.HasPrefix(payload.name, "jpeg-") {
			continue
		}
		assertMatchedBlockGrid(b, payload)
		payload := payload
		b.Run(payload.name+"/legacy-decode+intermediate-rgba+grid", func(b *testing.B) {
			benchmarkMatchedBlockCompose(b, payload, false)
		})
		b.Run(payload.name+"/direct-decode+ycbcr-grid", func(b *testing.B) {
			benchmarkMatchedBlockCompose(b, payload, true)
		})
	}
}

// BenchmarkBlockDecodePoolSaturatedCompute runs q80 through the production
// decode-worker function at defaultDecodeWorkers size and counts completed final
// grids. Its blocking feed keeps the workers saturated, but does not model CDP
// acknowledgement, rawFrameQueue.put drop policy, monotonic delivery, or display
// cadence. The result is compute throughput, not delivered FPS.
func BenchmarkBlockDecodePoolSaturatedCompute(b *testing.B) {
	var q80 screencastBenchPayload
	for _, payload := range collectScreencastBenchPayloads(b) {
		if payload.name == "jpeg-q80" {
			q80 = payload
			break
		}
	}
	if q80.data == "" {
		b.Fatal("jpeg-q80 payload not captured")
	}
	assertMatchedBlockGrid(b, q80)
	b.Run("jpeg-q80/legacy-workers+intermediate-rgba+grid", func(b *testing.B) {
		benchmarkMatchedBlockPool(b, q80, false)
	})
	b.Run("jpeg-q80/direct-workers+ycbcr-grid", func(b *testing.B) {
		benchmarkMatchedBlockPool(b, q80, true)
	})
}

func benchmarkMatchedBlockCompose(b *testing.B, payload screencastBenchPayload, direct bool) {
	b.Helper()
	e := &ChromiumEngine{}
	pool := rowband.New()
	defer pool.Close()
	pc := block.NewParallelCompose()
	br := block.Renderer{Cols: 160, Rows: 90, Scheme: block.SchemeSextant}
	var grid block.Grid
	compose := func(img image.Image) {
		frame := render.Frame{Img: img}
		if direct {
			pc.ComposeYCbCrInto(&br, &grid, frame, pool)
		} else {
			yc, ok := img.(*image.YCbCr)
			if !ok {
				b.Fatalf("decoded %s image is %T, want *image.YCbCr", payload.name, img)
			}
			rgba := image.NewRGBA(image.Rect(0, 0, screencastBenchDstW, screencastBenchDstH))
			legacyAverageYCbCrToRGBA(rgba, yc)
			frame.Img = rgba
			pc.ComposeInto(&br, &grid, frame, pool)
		}
		screencastBenchGrid = grid
	}
	img, err := e.decodeLiveFrame(payload.data)
	if err != nil {
		b.Fatal(err)
	}
	compose(img)
	b.ReportAllocs()
	b.SetBytes(int64(len(payload.raw)))
	b.ResetTimer()
	for range b.N {
		img, err := e.decodeLiveFrame(payload.data)
		if err != nil {
			b.Fatal(err)
		}
		compose(img)
	}
}

func benchmarkMatchedBlockPool(b *testing.B, payload screencastBenchPayload, direct bool) {
	b.Helper()
	workers := defaultDecodeWorkers()
	e := &ChromiumEngine{backend: "block", ingest: newIngestCounter()}
	if !direct {
		e.decode = func(data string) (*image.RGBA, error) {
			img, err := e.decodeLiveFrame(data)
			if err != nil {
				return nil, err
			}
			yc, ok := img.(*image.YCbCr)
			if !ok {
				return nil, fmt.Errorf("decoded pool image is %T, want *image.YCbCr", img)
			}
			rgba := image.NewRGBA(image.Rect(0, 0, screencastBenchDstW, screencastBenchDstH))
			legacyAverageYCbCrToRGBA(rgba, yc)
			return rgba, nil
		}
	}
	queue := newRawFrameQueue(workers * 2)
	decoded := make(chan decodedFrame, workers*2)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.runDecodeWorker(ctx, queue, decoded)
		}()
	}
	pool := rowband.New()
	defer pool.Close()
	pc := block.NewParallelCompose()
	br := block.Renderer{Cols: 160, Rows: 90, Scheme: block.SchemeSextant}
	var grid block.Grid
	b.ReportAllocs()
	b.SetBytes(int64(len(payload.raw)))
	b.ResetTimer()
	b.ReportMetric(float64(workers), "workers")
	go func() {
		for i := range b.N {
			queue.items <- seqRawFrame{seq: uint64(i + 1), data: payload.data}
		}
	}()
	for range b.N {
		frame := <-decoded
		img := frame.pending.img
		if direct {
			pc.ComposeYCbCrInto(&br, &grid, render.Frame{Img: img}, pool)
		} else {
			rgba, ok := img.(*image.RGBA)
			if !ok {
				b.Fatalf("legacy worker image is %T, want *image.RGBA", img)
			}
			pc.ComposeInto(&br, &grid, render.Frame{Img: rgba}, pool)
		}
		screencastBenchGrid = grid
	}
	b.StopTimer()
	cancel()
	wg.Wait()
}

func assertMatchedBlockGrid(tb testing.TB, payload screencastBenchPayload) {
	tb.Helper()
	e := &ChromiumEngine{}
	img, err := e.decodeLiveFrame(payload.data)
	if err != nil {
		tb.Fatalf("decode %s: %v", payload.name, err)
	}
	yc, ok := img.(*image.YCbCr)
	if !ok {
		tb.Fatalf("decoded %s image is %T, want *image.YCbCr", payload.name, img)
	}
	br := block.Renderer{Cols: 160, Rows: 90, Scheme: block.SchemeSextant}
	want := br.Compose(render.Frame{Img: oracleYCbCrBlockRGBA(yc, screencastBenchDstW, screencastBenchDstH)})
	legacyRGBA := image.NewRGBA(image.Rect(0, 0, screencastBenchDstW, screencastBenchDstH))
	legacyAverageYCbCrToRGBA(legacyRGBA, yc)
	legacy := br.Compose(render.Frame{Img: legacyRGBA})
	if !reflect.DeepEqual(legacy, want) {
		tb.Fatalf("%s faithful legacy grid differs from independent intermediate-RGBA grid", payload.name)
	}
	got := br.ComposeYCbCr(render.Frame{Img: yc})
	if !reflect.DeepEqual(got, want) {
		tb.Fatalf("%s direct grid differs from independent intermediate-RGBA grid", payload.name)
	}
}

// oracleYCbCrBlockRGBA is an independent equality oracle. Standard-library
// YCbCrAt owns all plane and subsampling offsets, so this does not repeat either
// timed kernel's indexing.
func oracleYCbCrBlockRGBA(src *image.YCbCr, dw, dh int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	for dy := range dh {
		sy0 := dy * sh / dh
		sy1 := (dy + 1) * sh / dh
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for dx := range dw {
			sx0 := dx * sw / dw
			sx1 := (dx + 1) * sw / dw
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var sumY, sumCb, sumCr, n uint64
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					c := src.YCbCrAt(sb.Min.X+sx, sb.Min.Y+sy)
					sumY += uint64(c.Y)
					sumCb += uint64(c.Cb)
					sumCr += uint64(c.Cr)
					n++
				}
			}
			r, g, blue := color.YCbCrToRGB(byte(sumY/n), byte(sumCb/n), byte(sumCr/n))
			offset := dst.PixOffset(dx, dy)
			dst.Pix[offset], dst.Pix[offset+1], dst.Pix[offset+2], dst.Pix[offset+3] = r, g, blue, 0xff
		}
	}
	return dst
}

// legacyAverageYCbCrToRGBA is a faithful test-only copy of the removed optimised
// production kernel. Its destination and scratch allocations remain in the timed
// legacy arms.
func legacyAverageYCbCrToRGBA(dst *image.RGBA, src *image.YCbCr) {
	db := dst.Bounds()
	dw, dh := db.Dx(), db.Dy()
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if dw == 0 || dh == 0 || sw == 0 || sh == 0 {
		return
	}
	ratH, ratV := legacySubsampleShift(src.SubsampleRatio)
	minXc := sb.Min.X / ratH
	minYc := sb.Min.Y / ratV
	cxIdx := make([]int, sw)
	for px := range sw {
		cxIdx[px] = (sb.Min.X+px)/ratH - minXc
	}
	sx0s := make([]int, dw)
	sx1s := make([]int, dw)
	for dx := range dw {
		sx0s[dx] = dx * sw / dw
		sx1s[dx] = (dx + 1) * sw / dw
	}
	sumY := make([]uint64, dw)
	sumCb := make([]uint64, dw)
	sumCr := make([]uint64, dw)
	cnt := make([]uint64, dw)
	for dy := range dh {
		for dx := range dw {
			sumY[dx], sumCb[dx], sumCr[dx], cnt[dx] = 0, 0, 0, 0
		}
		sy0 := dy * sh / dh
		sy1 := (dy + 1) * sh / dh
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for py := sy0; py < sy1; py++ {
			yRow := src.Y[py*src.YStride:]
			cBase := ((sb.Min.Y+py)/ratV - minYc) * src.CStride
			cb := src.Cb[cBase:]
			cr := src.Cr[cBase:]
			for dx := range dw {
				sx0, sx1 := sx0s[dx], sx1s[dx]
				if sx1 <= sx0 {
					sx1 = sx0 + 1
				}
				var ay, acb, acr, an uint64
				for px := sx0; px < sx1; px++ {
					ci := cxIdx[px]
					ay += uint64(yRow[px])
					acb += uint64(cb[ci])
					acr += uint64(cr[ci])
					an++
				}
				sumY[dx] += ay
				sumCb[dx] += acb
				sumCr[dx] += acr
				cnt[dx] += an
			}
		}
		for dx := range dw {
			n := cnt[dx]
			if n == 0 {
				n = 1
			}
			yv := byte(sumY[dx] / n & 0xff)
			cbv := byte(sumCb[dx] / n & 0xff)
			crv := byte(sumCr[dx] / n & 0xff)
			r, g, blue := color.YCbCrToRGB(yv, cbv, crv)
			offset := dst.PixOffset(db.Min.X+dx, db.Min.Y+dy)
			dst.Pix[offset] = r
			dst.Pix[offset+1] = g
			dst.Pix[offset+2] = blue
			dst.Pix[offset+3] = 0xff
		}
	}
}

func legacySubsampleShift(r image.YCbCrSubsampleRatio) (h, v int) {
	switch r {
	case image.YCbCrSubsampleRatio420:
		return 2, 2
	case image.YCbCrSubsampleRatio422:
		return 2, 1
	case image.YCbCrSubsampleRatio440:
		return 1, 2
	case image.YCbCrSubsampleRatio411:
		return 4, 1
	case image.YCbCrSubsampleRatio410:
		return 4, 2
	default:
		return 1, 1
	}
}

func decodeBlockBenchFrame(
	e *ChromiumEngine,
	data string,
	br *block.Renderer,
	grid *block.Grid,
	dst *image.RGBA,
	downscale *metrics.ParallelDownscale,
	compose *block.ParallelCompose,
	pool *rowband.Pool,
) (*block.Grid, error) {
	img, err := e.decodeLiveFrame(data)
	if err != nil {
		return nil, err
	}
	frame := render.Frame{Img: img}
	if yc, ok := img.(*image.YCbCr); ok && screencastBenchDstW < yc.Bounds().Dx() && screencastBenchDstH < yc.Bounds().Dy() {
		return compose.ComposeYCbCrInto(br, grid, frame, pool), nil
	}
	frame.Img = downscale.DownscaleInto(dst, img, pool)
	return compose.ComposeInto(br, grid, frame, pool), nil
}

func reportScreencastBenchMetrics(b *testing.B, payload screencastBenchPayload) {
	b.Helper()
	times := append([]float64(nil), payload.deliveryMS...)
	sort.Float64s(times)
	median := times[len(times)/2]
	b.ReportMetric(float64(len(payload.raw))/(1024*1024), "payload-MiB")
	b.ReportMetric(median, "cdp-p50-ms")
	b.ReportMetric(times[len(times)-1]-times[0], "cdp-range-ms")
	b.SetBytes(int64(len(payload.raw)))
}

func TestCDPScreencastJPEGFidelity(t *testing.T) {
	payloads := collectScreencastBenchPayloads(t)
	var reference *image.RGBA
	for _, payload := range payloads {
		img, err := decodeJPEGFast(payload.raw)
		if err != nil {
			t.Fatalf("decode %s: %v", payload.name, err)
		}
		rgba := imageToRGBA(img)
		if payload.name == "png" {
			reference = rgba
			continue
		}
		if reference == nil {
			t.Fatal("PNG reference must precede JPEG payloads")
		}
		t.Logf(
			"%s payload-B=%d PSNR=%.6f SSIM=%.9f",
			payload.name,
			len(payload.raw),
			metrics.PSNR(reference, rgba),
			metrics.SSIM(reference, rgba),
		)
	}
}

func collectScreencastBenchPayloads(tb testing.TB) []screencastBenchPayload {
	tb.Helper()
	execPath, err := resolveExecutable("")
	if err != nil {
		tb.Skipf("no Chromium for screencast benchmark: %v", err)
	}
	fixture, err := os.ReadFile("../../testdata/linuxmatters.png")
	if err != nil {
		tb.Fatalf("read source fixture: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fixture.png" {
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(fixture)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<!doctype html><style>html,body{margin:0;width:100%;height:100%;overflow:hidden;background:#fff}img{display:block;width:100vw;height:100vh}</style><img src='/fixture.png'>"))
	}))
	defer srv.Close()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(execPath),
		chromedp.WindowSize(screencastBenchWidth, screencastBenchHeight),
		chromedp.Flag("force-device-scale-factor", "1"),
	)
	if os.Getenv("FENCE_SANDBOX") == "1" {
		opts = append(opts, chromedp.NoSandbox)
	}
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer allocCancel()
	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()
	ctx, timeoutCancel := context.WithTimeout(ctx, 3*time.Minute)
	defer timeoutCancel()

	frames := make(chan *page.EventScreencastFrame, 8)
	chromedp.ListenTarget(ctx, func(ev any) {
		if frame, ok := ev.(*page.EventScreencastFrame); ok {
			select {
			case frames <- frame:
			default:
			}
		}
	})
	if err := chromedp.Run(ctx,
		page.Enable(),
		emulation.SetDeviceMetricsOverride(screencastBenchWidth, screencastBenchHeight, 1, false),
		chromedp.Navigate(srv.URL),
	); err != nil {
		tb.Fatalf("prepare Chromium benchmark page: %v", err)
	}
	var ready bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(`document.readyState === "complete" && document.querySelector("img").complete`, &ready)); err != nil || !ready {
		tb.Fatalf("source fixture did not finish loading: ready=%t err=%v", ready, err)
	}

	payloads := make([]screencastBenchPayload, len(screencastBenchFormats))
	for i, format := range screencastBenchFormats {
		payloads[i].name = format.name
		if _, _, err := captureScreencastBenchFrame(ctx, frames, format); err != nil {
			tb.Fatalf("warm %s screencast encoder: %v", format.name, err)
		}
	}
	for range screencastBenchSamples {
		for i, format := range screencastBenchFormats {
			data, elapsed, err := captureScreencastBenchFrame(ctx, frames, format)
			if err != nil {
				tb.Fatalf("capture %s screencast payload: %v", format.name, err)
			}
			payloads[i].deliveryMS = append(payloads[i].deliveryMS, float64(elapsed)/float64(time.Millisecond))
			if payloads[i].data == "" {
				payloads[i].data = data
			}
		}
	}
	for i := range payloads {
		raw, err := base64.StdEncoding.DecodeString(payloads[i].data)
		if err != nil {
			tb.Fatalf("decode %s base64 payload: %v", payloads[i].name, err)
		}
		cfg, err := pngOrJPEGConfig(raw)
		if err != nil {
			tb.Fatalf("read %s image config: %v", payloads[i].name, err)
		}
		if cfg.Width != screencastBenchWidth || cfg.Height != screencastBenchHeight {
			tb.Fatalf("%s payload dimensions = %dx%d, want %dx%d", payloads[i].name, cfg.Width, cfg.Height, screencastBenchWidth, screencastBenchHeight)
		}
		payloads[i].raw = raw
	}
	return payloads
}

func captureScreencastBenchFrame(ctx context.Context, frames chan *page.EventScreencastFrame, format screencastBenchFormat) (string, time.Duration, error) {
	for len(frames) > 0 {
		<-frames
	}
	action := page.StartScreencast().
		WithFormat(format.format).
		WithMaxWidth(screencastBenchWidth).
		WithMaxHeight(screencastBenchHeight)
	if format.format == page.ScreencastFormatJpeg {
		action = action.WithQuality(int64(format.quality))
	}
	start := time.Now()
	if err := chromedp.Run(ctx, action); err != nil {
		return "", 0, err
	}
	var frame *page.EventScreencastFrame
	select {
	case frame = <-frames:
	case <-time.After(20 * time.Second):
		_ = chromedp.Run(ctx, page.StopScreencast())
		return "", 0, fmt.Errorf("timed out waiting for %s frame", format.name)
	case <-ctx.Done():
		return "", 0, ctx.Err()
	}
	elapsed := time.Since(start)
	if err := chromedp.Run(ctx, page.ScreencastFrameAck(frame.SessionID), page.StopScreencast()); err != nil {
		return "", 0, err
	}
	return frame.Data, elapsed, nil
}

func pngOrJPEGConfig(raw []byte) (image.Config, error) {
	if bytes.HasPrefix(raw, pngSignature) {
		return png.DecodeConfig(bytes.NewReader(raw))
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return image.Config{}, err
	}
	if format != "jpeg" {
		return image.Config{}, fmt.Errorf("image format = %q, want jpeg", format)
	}
	return cfg, nil
}
