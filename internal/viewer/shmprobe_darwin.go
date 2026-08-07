//go:build darwin

package viewer

import (
	"errors"
	"fmt"
	"os"
	"sync/atomic"

	"github.com/wimpysworld/nyala/internal/posixshm"
	"golang.org/x/sys/unix"
)

const (
	darwinProbeSHMPrefix   = "/ny-p-"
	probeSHMCreateAttempts = 100
)

// probePixel is the 1x1 f=24 probe payload: one black RGB pixel.
var probePixel = []byte{0, 0, 0}

var probeSHMSeq atomic.Uint64

func probeSHMName(pid uint32, seq uint64) string {
	return fmt.Sprintf("%s%x-%x", darwinProbeSHMPrefix, pid, seq)
}

func createProbeSHMObject(name string) (*posixshm.Object, error) {
	obj, err := posixshm.Create(name, len(probePixel))
	if err != nil {
		return nil, fmt.Errorf("create shm probe object: %w", err)
	}
	copy(obj.Data(), probePixel)
	return obj, nil
}

// createProbeSHM creates the mapped POSIX shared-memory object for the Kitty
// shm transport probe and fills it with probePixel. It closes the mapping but
// leaves the name available for the terminal to open.
func createProbeSHM() (string, error) {
	return createProbeSHMWithAttempts(probeSHMCreateAttempts)
}

func createProbeSHMWithAttempts(attempts int) (string, error) {
	for range attempts {
		name := probeSHMName(uint32(os.Getpid()), probeSHMSeq.Add(1))
		if err := posixshm.ValidateName(name); err != nil {
			return "", fmt.Errorf("guard shm probe object name %s: %w", name, err)
		}
		obj, err := createProbeSHMObject(name)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", err
		}
		if err := obj.Close(); err != nil {
			return "", errors.Join(fmt.Errorf("close shm probe object: %w", err), posixshm.Unlink(name))
		}
		return name, nil
	}
	return "", fmt.Errorf("no free shm probe object name after %d attempts", attempts)
}

// unlinkProbeSHM removes the probe object when the terminal has not consumed
// it. A missing object means the terminal consumed it and is not an error.
func unlinkProbeSHM(name string) {
	_ = posixshm.Unlink(name)
}
