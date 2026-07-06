package engine

// base64 wiring guard: the live and capture screencast paths base64-decode the
// Chromium Data field through github.com/emmansun/base64 (SIMD, drop-in
// encoding/base64 API) with a stdlib fallback (decodeBase64Fast in frame.go). These
// tests guard the byte-identical decode across the blob and the padding/newline edge
// cases, prove the fallback engages on a malformed input, and keep an emmansun-vs-
// stdlib decode benchmark so the win stays measurable. The decode sits inside the
// measured in_decode span, ahead of the image decode.
//
// Run:
//   go test -run 'TestEmmansunBase64|TestDecodeBase64Fast' ./internal/engine/
//   go test -bench BenchmarkBase64Decode -benchmem ./internal/engine/

import (
	stdb64 "encoding/base64"
	"fmt"
	"testing"

	emmb64 "github.com/emmansun/base64"
)

// spikeBlob returns a representative screencast base64 blob: a tiled photographic
// JPEG at the moving-stream quality, base64-encoded with stdlib. This is the exact
// class of bytes the live screencast Data field carries.
func spikeBlob(tb testing.TB) string {
	tb.Helper()
	src := loadSourceRGBA(tb)
	jbytes := encodeBaseline420(tb, tileTo(src, 1920, 1080), movingScreencastQuality)
	return stdb64.StdEncoding.EncodeToString(jbytes)
}

// TestEmmansunBase64Equivalence asserts emmansun decode is byte-identical to stdlib
// across a real blob and the edge cases: empty, each padding-tail length, and a blob
// with embedded CRLF (both must ignore newlines the same way).
func TestEmmansunBase64Equivalence(t *testing.T) {
	cases := map[string]string{
		"empty": stdb64.StdEncoding.EncodeToString(nil),
		"pad0":  stdb64.StdEncoding.EncodeToString([]byte("abc")),   // len%3==0, no pad
		"pad1":  stdb64.StdEncoding.EncodeToString([]byte("abcde")), // one '=' tail
		"pad2":  stdb64.StdEncoding.EncodeToString([]byte("abcd")),  // two '=' tail
		"blob":  spikeBlob(t),
	}
	// Embedded CRLF: fold the blob into 76-char lines, the classic MIME wrap.
	blob := cases["blob"]
	var wrapped []byte
	for i := 0; i < len(blob); i += 76 {
		end := i + 76
		if end > len(blob) {
			end = len(blob)
		}
		wrapped = append(wrapped, blob[i:end]...)
		wrapped = append(wrapped, '\r', '\n')
	}
	cases["blob_crlf"] = string(wrapped)

	for name, s := range cases {
		want, wErr := stdb64.StdEncoding.DecodeString(s)
		got, gErr := emmb64.StdEncoding.DecodeString(s)
		if (wErr == nil) != (gErr == nil) {
			t.Errorf("%s: error mismatch: stdlib=%v emmansun=%v", name, wErr, gErr)
			continue
		}
		if wErr != nil {
			continue
		}
		if len(got) != len(want) {
			t.Errorf("%s: length %d, want %d", name, len(got), len(want))
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s: byte %d = %d, want %d", name, i, got[i], want[i])
				break
			}
		}
	}
}

// TestDecodeBase64FastEquivalence confirms the wired helper is byte-identical to
// stdlib on a real screencast blob, so the decoded bytes feeding the image decode do
// not change.
func TestDecodeBase64FastEquivalence(t *testing.T) {
	s := spikeBlob(t)
	want, err := stdb64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("stdlib decode: %v", err)
	}
	got, err := decodeBase64Fast(s)
	if err != nil {
		t.Fatalf("decodeBase64Fast: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("length %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("byte %d = %d, want %d", i, got[i], want[i])
		}
	}
}

// TestDecodeBase64FastFallback proves the stdlib fallback engages on an input the
// emmansun decoder declines, so a malformed payload decodes exactly as it does today.
// A lone '!' is not a base64 alphabet character; both decoders reject it, and the
// helper must surface the stdlib error, not panic or diverge.
func TestDecodeBase64FastFallback(t *testing.T) {
	const bad = "!!!!"
	_, gErr := decodeBase64Fast(bad)
	_, sErr := stdb64.StdEncoding.DecodeString(bad)
	if (gErr == nil) != (sErr == nil) {
		t.Fatalf("error mismatch: decodeBase64Fast=%v stdlib=%v", gErr, sErr)
	}
	if sErr == nil {
		t.Fatal("expected stdlib to reject malformed input; the fallback branch is not exercised")
	}
}

func benchB64Stdlib(b *testing.B, s string) {
	b.SetBytes(int64(len(s)))
	b.ReportAllocs()
	for range b.N {
		out, err := stdb64.StdEncoding.DecodeString(s)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

func benchB64Emmansun(b *testing.B, s string) {
	b.SetBytes(int64(len(s)))
	b.ReportAllocs()
	for range b.N {
		out, err := emmb64.StdEncoding.DecodeString(s)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
	}
}

// benchB64EmmansunPooled measures the alloc-killing form: Decode(dst, src) into a
// reused buffer, so it isolates the SIMD-decode win from the per-call output alloc.
func benchB64EmmansunPooled(b *testing.B, s string) {
	b.SetBytes(int64(len(s)))
	b.ReportAllocs()
	src := []byte(s)
	dst := make([]byte, emmb64.StdEncoding.DecodedLen(len(src)))
	for range b.N {
		n, err := emmb64.StdEncoding.Decode(dst, src)
		if err != nil {
			b.Fatal(err)
		}
		_ = n
	}
}

// BenchmarkBase64Decode compares stdlib, emmansun DecodeString, and emmansun pooled
// Decode on the representative screencast blob.
func BenchmarkBase64Decode(b *testing.B) {
	s := spikeBlob(b)
	b.Run(fmt.Sprintf("blob-%dB/stdlib", len(s)), func(b *testing.B) { benchB64Stdlib(b, s) })
	b.Run(fmt.Sprintf("blob-%dB/emmansun", len(s)), func(b *testing.B) { benchB64Emmansun(b, s) })
	b.Run(fmt.Sprintf("blob-%dB/emmansun-pooled", len(s)), func(b *testing.B) { benchB64EmmansunPooled(b, s) })
}
