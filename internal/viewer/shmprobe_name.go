package viewer

import "fmt"

const darwinProbeSHMPrefix = "/ny-p-"

func probeSHMName(pid uint32, seq uint64) string {
	return fmt.Sprintf("%s%x-%x", darwinProbeSHMPrefix, pid, seq)
}
