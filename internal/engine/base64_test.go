package engine

// base64 wiring guard: the live and capture screencast paths base64-decode the
// Chromium Data field through github.com/emmansun/base64 (SIMD, drop-in
// encoding/base64 API). These tests guard the byte-identical decode across the blob
// and padding/newline edge cases, prove malformed input returns the SIMD decoder's
// error without losing the pooled buffer, and keep an emmansun-vs-stdlib decode
// benchmark so the win stays measurable. The decode sits inside the measured
// in_decode span, ahead of the image decode.
//
// Run:
//   go test -run 'TestEmmansunBase64|TestDecodeBase64Pooled' ./internal/engine/
//   go test -bench BenchmarkBase64Decode -benchmem ./internal/engine/

import (
	stdb64 "encoding/base64"
	"errors"
	"fmt"
	"sync"
	"testing"

	emmb64 "github.com/emmansun/base64"
)

// spikeBlob returns a representative screencast base64 blob: a tiled photographic
// JPEG at the moving-stream quality, base64-encoded with stdlib. This is the exact
// class of bytes the live screencast Data field carries.
func spikeBlob(tb testing.TB) string {
	tb.Helper()
	src := loadSourceRGBA(tb)
	jbytes := encodeBaseline420(tb, tileTo(src, 1920, 1080))
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

// TestDecodeBase64PooledEquivalence confirms the pooled helper is byte-identical to
// stdlib across a real blob and the edge cases (empty, each padding tail, embedded
// CRLF), and that reusing the pooled buffer across calls never leaks stale bytes into a
// later, shorter decode.
func TestDecodeBase64PooledEquivalence(t *testing.T) {
	cases := map[string]string{
		"empty": stdb64.StdEncoding.EncodeToString(nil),
		"pad0":  stdb64.StdEncoding.EncodeToString([]byte("abc")),
		"pad1":  stdb64.StdEncoding.EncodeToString([]byte("abcde")),
		"pad2":  stdb64.StdEncoding.EncodeToString([]byte("abcd")),
		"blob":  spikeBlob(t),
	}
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

	// Order the calls big-then-small so a reused buffer would expose stale tail bytes if
	// the helper ever returned more than n bytes.
	order := []string{"blob", "pad2", "blob_crlf", "pad0", "empty", "pad1"}
	for _, name := range order {
		s := cases[name]
		want, err := stdb64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("%s: stdlib decode: %v", name, err)
		}
		got, release, err := decodeBase64Pooled(s)
		if err != nil {
			t.Fatalf("%s: decodeBase64Pooled: %v", name, err)
		}
		if len(got) != len(want) {
			release()
			t.Fatalf("%s: length %d, want %d", name, len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				release()
				t.Fatalf("%s: byte %d = %d, want %d", name, i, got[i], want[i])
			}
		}
		release()
	}
}

// TestDecodeBase64PooledMalformed proves malformed input returns the emmansun error
// and restores the borrowed buffer pointer to the pool before returning.
func TestDecodeBase64PooledMalformed(t *testing.T) {
	const bad = "!!!!"
	base64DecodeBufPool = sync.Pool{New: newB64Buf}
	seed, _ := newB64Buf().(*b64Buf)
	seed.buf = make([]byte, 0, emmb64.StdEncoding.DecodedLen(len(bad)))
	base64DecodeBufPool.Put(seed)

	wantBuf := make([]byte, emmb64.StdEncoding.DecodedLen(len(bad)))
	_, wantErr := emmb64.StdEncoding.Decode(wantBuf, []byte(bad))
	raw, release, gotErr := decodeBase64Pooled(bad)
	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("error = %v, want emmansun error %v", gotErr, wantErr)
	}
	if raw != nil || release != nil {
		t.Fatalf("error result has raw=%t release=%t, want both false", raw != nil, release != nil)
	}
	if raceEnabled {
		// The race-enabled sync.Pool intentionally drops random Put calls, so
		// the borrowed-pointer round trip cannot be asserted under -race.
		return
	}
	reused, _ := base64DecodeBufPool.Get().(*b64Buf)
	if reused != seed {
		t.Fatalf("pool returned buffer %p, want borrowed buffer %p", reused, seed)
	}
	base64DecodeBufPool.Put(reused)
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

// benchB64Pooled measures the wired pooled path on a string input: it calls the real
// decodeBase64Pooled helper, so it captures the string-src zero-copy view and the
// pooled-buffer reuse exactly as the live and capture decode paths run them.
func benchB64Pooled(b *testing.B, s string) {
	b.SetBytes(int64(len(s)))
	b.ReportAllocs()
	for range b.N {
		out, release, err := decodeBase64Pooled(s)
		if err != nil {
			b.Fatal(err)
		}
		_ = out
		release()
	}
}

// BenchmarkBase64Decode compares stdlib, emmansun DecodeString, emmansun pooled Decode
// over a []byte src, and the wired string-input decodeBase64Pooled helper on the
// representative screencast blob.
func BenchmarkBase64Decode(b *testing.B) {
	s := spikeBlob(b)
	b.Run(fmt.Sprintf("blob-%dB/stdlib", len(s)), func(b *testing.B) { benchB64Stdlib(b, s) })
	b.Run(fmt.Sprintf("blob-%dB/emmansun", len(s)), func(b *testing.B) { benchB64Emmansun(b, s) })
	b.Run(fmt.Sprintf("blob-%dB/emmansun-pooled", len(s)), func(b *testing.B) { benchB64EmmansunPooled(b, s) })
	b.Run(fmt.Sprintf("blob-%dB/pooled-str", len(s)), func(b *testing.B) { benchB64Pooled(b, s) })
}
