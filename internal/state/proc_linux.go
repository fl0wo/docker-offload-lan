//go:build linux

package state

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ProcessStartTime returns field 22 (starttime, clock ticks since boot) of
// /proc/<pid>/stat as an opaque token.
func ProcessStartTime(pid int) (string, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", err
	}
	// comm (field 2) may contain spaces and parentheses: split after the last ')'.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return "", fmt.Errorf("unexpected /proc/%d/stat format", pid)
	}
	fields := strings.Fields(s[i+1:])
	// fields[0] is field 3 (state); starttime is field 22 → index 19.
	if len(fields) < 20 {
		return "", fmt.Errorf("unexpected /proc/%d/stat format", pid)
	}
	return fields[19], nil
}
