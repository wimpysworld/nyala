package kitty

import (
	"math"
	"testing"

	"github.com/wimpysworld/nyala/internal/posixshm"
)

func TestDarwinSHMName(t *testing.T) {
	boundary := darwinSHMName(math.MaxInt32, math.MaxUint64)
	if len(boundary) > posixshm.MaxNameBytes {
		t.Fatalf("maximum name length = %d, want at most %d: %q", len(boundary), posixshm.MaxNameBytes, boundary)
	}
	if err := posixshm.ValidateName(boundary); err != nil {
		t.Fatalf("maximum name is invalid: %v", err)
	}
}
