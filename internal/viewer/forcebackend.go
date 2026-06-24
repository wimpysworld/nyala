package viewer

import (
	"fmt"
	"slices"
)

// Backend mode strings the --backend flag accepts. They mirror the kong enum and
// backendName: auto leaves detection in charge, block is the floor, and kitty and
// sixel force a graphics backend that detection must confirm.
const (
	// ModeAuto leaves the detected cycle in charge; the validator always passes it.
	ModeAuto = "auto"
	// ModeBlock forces the block floor backend; the validator always passes it.
	ModeBlock = "block"
	// ModeKitty forces the Kitty graphics backend; the validator passes it only
	// when detection confirmed Kitty.
	ModeKitty = "kitty"
	// ModeSixel forces the Sixel graphics backend; the validator passes it only
	// when detection confirmed Sixel.
	ModeSixel = "sixel"
)

// ValidateForcedBackend reports whether the requested --backend mode is usable
// given the resolved detection result. It is pure and touches no terminal: it
// reads only the enabled set (post-configureBackends), the resolved decision the
// drawing path already uses. ModeAuto and ModeBlock always pass, since block is
// the floor backend present in every enabled set. ModeKitty and ModeSixel pass
// only when enabled lists that backend, that is when detection confirmed it;
// otherwise it returns an unsupported-backend error naming the backend. An
// unknown mode is an unsupported-backend error.
func ValidateForcedBackend(mode string, enabled []Backend) error {
	switch mode {
	case ModeAuto, ModeBlock:
		return nil
	case ModeKitty:
		if slices.Contains(enabled, BackendKitty) {
			return nil
		}
	case ModeSixel:
		if slices.Contains(enabled, BackendSixel) {
			return nil
		}
	}
	return fmt.Errorf("viewer: backend %q is not supported by this terminal", mode)
}
