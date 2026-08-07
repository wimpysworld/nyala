package posixshm

import (
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want bool
	}{
		{name: "/n", want: true},
		{name: "/" + strings.Repeat("n", MaxNameBytes-1), want: true},
		{name: "", want: false},
		{name: "nyala", want: false},
		{name: "/", want: false},
		{name: "//nyala", want: false},
		{name: "/nyala/frame", want: false},
		{name: "/nyala\x00frame", want: false},
		{name: "/" + strings.Repeat("n", MaxNameBytes), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := ValidateName(test.name); (err == nil) != test.want {
				t.Errorf("ValidateName(%q) error = %v, want valid %t", test.name, err, test.want)
			}
		})
	}
}

func TestValidateNameCountsBytes(t *testing.T) {
	t.Parallel()

	if err := ValidateName("/" + strings.Repeat("é", 15)); err != nil {
		t.Fatalf("ValidateName(31-byte UTF-8 name) = %v", err)
	}
	if err := ValidateName("/" + strings.Repeat("é", 16)); err == nil {
		t.Fatal("ValidateName(33-byte UTF-8 name) succeeded")
	}
}
