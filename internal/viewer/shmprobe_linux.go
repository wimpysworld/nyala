//go:build linux

package viewer

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// shmDir is where Linux exposes POSIX shared-memory objects. shm_open("/name")
// on the terminal side opens shmDir + "name", so the probe creates its object
// here directly.
const shmDir = "/dev/shm/"

// probePixel is the 1x1 f=24 probe payload: one black RGB pixel.
var probePixel = []byte{0, 0, 0}

// createProbeSHM creates the POSIX shared-memory object for the kitty shm
// transport probe and writes probePixel into it. It returns the object name
// for the escape payload, with the leading slash the graphics protocol
// expects. Creation fails where the probe cannot work anyway (a private or
// missing /dev/shm), so the caller treats an error as shm unsupported.
func createProbeSHM() (string, error) {
	name := fmt.Sprintf("/nyala-shm-probe-%d", os.Getpid())
	fd, err := unix.Open(shmDir+name[1:], unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return "", fmt.Errorf("create shm probe object: %w", err)
	}
	defer unix.Close(fd)
	if _, err := unix.Write(fd, probePixel); err != nil {
		unlinkProbeSHM(name)
		return "", fmt.Errorf("write shm probe pixel: %w", err)
	}
	return name, nil
}

// unlinkProbeSHM removes the probe object when the terminal has not consumed
// it. Kitty unlinks the object itself after reading, so a missing entry is the
// consumed case and not an error.
func unlinkProbeSHM(name string) {
	_ = unix.Unlink(shmDir + name[1:])
}
