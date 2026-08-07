package kitty

import "strconv"

const darwinSHMPrefix = "/ny-f-"

func darwinSHMName(pid int, seq uint64) string {
	return darwinSHMPrefix + strconv.Itoa(pid) + "-" + strconv.FormatUint(seq, 36)
}
