package app

import (
	"strings"
	"testing"
)

func TestParseCaptureEnv(t *testing.T) {
	for _, tc := range []struct {
		name        string
		value       string
		set         bool
		scroll      string
		scrollSet   bool
		settle      string
		settleSet   bool
		wantEnabled bool
		wantN       int
		wantScroll  int
		wantSettle  bool
	}{
		{"unset disables", "", false, "", false, "", false, false, 0, 0, false},
		{"zero disables", "0", true, "", false, "", false, false, 0, 0, false},
		{"one enables count 1", "1", true, "", false, "", false, true, 1, 0, true},
		{"thirty enables count 30", "30", true, "", false, "", false, true, 30, 0, true},
		{"scroll ignored off capture", "0", true, "600", true, "", false, false, 0, 0, false},
		{"scroll unset means zero", "1", true, "", false, "", false, true, 1, 0, true},
		{"scroll zero means zero", "1", true, "0", true, "", false, true, 1, 0, true},
		{"scroll positive sets px", "1", true, "600", true, "", false, true, 1, 600, true},
		{"settle unset defaults on", "1", true, "600", true, "", false, true, 1, 600, true},
		{"settle zero turns off", "1", true, "600", true, "0", true, true, 1, 600, false},
		{"settle one keeps on", "1", true, "600", true, "1", true, true, 1, 600, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseCaptureEnv(tc.value, tc.set, tc.scroll, tc.scrollSet, tc.settle, tc.settleSet)
			if err != nil {
				t.Fatalf("ParseCaptureEnv(%q, %v): unexpected error %v", tc.value, tc.set, err)
			}
			if got.Enabled != tc.wantEnabled {
				t.Errorf("ParseCaptureEnv(%q, %v): Enabled = %v, want %v", tc.value, tc.set, got.Enabled, tc.wantEnabled)
			}
			if got.N != tc.wantN {
				t.Errorf("ParseCaptureEnv(%q, %v): N = %d, want %d", tc.value, tc.set, got.N, tc.wantN)
			}
			if got.ScrollPx != tc.wantScroll {
				t.Errorf("ParseCaptureEnv(%q, %v, scroll %q): ScrollPx = %d, want %d", tc.value, tc.set, tc.scroll, got.ScrollPx, tc.wantScroll)
			}
			if got.ScrollSettle != tc.wantSettle {
				t.Errorf("ParseCaptureEnv(%q, settle %q): ScrollSettle = %v, want %v", tc.value, tc.settle, got.ScrollSettle, tc.wantSettle)
			}
		})
	}
}

func TestParseCaptureEnvInvalid(t *testing.T) {
	for _, tc := range []struct {
		name      string
		value     string
		set       bool
		scroll    string
		scrollSet bool
		wantMsg   string
	}{
		{"negative", "-1", true, "", false, "NYALA_CAPTURE"},
		{"non-integer", "abc", true, "", false, "NYALA_CAPTURE"},
		{"empty after set", "", true, "", false, "NYALA_CAPTURE"},
		{"scroll negative", "1", true, "-1", true, "NYALA_CAPTURE_SCROLL"},
		{"scroll non-integer", "1", true, "abc", true, "NYALA_CAPTURE_SCROLL"},
		{"scroll empty after set", "1", true, "", true, "NYALA_CAPTURE_SCROLL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCaptureEnv(tc.value, tc.set, tc.scroll, tc.scrollSet, "", false)
			if err == nil {
				t.Fatalf("ParseCaptureEnv(%q, %v): want an error, got nil", tc.value, tc.set)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("ParseCaptureEnv(%q, %v): error = %q, want it to mention %s", tc.value, tc.set, err, tc.wantMsg)
			}
		})
	}
}
