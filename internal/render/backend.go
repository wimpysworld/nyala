package render

import "fmt"

// Backend identifies one supported terminal rendering backend.
type Backend uint8

const (
	// BackendBlock renders the captured image and text overlay as terminal cells.
	BackendBlock Backend = iota
	// BackendSixel renders the captured image through the Sixel protocol.
	BackendSixel
	// BackendKitty renders the captured image through the Kitty graphics protocol.
	BackendKitty
)

// CaptureFormat identifies the image encoding requested from the browser.
type CaptureFormat uint8

const (
	// CaptureJPEG requests JPEG screencast frames.
	CaptureJPEG CaptureFormat = iota
	// CapturePNG requests PNG screencast frames.
	CapturePNG
)

// ParseBackend parses one backend name at an input boundary.
func ParseBackend(name string) (Backend, error) {
	switch name {
	case BackendBlock.String():
		return BackendBlock, nil
	case BackendSixel.String():
		return BackendSixel, nil
	case BackendKitty.String():
		return BackendKitty, nil
	default:
		return BackendBlock, fmt.Errorf("render: unknown backend %q", name)
	}
}

// String returns the stable backend name.
func (b Backend) String() string {
	switch b {
	case BackendSixel:
		return "sixel"
	case BackendKitty:
		return "kitty"
	default:
		return "block"
	}
}

// CaptureFormat returns the browser screencast encoding for the backend.
func (b Backend) CaptureFormat() CaptureFormat {
	switch b {
	case BackendSixel, BackendKitty:
		return CapturePNG
	default:
		return CaptureJPEG
	}
}

// UsesTextOverlay reports whether the backend consumes the DOM text overlay.
func (b Backend) UsesTextOverlay() bool {
	return b != BackendSixel && b != BackendKitty
}

// UsesDirectPayload reports whether encoded screencast data can bypass decoding.
func (b Backend) UsesDirectPayload() bool {
	return b == BackendKitty
}

// UsesLayoutScaling reports whether the backend applies the block layout scale.
func (b Backend) UsesLayoutScaling() bool {
	return b != BackendSixel && b != BackendKitty
}

// QuantisesScroll reports whether vertical scrolling snaps to whole cells.
func (b Backend) QuantisesScroll() bool {
	return b != BackendSixel && b != BackendKitty
}
