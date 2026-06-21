//go:build ocr

// This file holds the cgo gosseract engine. It builds only under the "ocr" build
// tag, so the default build and the shipped nyala binary stay cgo-free. The Nix
// dev shell provisions libtesseract and leptonica for the tagged test build (see
// flake.nix, task 8.4). The OCR test skips when the engine is absent.
package ocr

import (
	"bytes"
	"image"
	"image/png"

	"github.com/otiai10/gosseract/v2"
)

// DefaultWhitelist is the Stage 0 char whitelist: the ladder strings use ASCII
// letters, digits, space, and the "px" label punctuation. Constraining the
// alphabet lifts CER on the known synthetic lines.
const DefaultWhitelist = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789 .:-"

// Engine is the gosseract OCR engine wrapping a Tesseract client. It applies the
// fixed Stage 0 tuning: the char whitelist, the DAWG dictionaries disabled, and
// single-block page segmentation. Construct it with NewEngine and Close it when
// done.
type Engine struct {
	client       *gosseract.Client
	conditioning Conditioning
}

// NewEngine builds an Engine with the fixed Stage 0 tuning applied to a fresh
// gosseract client: whitelist set, the system and frequency DAWG dictionaries
// off (Tesseract corrects synthetic text otherwise), and single-block PSM. The
// caller must Close the returned Engine.
func NewEngine() (*Engine, error) {
	c := gosseract.NewClient()
	if err := c.SetWhitelist(DefaultWhitelist); err != nil {
		c.Close()
		return nil, err
	}
	// Disable the DAWG dictionaries so Tesseract does not "correct" the known
	// synthetic ladder strings against its word list.
	for _, v := range []gosseract.SettableVariable{
		"load_system_dawg",
		"load_freq_dawg",
		"load_punc_dawg",
		"load_number_dawg",
		"load_unambig_dawg",
		"load_bigram_dawg",
	} {
		if err := c.SetVariable(v, "0"); err != nil {
			c.Close()
			return nil, err
		}
	}
	if err := c.SetPageSegMode(gosseract.PSM_SINGLE_BLOCK); err != nil {
		c.Close()
		return nil, err
	}
	return &Engine{client: c, conditioning: DefaultConditioning()}, nil
}

// Close releases the underlying Tesseract client.
func (e *Engine) Close() error {
	return e.client.Close()
}

// Recognise runs the fixed conditioning path on src, then OCRs the conditioned
// image and returns the recovered text. srcCapHeight is the source capital
// height in source pixels; it drives the integer upscale.
func (e *Engine) Recognise(src image.Image, srcCapHeight int) (string, error) {
	cond := Condition(src, srcCapHeight, e.conditioning)
	var buf bytes.Buffer
	if err := png.Encode(&buf, cond); err != nil {
		return "", err
	}
	if err := e.client.SetImageFromBytes(buf.Bytes()); err != nil {
		return "", err
	}
	return e.client.Text()
}

// ScoreImage conditions and OCRs src, then scores the result against truth.
func (e *Engine) ScoreImage(src image.Image, srcCapHeight int, truth string) (Result, error) {
	got, err := e.Recognise(src, srcCapHeight)
	if err != nil {
		return Result{}, err
	}
	return Score(truth, trimTrailingNewline(got)), nil
}

// trimTrailingNewline drops the trailing newline Tesseract appends so the scored
// string matches the single-line truth.
func trimTrailingNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
