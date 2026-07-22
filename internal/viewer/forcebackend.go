package viewer

import (
	"fmt"
	"slices"

	"github.com/wimpysworld/nyala/internal/render"
)

// Backend mode strings the --backend flag accepts. They mirror the kong enum:
// auto leaves detection in charge, and a named mode picks the starting backend.
// A graphics start must be confirmed by detection; Tab then cycles every
// supported backend regardless of the start.
const (
	// ModeAuto leaves the detected cycle in charge; the validator always passes it.
	ModeAuto = "auto"
	// ModeBlock starts on the block floor backend; the validator always passes it.
	ModeBlock = "block"
	// ModeKitty starts on the Kitty graphics backend; the validator passes it
	// only when detection confirmed Kitty.
	ModeKitty = "kitty"
	// ModeSixel starts on the Sixel graphics backend; the validator passes it
	// only when detection confirmed Sixel.
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

// forcedBackend maps a named --backend mode to the Backend the session starts
// on. It returns ok=false for ModeAuto and any unnamed mode, meaning no forced
// start: auto leaves the detected nomination in charge. The three named modes
// map to their backend.
func forcedBackend(mode string) (Backend, bool) {
	if mode == ModeAuto {
		return BackendBlock, false
	}
	backend, err := render.ParseBackend(mode)
	return backend, err == nil
}

// applyForcedBackend starts the session on a named --backend mode. For a named
// mode it sets the active backend to the forced one and keeps st.Enabled as
// the detected cycle, so Tab still cycles every supported backend in order
// from the forced start. ModeAuto and any unnamed mode leave the detected
// nomination untouched. Call it only after ValidateForcedBackend passes, so a
// graphics backend the terminal cannot run never becomes the start.
func applyForcedBackend(st *State, mode string) {
	if forced, ok := forcedBackend(mode); ok {
		st.Backend = forced
	}
}
