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
		wantEnabled bool
		wantN       int
	}{
		{"unset disables", "", false, false, 0},
		{"zero disables", "0", true, false, 0},
		{"one enables count 1", "1", true, true, 1},
		{"thirty enables count 30", "30", true, true, 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseCaptureEnv(tc.value, tc.set)
			if err != nil {
				t.Fatalf("ParseCaptureEnv(%q, %v): unexpected error %v", tc.value, tc.set, err)
			}
			if got.Enabled != tc.wantEnabled {
				t.Errorf("ParseCaptureEnv(%q, %v): Enabled = %v, want %v", tc.value, tc.set, got.Enabled, tc.wantEnabled)
			}
			if got.N != tc.wantN {
				t.Errorf("ParseCaptureEnv(%q, %v): N = %d, want %d", tc.value, tc.set, got.N, tc.wantN)
			}
		})
	}
}

func TestParseCaptureEnvInvalid(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		set   bool
	}{
		{"negative", "-1", true},
		{"non-integer", "abc", true},
		{"empty after set", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCaptureEnv(tc.value, tc.set)
			if err == nil {
				t.Fatalf("ParseCaptureEnv(%q, %v): want an error, got nil", tc.value, tc.set)
			}
			if !strings.Contains(err.Error(), "NYALA_CAPTURE") {
				t.Errorf("ParseCaptureEnv(%q, %v): error = %q, want it to mention NYALA_CAPTURE", tc.value, tc.set, err)
			}
		})
	}
}
