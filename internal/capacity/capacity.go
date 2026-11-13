// Package capacity measures how much memory, disk and CPU a node has (brief
// §7.4). Providers return a Snapshot; admission control (internal/node)
// turns snapshots into accept/refuse decisions.
//
// Parsers are pure functions over file contents so they are tested on
// recorded samples on any OS; only statfs and file reads touch the system.
package capacity

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Snapshot is one capacity measurement. Byte values are exact bytes.
type Snapshot struct {
	// MemTotal and MemAvailable are the memory of the machine (or VM) Docker
	// runs on: MemTotal / MemAvailable from /proc/meminfo on Linux.
	MemTotal     int64
	MemAvailable int64

	// HostMemFree is free RAM of the host OS behind a VM (Windows for WSL2).
	// Only meaningful when HostMemKnown.
	HostMemFree  int64
	HostMemKnown bool

	// DiskPath is the filesystem Docker writes to (its data-root) and
	// DiskFree the bytes available to unprivileged writers there.
	DiskPath string
	DiskFree int64

	// HostDiskPath / HostDiskFree describe the host drive backing the VM disk
	// (Windows C: seen as /mnt/c on WSL2). Only meaningful when HostDiskKnown.
	HostDiskPath  string
	HostDiskFree  int64
	HostDiskKnown bool

	CPUCores int
	Load1    float64

	// Warnings describe degraded measurements (e.g. Windows interop off).
	Warnings []string
}

// Provider measures capacity.
type Provider interface {
	// Name is the provider id reported in /v1/healthz: linux, wsl2, fake.
	Name() string
	Snapshot(ctx context.Context) (Snapshot, error)
}

// Meminfo is the subset of /proc/meminfo we use, in bytes.
type Meminfo struct {
	Total     int64
	Available int64
}

// ParseMeminfo parses /proc/meminfo contents. Values are in kB (KiB).
func ParseMeminfo(b []byte) (Meminfo, error) {
	var m Meminfo
	var haveTotal, haveAvail bool
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			return m, fmt.Errorf("parsing /proc/meminfo %s: %w", key, err)
		}
		mult := int64(1)
		if len(f) > 1 && strings.EqualFold(f[1], "kB") {
			mult = 1024
		}
		switch key {
		case "MemTotal":
			m.Total, haveTotal = v*mult, true
		case "MemAvailable":
			m.Available, haveAvail = v*mult, true
		}
	}
	if err := sc.Err(); err != nil {
		return m, err
	}
	if !haveTotal || !haveAvail {
		return m, errors.New("/proc/meminfo lacks MemTotal or MemAvailable (kernel older than 3.14?)")
	}
	return m, nil
}

// ParseLoadavg returns the 1-minute load average from /proc/loadavg.
func ParseLoadavg(b []byte) (float64, error) {
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0, errors.New("/proc/loadavg is empty")
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parsing /proc/loadavg: %w", err)
	}
	return v, nil
}

// ParseWindowsFreeKiB parses the output of
// (Get-CimInstance Win32_OperatingSystem).FreePhysicalMemory, which is in
// KiB, and returns bytes.
func ParseWindowsFreeKiB(b []byte) (int64, error) {
	s := strings.TrimSpace(strings.TrimPrefix(string(b), "\xef\xbb\xbf"))
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("unexpected FreePhysicalMemory output %q", truncate(s, 60))
	}
	return v * 1024, nil
}

// IsWSL reports whether the kernel is a WSL kernel, from the presence of the
// WSLInterop binfmt entry or "microsoft" in /proc/version.
func IsWSL(procVersion []byte, interopExists bool) bool {
	return interopExists || strings.Contains(strings.ToLower(string(procVersion)), "microsoft")
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
