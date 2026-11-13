//go:build !linux && !darwin

package capacity

import (
	"fmt"
	"runtime"
)

// StatfsFree is not implemented on this OS.
func StatfsFree(path string) (int64, error) {
	return 0, fmt.Errorf("measuring free disk is not supported on %s", runtime.GOOS)
}
