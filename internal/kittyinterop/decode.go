// Package kittyinterop decodes Kitty graphics streams into an offline terminal
// image model for compatibility tests.
package kittyinterop

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"strconv"
	"strings"
)

const (
	apcIntro    = "\x1b_G"
	st          = "\x1b\\"
	maxChunkLen = 4096
)

// EventKind identifies a Kitty terminal state change.
type EventKind uint8

const (
	// EventTransmit records a transmit-and-place command.
	EventTransmit EventKind = iota + 1
	// EventDelete records deletion of an image and its stored data.
	EventDelete
)

// Chunk records one APC frame in a Kitty command.
type Chunk struct {
	Options      map[string]string
	PayloadBytes int
}

// Event records one reassembled Kitty command and its decoded result.
type Event struct {
	Kind    EventKind
	ID      int
	Format  int
	Width   int
	Height  int
	Payload []byte
	Image   *image.RGBA
	Chunks  []Chunk
}

// Terminal is the image state left by a decoded Kitty graphics stream.
type Terminal struct {
	Events     []Event
	Images     map[int]*image.RGBA
	LastPlaced int
}

type frame struct {
	options map[string]string
	body    []byte
}

// Decode parses consecutive Kitty graphics APC commands, applies transmit,
// place, and delete operations, and returns the visible image state. It supports
// the direct PNG and zlib-compressed RGB forms emitted by Nyala's live viewer.
func Decode(stream []byte) (*Terminal, error) {
	terminal := &Terminal{Images: make(map[int]*image.RGBA)}
	for len(stream) > 0 {
		first, rest, err := parseFrame(stream)
		if err != nil {
			return nil, err
		}
		stream = rest
		frames := []frame{first}
		more, err := continuation(first.options, true)
		if err != nil {
			return nil, err
		}
		for more {
			if len(frames[len(frames)-1].body) != maxChunkLen {
				return nil, fmt.Errorf("kittyinterop: continued chunk has %d payload bytes, want %d", len(frames[len(frames)-1].body), maxChunkLen)
			}
			var next frame
			next, stream, err = parseFrame(stream)
			if err != nil {
				return nil, err
			}
			more, err = continuation(next.options, false)
			if err != nil {
				return nil, err
			}
			frames = append(frames, next)
		}
		if len(frames[len(frames)-1].body) > maxChunkLen {
			return nil, fmt.Errorf("kittyinterop: final chunk has %d payload bytes, limit %d", len(frames[len(frames)-1].body), maxChunkLen)
		}
		event, err := decodeCommand(frames)
		if err != nil {
			return nil, err
		}
		terminal.Events = append(terminal.Events, event)
		switch event.Kind {
		case EventTransmit:
			terminal.Images[event.ID] = event.Image
			terminal.LastPlaced = event.ID
		case EventDelete:
			delete(terminal.Images, event.ID)
			if terminal.LastPlaced == event.ID {
				terminal.LastPlaced = 0
			}
		}
	}
	return terminal, nil
}

func parseFrame(stream []byte) (frame, []byte, error) {
	if !bytes.HasPrefix(stream, []byte(apcIntro)) {
		return frame{}, nil, errors.New("kittyinterop: stream does not start with Kitty APC introducer")
	}
	end := bytes.Index(stream[len(apcIntro):], []byte(st))
	if end < 0 {
		return frame{}, nil, errors.New("kittyinterop: Kitty APC lacks a string terminator")
	}
	end += len(apcIntro)
	inner := stream[len(apcIntro):end]
	control, body, ok := bytes.Cut(inner, []byte{';'})
	if !ok {
		control, body = inner, nil
	}
	options, err := parseOptions(string(control))
	if err != nil {
		return frame{}, nil, err
	}
	return frame{options: options, body: append([]byte(nil), body...)}, stream[end+len(st):], nil
}

func parseOptions(control string) (map[string]string, error) {
	options := make(map[string]string)
	if control == "" {
		return options, nil
	}
	for _, field := range strings.Split(control, ",") {
		key, value, ok := strings.Cut(field, "=")
		if !ok || key == "" || value == "" {
			return nil, fmt.Errorf("kittyinterop: malformed control field %q", field)
		}
		if _, exists := options[key]; exists {
			return nil, fmt.Errorf("kittyinterop: duplicate control key %q", key)
		}
		options[key] = value
	}
	return options, nil
}

func continuation(options map[string]string, first bool) (bool, error) {
	value, present := options["m"]
	if first && !present {
		return false, nil
	}
	if !first && len(options) != 1 {
		return false, errors.New("kittyinterop: continuation frame carries keys other than m")
	}
	switch value {
	case "1":
		return true, nil
	case "0":
		return false, nil
	default:
		return false, fmt.Errorf("kittyinterop: invalid continuation value m=%q", value)
	}
}

func decodeCommand(frames []frame) (Event, error) {
	first := frames[0].options
	event := Event{Chunks: make([]Chunk, len(frames))}
	var encoded bytes.Buffer
	for i, part := range frames {
		event.Chunks[i] = Chunk{Options: part.options, PayloadBytes: len(part.body)}
		encoded.Write(part.body)
	}

	id, err := positiveOption(first, "i")
	if err != nil {
		return Event{}, err
	}
	event.ID = id
	switch first["a"] {
	case "T":
		if transport := first["t"]; (transport != "" && transport != "d") || first["q"] != "2" {
			return Event{}, errors.New("kittyinterop: transmit must use direct transport and suppress replies")
		}
		event.Kind = EventTransmit
		event.Format, err = positiveOption(first, "f")
		if err != nil {
			return Event{}, err
		}
		event.Payload, err = base64.StdEncoding.DecodeString(encoded.String())
		if err != nil {
			return Event{}, fmt.Errorf("kittyinterop: decode base64 payload: %w", err)
		}
		event.Image, err = decodeImage(first, event.Format, event.Payload)
		if err != nil {
			return Event{}, err
		}
		event.Width, event.Height = event.Image.Bounds().Dx(), event.Image.Bounds().Dy()
		return event, nil
	case "d":
		if len(frames) != 1 || len(frames[0].body) != 0 || first["d"] != "I" || first["q"] != "2" {
			return Event{}, errors.New("kittyinterop: delete must free one image id with an empty payload")
		}
		event.Kind = EventDelete
		return event, nil
	default:
		return Event{}, fmt.Errorf("kittyinterop: unsupported action a=%q", first["a"])
	}
}

func decodeImage(options map[string]string, format int, payload []byte) (*image.RGBA, error) {
	switch format {
	case 100:
		decoded, err := png.Decode(bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("kittyinterop: decode PNG: %w", err)
		}
		return toRGBA(decoded), nil
	case 24:
		if options["o"] != "z" {
			return nil, errors.New("kittyinterop: RGB payload is not zlib-compressed")
		}
		width, err := positiveOption(options, "s")
		if err != nil {
			return nil, err
		}
		height, err := positiveOption(options, "v")
		if err != nil {
			return nil, err
		}
		zr, err := zlib.NewReader(bytes.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("kittyinterop: open zlib payload: %w", err)
		}
		rgb, readErr := io.ReadAll(zr)
		closeErr := zr.Close()
		if readErr != nil {
			return nil, fmt.Errorf("kittyinterop: read zlib payload: %w", readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("kittyinterop: close zlib payload: %w", closeErr)
		}
		if len(rgb) != width*height*3 {
			return nil, fmt.Errorf("kittyinterop: RGB payload has %d bytes, want %d", len(rgb), width*height*3)
		}
		out := image.NewRGBA(image.Rect(0, 0, width, height))
		for si, di := 0, 0; si < len(rgb); si, di = si+3, di+4 {
			copy(out.Pix[di:di+3], rgb[si:si+3])
			out.Pix[di+3] = 0xff
		}
		return out, nil
	default:
		return nil, fmt.Errorf("kittyinterop: unsupported pixel format f=%d", format)
	}
}

func positiveOption(options map[string]string, key string) (int, error) {
	value, ok := options[key]
	if !ok {
		return 0, fmt.Errorf("kittyinterop: missing control key %q", key)
	}
	n, err := strconv.Atoi(value)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("kittyinterop: invalid positive integer %s=%q", key, value)
	}
	return n, nil
}

func toRGBA(src image.Image) *image.RGBA {
	bounds := src.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(out, out.Bounds(), src, bounds.Min, draw.Src)
	return out
}
