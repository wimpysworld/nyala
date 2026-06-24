package viewer

import (
	"strings"
	"testing"
)

// TestValidateForcedBackend covers the forced-backend decision over an injected
// detection result. It injects the enabled set via SetEnabled, the same seam
// configureBackends fills, never a real terminal. auto and block always pass;
// kitty and sixel pass only when the enabled set lists that backend, else fail
// with an error naming the backend.
func TestValidateForcedBackend(t *testing.T) {
	cases := []struct {
		name      string
		mode      string
		enabled   []Backend
		wantErr   bool
		wantNamed string
	}{
		{"auto passes with graphics", ModeAuto, []Backend{BackendKitty, BackendSixel, BackendBlock}, false, ""},
		{"auto passes block only", ModeAuto, []Backend{BackendBlock}, false, ""},
		{"block passes block only", ModeBlock, []Backend{BackendBlock}, false, ""},
		{"block passes with graphics", ModeBlock, []Backend{BackendKitty, BackendSixel, BackendBlock}, false, ""},
		{"kitty passes when detected", ModeKitty, []Backend{BackendKitty, BackendBlock}, false, ""},
		{"kitty fails when not detected", ModeKitty, []Backend{BackendBlock}, true, "kitty"},
		{"sixel passes when detected", ModeSixel, []Backend{BackendSixel, BackendBlock}, false, ""},
		{"sixel fails when not detected", ModeSixel, []Backend{BackendBlock}, true, "sixel"},
		{"unknown mode fails", "bogus", []Backend{BackendBlock}, true, "bogus"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewState(120, 40, 64, 64)
			s.SetEnabled(c.enabled)
			err := ValidateForcedBackend(c.mode, s.Enabled)
			if c.wantErr {
				if err == nil {
					t.Fatalf("mode %q enabled %v: want error, got nil", c.mode, c.enabled)
				}
				if !strings.Contains(err.Error(), c.wantNamed) {
					t.Fatalf("error %q does not name %q", err.Error(), c.wantNamed)
				}
				return
			}
			if err != nil {
				t.Fatalf("mode %q enabled %v: want nil, got %v", c.mode, c.enabled, err)
			}
		})
	}
}

// TestApplyForcedBackendPinsNamedBackend asserts a named --backend mode forces
// the active backend even when detection advertised a higher-fidelity one. It
// simulates wezterm: detection enables kitty, sixel, then block, so the active
// backend resolves to kitty. Forcing block must pin block and shrink the Tab
// cycle to block only. auto must leave the detected cycle untouched.
func TestApplyForcedBackendPinsNamedBackend(t *testing.T) {
	t.Run("block forces block over detected kitty", func(t *testing.T) {
		st := NewState(120, 40, 64, 64)
		st.SetEnabled([]Backend{BackendKitty, BackendSixel, BackendBlock})
		if st.Backend != BackendKitty {
			t.Fatalf("detected active backend = %v, want kitty", st.Backend)
		}

		applyForcedBackend(&st, ModeBlock)

		if st.Backend != BackendBlock {
			t.Fatalf("forced active backend = %v, want block", st.Backend)
		}
		if len(st.Enabled) != 1 || st.Enabled[0] != BackendBlock {
			t.Fatalf("forced cycle = %v, want block only", st.Enabled)
		}
	})

	t.Run("auto leaves the detected cycle", func(t *testing.T) {
		st := NewState(120, 40, 64, 64)
		st.SetEnabled([]Backend{BackendKitty, BackendSixel, BackendBlock})

		applyForcedBackend(&st, ModeAuto)

		if st.Backend != BackendKitty {
			t.Fatalf("auto active backend = %v, want kitty unchanged", st.Backend)
		}
		if len(st.Enabled) != 3 {
			t.Fatalf("auto cycle = %v, want the detected three backends", st.Enabled)
		}
	})
}
