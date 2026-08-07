// Package posixshm creates mapped POSIX shared-memory objects on Darwin.
package posixshm

import (
	"fmt"
	"strings"
)

// MaxNameBytes is Darwin's maximum POSIX shared-memory object name length,
// including the leading slash.
const MaxNameBytes = 31

// ValidateName checks that name is valid for Darwin's shm_open. A valid name
// has one leading slash, at least one byte after it, no other slash, and no
// more than MaxNameBytes bytes.
func ValidateName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("posixshm: empty object name")
	case name[0] != '/':
		return fmt.Errorf("posixshm: object name %q must start with one slash", name)
	case len(name) == 1:
		return fmt.Errorf("posixshm: object name %q has no name after the slash", name)
	case strings.Contains(name[1:], "/"):
		return fmt.Errorf("posixshm: object name %q contains more than one slash", name)
	case strings.IndexByte(name, 0) >= 0:
		return fmt.Errorf("posixshm: object name contains a NUL byte")
	case len(name) > MaxNameBytes:
		return fmt.Errorf("posixshm: object name is %d bytes, maximum is %d", len(name), MaxNameBytes)
	default:
		return nil
	}
}
