package kitty

import (
	"crypto/rand"
	stdb64 "encoding/base64"
	"testing"

	emmb64 "github.com/emmansun/base64"
)

// benchPayload builds a representative Kitty encode input: a few hundred KB of
// random bytes, standing in for a zlib-compressed RGB frame body (already
// high-entropy, so incompressible). base64 encode time is data-independent, so
// the byte source only fixes a realistic length.
func benchPayload(tb testing.TB, n int) []byte {
	tb.Helper()
	p := make([]byte, n)
	if _, err := rand.Read(p); err != nil {
		tb.Fatalf("fill payload: %v", err)
	}
	return p
}

// BenchmarkEncodeStdlib measures encoding/base64 StdEncoding.Encode.
func BenchmarkEncodeStdlib(b *testing.B) {
	src := benchPayload(b, 300*1024)
	dst := make([]byte, stdb64.StdEncoding.EncodedLen(len(src)))
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for range b.N {
		stdb64.StdEncoding.Encode(dst, src)
	}
}

// BenchmarkEncodeEmmansun measures emmansun/base64 StdEncoding.Encode, the SIMD
// drop-in the live and capture encoders now use.
func BenchmarkEncodeEmmansun(b *testing.B) {
	src := benchPayload(b, 300*1024)
	dst := make([]byte, emmb64.StdEncoding.EncodedLen(len(src)))
	b.SetBytes(int64(len(src)))
	b.ResetTimer()
	for range b.N {
		emmb64.StdEncoding.Encode(dst, src)
	}
}
