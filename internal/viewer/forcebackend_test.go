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
