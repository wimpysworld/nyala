package render

import "testing"

func TestParseBackend(t *testing.T) {
	for _, tc := range []struct {
		name string
		want Backend
	}{
		{name: "block", want: BackendBlock},
		{name: "sixel", want: BackendSixel},
		{name: "kitty", want: BackendKitty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseBackend(tc.name)
			if err != nil {
				t.Fatalf("ParseBackend(%q): %v", tc.name, err)
			}
			if got != tc.want {
				t.Fatalf("ParseBackend(%q) = %v, want %v", tc.name, got, tc.want)
			}
			if got.String() != tc.name {
				t.Fatalf("Backend.String() = %q, want %q", got.String(), tc.name)
			}
		})
	}
}

func TestParseBackendRejectsInvalidValues(t *testing.T) {
	for _, name := range []string{"", "auto", "bogus", "KITTY"} {
		if _, err := ParseBackend(name); err == nil {
			t.Fatalf("ParseBackend(%q) succeeded, want error", name)
		}
	}
	if got := Backend(99).String(); got != BackendBlock.String() {
		t.Fatalf("invalid Backend.String() = %q, want Block fallback %q", got, BackendBlock.String())
	}
}

func TestBackendCapabilities(t *testing.T) {
	for _, tc := range []struct {
		backend                               Backend
		format                                CaptureFormat
		text, direct, layout, quantisedScroll bool
	}{
		{BackendBlock, CaptureJPEG, true, false, true, true},
		{BackendSixel, CaptureJPEG, false, false, false, false},
		{BackendKitty, CapturePNG, false, true, false, false},
		{Backend(99), CaptureJPEG, true, false, true, true},
	} {
		if got := tc.backend.CaptureFormat(); got != tc.format {
			t.Errorf("%s capture format = %v, want %v", tc.backend, got, tc.format)
		}
		if got := tc.backend.UsesTextOverlay(); got != tc.text {
			t.Errorf("%s text overlay = %v, want %v", tc.backend, got, tc.text)
		}
		if got := tc.backend.UsesDirectPayload(); got != tc.direct {
			t.Errorf("%s direct payload = %v, want %v", tc.backend, got, tc.direct)
		}
		if got := tc.backend.UsesLayoutScaling(); got != tc.layout {
			t.Errorf("%s layout scaling = %v, want %v", tc.backend, got, tc.layout)
		}
		if got := tc.backend.QuantisesScroll(); got != tc.quantisedScroll {
			t.Errorf("%s quantised scroll = %v, want %v", tc.backend, got, tc.quantisedScroll)
		}
	}
}
