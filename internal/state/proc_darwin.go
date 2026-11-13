//go:build darwin

package state

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// ProcessStartTime returns an opaque, stable start-time token for pid, read
// with sysctl kern.proc.pid (no CGO, no subprocess).
func ProcessStartTime(pid int) (string, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	if kp.Proc.P_pid != int32(pid) {
		return "", fmt.Errorf("process %d not found", pid)
	}
	tv := kp.Proc.P_starttime
	return fmt.Sprintf("%d.%06d", tv.Sec, tv.Usec), nil
}
