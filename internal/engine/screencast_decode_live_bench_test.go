//go:build live

package engine

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/wimpysworld/nyala/internal/metrics"
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

var screencastBenchImage image.Image

// BenchmarkCDPScreencastDecode captures Chromium screencast payloads from one
// unchanged 2560x2880 page, then measures three distinct production costs:
//
//   - image-decode-native: encoded bytes to the decoder's native image layout;
//   - screencast-decode-rgba: base64, image decode, and full-size RGBA output;
//   - block-decode-320x270: base64, image decode, and the live block downscale.
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

		b.Run(payload.name+"/screencast-decode-rgba", func(b *testing.B) {
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

		b.Run(payload.name+"/block-decode-320x270", func(b *testing.B) {
			e := &ChromiumEngine{
				backend:  "block",
				captureW: screencastBenchDstW,
				captureH: screencastBenchDstH,
			}
			pool := rowband.New()
			defer pool.Close()
			downscale := metrics.NewParallelDownscale()
			dst := image.NewRGBA(image.Rect(0, 0, screencastBenchDstW, screencastBenchDstH))
			if _, err := decodeBlockBenchFrame(e, payload.data, dst, downscale, pool); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				img, err := decodeBlockBenchFrame(e, payload.data, dst, downscale, pool)
				if err != nil {
					b.Fatal(err)
				}
				screencastBenchImage = img
			}
			reportScreencastBenchMetrics(b, payload)
		})
	}
}

func decodeBlockBenchFrame(e *ChromiumEngine, data string, dst *image.RGBA, downscale *metrics.ParallelDownscale, pool *rowband.Pool) (*image.RGBA, error) {
	img, err := e.decodeLiveFrame(data)
	if err != nil {
		return nil, err
	}
	if rgba, ok := img.(*image.RGBA); ok && img.Bounds().Dx() == screencastBenchDstW && img.Bounds().Dy() == screencastBenchDstH {
		return rgba, nil
	}
	return downscale.DownscaleInto(dst, img, pool), nil
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
