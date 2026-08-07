package viewer

import (
	"math"
	"strings"
	"testing"

	"github.com/wimpysworld/nyala/internal/posixshm"
)

func TestDarwinProbeSHMName(t *testing.T) {
	for _, test := range []struct {
		pid uint32
		seq uint64
	}{
		{pid: 1, seq: 1},
		{pid: math.MaxUint32, seq: math.MaxUint64},
	} {
		name := probeSHMName(test.pid, test.seq)
		if err := posixshm.ValidateName(name); err != nil {
			t.Errorf("probeSHMName(%d, %d) = %q: %v", test.pid, test.seq, name, err)
		}
		if len(name) > posixshm.MaxNameBytes {
			t.Errorf("probeSHMName(%d, %d) is %d bytes, maximum is %d", test.pid, test.seq, len(name), posixshm.MaxNameBytes)
		}
		if !strings.HasPrefix(name, "/") || strings.Contains(name[1:], "/") {
			t.Errorf("probeSHMName(%d, %d) = %q, want one leading slash", test.pid, test.seq, name)
		}
	}
}
