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
		{"block passes without block capability", ModeBlock, []Backend{BackendKitty, BackendSixel}, false, ""},
		{"kitty passes when detected", ModeKitty, []Backend{BackendKitty, BackendBlock}, false, ""},
		{"kitty fails when not detected", ModeKitty, []Backend{BackendBlock}, true, "kitty"},
		{"sixel passes when detected", ModeSixel, []Backend{BackendSixel, BackendBlock}, false, ""},
		{"sixel fails when not detected", ModeSixel, []Backend{BackendBlock}, true, "sixel"},
		{"unknown mode fails", "bogus", []Backend{BackendBlock}, true, "bogus"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewState(120, 40)
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

// TestApplyForcedBackendSetsStartingBackend asserts a named --backend mode
// selects the starting backend without shrinking the Tab cycle. It simulates
// wezterm: detection enables kitty, sixel, then block, so the active backend
// resolves to kitty. Forcing block must start on block, keep all three
// backends in the cycle, and let Tab cycle kitty -> sixel -> block -> wrap
// from that start. auto must leave the detected nomination untouched.
func TestApplyForcedBackendSetsStartingBackend(t *testing.T) {
	t.Run("block starts on block over detected kitty", func(t *testing.T) {
		st := NewState(120, 40)
		st.SetEnabled([]Backend{BackendKitty, BackendSixel, BackendBlock})
		if st.Backend != BackendKitty {
			t.Fatalf("detected active backend = %v, want kitty", st.Backend)
		}

		applyForcedBackend(&st, ModeBlock)

		if st.Backend != BackendBlock {
			t.Fatalf("starting backend = %v, want block", st.Backend)
		}
		if len(st.Enabled) != 3 {
			t.Fatalf("cycle = %v, want the detected three backends", st.Enabled)
		}
	})

	t.Run("tab cycles the full set from a forced block start", func(t *testing.T) {
		st := NewState(120, 40)
		st.SetEnabled([]Backend{BackendKitty, BackendSixel, BackendBlock})
		applyForcedBackend(&st, ModeBlock)

		want := []Backend{BackendKitty, BackendSixel, BackendBlock}
		for i, w := range want {
			if got := st.CycleBackend(); got != w {
				t.Fatalf("cycle step %d = %v, want %v", i+1, got, w)
			}
		}
	})

	t.Run("tab wraps in cycle order from a forced sixel start", func(t *testing.T) {
		st := NewState(120, 40)
		st.SetEnabled([]Backend{BackendKitty, BackendSixel, BackendBlock})
		applyForcedBackend(&st, ModeSixel)

		if st.Backend != BackendSixel {
			t.Fatalf("starting backend = %v, want sixel", st.Backend)
		}
		want := []Backend{BackendBlock, BackendKitty, BackendSixel}
		for i, w := range want {
			if got := st.CycleBackend(); got != w {
				t.Fatalf("cycle step %d = %v, want %v", i+1, got, w)
			}
		}
	})

	t.Run("auto leaves the detected cycle", func(t *testing.T) {
		st := NewState(120, 40)
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
