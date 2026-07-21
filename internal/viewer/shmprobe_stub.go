//go:build !linux

package viewer

import "errors"

// createProbeSHM reports that POSIX shared-memory probing is unavailable on
// this platform, so the handshake skips the shm transport query and resolves
// shm as unsupported.
func createProbeSHM() (string, error) {
	return "", errors.ErrUnsupported
}

// unlinkProbeSHM is a no-op on platforms without the shm probe.
func unlinkProbeSHM(string) {}
