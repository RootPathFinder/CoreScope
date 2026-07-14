//go:build !linux

package companion

import (
	"fmt"
)

// OpenSerial is only implemented on Linux (USB ACM hosts).
func OpenSerial(path string, baud int) (Port, error) {
	return nil, fmt.Errorf("companion serial open not supported on this platform (%s)", path)
}
