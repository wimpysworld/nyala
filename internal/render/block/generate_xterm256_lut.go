//go:build ignore

// Command generate_xterm256_lut writes the static perceptual xterm-256 lookup
// table embedded by colour.go.
package main

import (
	"fmt"
	"image/color"
	"math"
	"os"

	colorful "github.com/lucasb-eyer/go-colorful"
)

const output = "xterm256_perceptual_lut.bin"

func main() {
	palette := make([]color.RGBA, 0, 240)
	for index := 16; index < 256; index++ {
		palette = append(palette, xtermColour(uint8(index)))
	}
	lut := make([]byte, 1<<15)
	for key := range lut {
		candidate := color.RGBA{
			R: expand5(uint8(key >> 10)),
			G: expand5(uint8(key >> 5 & 31)),
			B: expand5(uint8(key & 31)),
			A: 0xff,
		}
		bestIndex, bestDistance := uint8(16), math.Inf(1)
		for offset, paletteColour := range palette {
			distance := deltaE2000(candidate, paletteColour)
			if distance < bestDistance {
				bestIndex, bestDistance = uint8(offset+16), distance
			}
		}
		lut[key] = bestIndex
	}
	if err := os.WriteFile(output, lut, 0o644); err != nil {
		panic(fmt.Errorf("write %s: %w", output, err))
	}
}

func deltaE2000(a, b color.RGBA) float64 {
	ca := colorful.Color{R: float64(a.R) / 255, G: float64(a.G) / 255, B: float64(a.B) / 255}
	cb := colorful.Color{R: float64(b.R) / 255, G: float64(b.G) / 255, B: float64(b.B) / 255}
	return ca.DistanceCIEDE2000(cb)
}

func xtermColour(index uint8) color.RGBA {
	if index >= 232 {
		value := uint8(8 + 10*(int(index)-232))
		return color.RGBA{R: value, G: value, B: value, A: 0xff}
	}
	cube := int(index) - 16
	return color.RGBA{
		R: cubeLevel(cube / 36),
		G: cubeLevel(cube / 6 % 6),
		B: cubeLevel(cube % 6),
		A: 0xff,
	}
}

func cubeLevel(axis int) uint8 {
	if axis == 0 {
		return 0
	}
	return uint8(55 + 40*axis)
}

func expand5(value uint8) uint8 { return value<<3 | value>>2 }
