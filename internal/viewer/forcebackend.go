package viewer

import (
	"fmt"
	"slices"

	"github.com/wimpysworld/nyala/internal/render"
)

// Backend mode strings the --backend flag accepts. They mirror the kong enum:
// auto leaves detection in charge, block is the floor, and kitty and
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
	if mode == ModeAuto || mode == ModeBlock {
		return nil
	}
	backend, err := render.ParseBackend(mode)
	if err == nil && slices.Contains(enabled, backend) {
		return nil
	}
	return fmt.Errorf("viewer: backend %q is not supported by this terminal", mode)
}

// forcedBackend maps a named --backend mode to the Backend it pins. It returns
// ok=false for ModeAuto and any unnamed mode, meaning no forcing: auto leaves
// the detected cycle in charge. The three named modes map to their backend.
func forcedBackend(mode string) (Backend, bool) {
	if mode == ModeAuto {
		return BackendBlock, false
	}
	backend, err := render.ParseBackend(mode)
	return backend, err == nil
}

// applyForcedBackend pins the active backend and the Tab cycle to a named
// --backend mode. For a named mode it calls st.SetEnabled with the forced
// backend alone, so st.Backend becomes that backend and the cycle holds only it.
// ModeAuto and any unnamed mode leave the detected cycle untouched. Call it only
// after ValidateForcedBackend passes, so a forced graphics backend the terminal
// cannot run never reaches here.
func applyForcedBackend(st *State, mode string) {
	if forced, ok := forcedBackend(mode); ok {
		st.SetEnabled([]Backend{forced})
	}
}
