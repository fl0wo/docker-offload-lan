package capacity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/config"
)

// DefaultDockerRoot is used when `docker info` cannot tell us DockerRootDir.
const DefaultDockerRoot = "/var/lib/docker"

// RootDirFunc returns Docker's data-root (docker info DockerRootDir).
type RootDirFunc func(ctx context.Context) (string, error)

// Linux reads /proc and statfs. Fields are injectable for tests.
type Linux struct {
	// ProcDir is the procfs mount; default /proc.
	ProcDir string
	// RootDir resolves Docker's data-root; nil means DefaultDockerRoot.
	RootDir RootDirFunc
	// DiskFree returns free bytes of the filesystem holding path; nil means
	// statfs (unix only).
	DiskFree func(path string) (int64, error)

	mu   sync.Mutex
	root string // resolved data-root, cached after first success
}

// Name implements Provider.
func (*Linux) Name() string { return "linux" }

func (l *Linux) proc(name string) string {
	d := l.ProcDir
	if d == "" {
		d = "/proc"
	}
	return filepath.Join(d, name)
}

func (l *Linux) diskFree(path string) (int64, error) {
	if l.DiskFree != nil {
		return l.DiskFree(path)
	}
	return StatfsFree(path)
}

// dockerRoot returns the data-root, falling back to DefaultDockerRoot with a
// warning when docker info fails (the daemon may be restarting).
func (l *Linux) dockerRoot(ctx context.Context) (string, string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.root != "" {
		return l.root, ""
	}
	if l.RootDir == nil {
		return DefaultDockerRoot, ""
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	r, err := l.RootDir(cctx)
	if err != nil || r == "" {
		return DefaultDockerRoot, fmt.Sprintf("could not ask Docker for its data-root (%v); measuring disk at %s instead", err, DefaultDockerRoot)
	}
	l.root = r
	return r, ""
}

// Snapshot implements Provider.
func (l *Linux) Snapshot(ctx context.Context) (Snapshot, error) {
	var s Snapshot
	b, err := os.ReadFile(l.proc("meminfo"))
	if err != nil {
		return s, fmt.Errorf("reading memory info: %w (the linux capacity provider needs procfs; on other systems set capacity.provider)", err)
	}
	mi, err := ParseMeminfo(b)
	if err != nil {
		return s, err
	}
	s.MemTotal, s.MemAvailable = mi.Total, mi.Available

	root, warn := l.dockerRoot(ctx)
	if warn != "" {
		s.Warnings = append(s.Warnings, warn)
	}
	s.DiskPath = root
	free, err := l.diskFree(root)
	if err != nil {
		return s, fmt.Errorf("measuring free disk at %s: %w", root, err)
	}
	s.DiskFree = free

	s.CPUCores = runtime.NumCPU()
	if b, err := os.ReadFile(l.proc("loadavg")); err == nil {
		if v, err := ParseLoadavg(b); err == nil {
			s.Load1 = v
		}
	}
	return s, nil
}

// CommandFunc runs a program and returns its stdout. Injectable for tests.
type CommandFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

func execCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return out, fmt.Errorf("%w: %s", err, truncate(strings.TrimSpace(string(ee.Stderr)), 200))
		}
	}
	return out, err
}

// WindowsFreeMemoryArgs is the powershell invocation reading Windows free RAM
// in KiB.
var WindowsFreeMemoryArgs = []string{"-NoProfile", "-NonInteractive", "-Command",
	"(Get-CimInstance Win32_OperatingSystem).FreePhysicalMemory"}

// WSL2 is the Linux provider plus the Windows host's view: free space of the
// C: drive (/mnt/c) and, optionally, Windows free RAM through interop.
type WSL2 struct {
	Linux *Linux
	// HostDiskPath is where the Windows drive backing the VHD is mounted.
	HostDiskPath string
	// WindowsMemory enables the powershell.exe query.
	WindowsMemory bool
	// Command runs powershell.exe; nil means os/exec.
	Command CommandFunc
	// PowerShell is the interop binary; default "powershell.exe".
	PowerShell string
	// TTL caches the Windows memory value (powershell is slow to start).
	TTL time.Duration
	// Timeout bounds one powershell call.
	Timeout time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// InteropSockets lists candidate WSL interop sockets; nil means
	// DefaultInteropSockets. Empty result: run without touching WSL_INTEROP.
	InteropSockets func() []string

	mu       sync.Mutex
	interop  string // last socket that worked
	cachedAt time.Time
	winFree  int64
	winErr   error
}

// Name implements Provider.
func (*WSL2) Name() string { return "wsl2" }

func (w *WSL2) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// windowsFree returns Windows free RAM, cached for TTL (errors included, so a
// disabled interop does not cost a timeout on every request).
func (w *WSL2) windowsFree(ctx context.Context) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	ttl := w.TTL
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	if !w.cachedAt.IsZero() && w.now().Sub(w.cachedAt) < ttl {
		return w.winFree, w.winErr
	}
	timeout := w.Timeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	run := w.Command
	if run == nil {
		run = execCommand
	}
	ps := w.PowerShell
	if ps == "" {
		ps = FindPowerShell(exec.LookPath, fileExists)
	}
	// Which interop socket works depends on how the distro was started: when
	// a scheduled task (S4U logon) started it, the init's socket returns
	// nothing and only the task session's socket works (real Windows 10
	// tower). systemd services get no WSL_INTEROP at all. So try the last
	// working socket first, then every other one, and remember the winner.
	list := w.InteropSockets
	if list == nil {
		list = DefaultInteropSockets
	}
	cands := orderInterop(w.interop, os.Getenv("WSL_INTEROP"), list())
	attempt := func(sock string) (int64, error) {
		if sock != "" {
			_ = os.Setenv("WSL_INTEROP", sock)
		}
		out, err := run(cctx, ps, WindowsFreeMemoryArgs...)
		if err != nil {
			return 0, err
		}
		return ParseWindowsFreeKiB(out)
	}
	var v int64
	var err error
	if len(cands) == 0 {
		v, err = attempt("")
	}
	for _, c := range cands {
		if v, err = attempt(c); err == nil {
			w.interop = c
			break
		}
		if cctx.Err() != nil {
			break
		}
	}
	if cctx.Err() != nil && err != nil {
		err = fmt.Errorf("powershell.exe did not answer within %s", timeout)
	}
	w.winFree, w.winErr, w.cachedAt = v, err, w.now()
	return v, err
}

// Snapshot implements Provider.
func (w *WSL2) Snapshot(ctx context.Context) (Snapshot, error) {
	s, err := w.Linux.Snapshot(ctx)
	if err != nil {
		return s, err
	}
	hp := w.HostDiskPath
	if hp == "" {
		hp = "/mnt/c"
	}
	s.HostDiskPath = hp
	if free, err := w.Linux.diskFree(hp); err != nil {
		s.Warnings = append(s.Warnings, fmt.Sprintf(
			"cannot measure the Windows drive at %s (%v): inside WSL `df /` shows the virtual disk size, not real free space; check that C: is mounted (automount in /etc/wsl.conf)", hp, err))
	} else {
		s.HostDiskFree, s.HostDiskKnown = free, true
	}
	if w.WindowsMemory {
		if v, err := w.windowsFree(ctx); err != nil {
			s.Warnings = append(s.Warnings, fmt.Sprintf(
				"Windows free memory unavailable (%v); admission uses WSL memory only. Enable interop ([interop] enabled=true in /etc/wsl.conf) or set capacity.windows_memory_check: false", err))
		} else {
			s.HostMemFree, s.HostMemKnown = v, true
		}
	}
	return s, nil
}

// Fake is a deterministic, settable provider for tests and demos.
type Fake struct {
	mu   sync.Mutex
	snap Snapshot
	err  error
}

// NewFake returns a fake provider reporting s.
func NewFake(s Snapshot) *Fake { return &Fake{snap: s} }

// FakeFromConfig builds a fake from capacity.fake in the node config.
// Host values of 0 mean "not applicable" (like the linux provider).
func FakeFromConfig(fc *config.FakeCapacity) *Fake {
	gb := func(v float64) int64 { return int64(v * float64(1<<30)) }
	s := Snapshot{
		MemTotal:     gb(fc.TotalMemoryGB),
		MemAvailable: gb(fc.AvailableMemoryGB),
		DiskPath:     DefaultDockerRoot,
		DiskFree:     gb(fc.FreeDiskGB),
		CPUCores:     runtime.NumCPU(),
	}
	if fc.HostFreeMemoryGB > 0 {
		s.HostMemFree, s.HostMemKnown = gb(fc.HostFreeMemoryGB), true
	}
	if fc.HostFreeDiskGB > 0 {
		s.HostDiskPath, s.HostDiskFree, s.HostDiskKnown = "/mnt/c", gb(fc.HostFreeDiskGB), true
	}
	return NewFake(s)
}

// Name implements Provider.
func (*Fake) Name() string { return "fake" }

// Snapshot implements Provider.
func (f *Fake) Snapshot(context.Context) (Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.snap
	s.Warnings = append([]string(nil), f.snap.Warnings...)
	return s, f.err
}

// Set replaces the reported snapshot.
func (f *Fake) Set(s Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap = s
}

// Update mutates the reported snapshot in place.
func (f *Fake) Update(fn func(*Snapshot)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(&f.snap)
}

// SetErr makes Snapshot fail (nil clears it).
func (f *Fake) SetErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// Detect reports whether this machine is a WSL kernel.
func Detect(procDir string) (wsl bool) {
	if procDir == "" {
		procDir = "/proc"
	}
	_, err := os.Stat(filepath.Join(procDir, "sys/fs/binfmt_misc/WSLInterop"))
	v, _ := os.ReadFile(filepath.Join(procDir, "version"))
	return IsWSL(v, err == nil)
}

// New builds the provider selected by the node config. rootDir resolves
// Docker's data-root (may be nil).
func New(c config.CapacityConfig, rootDir RootDirFunc) (Provider, error) {
	lin := &Linux{RootDir: rootDir}
	wsl := func() Provider {
		return &WSL2{Linux: lin, HostDiskPath: "/mnt/c", WindowsMemory: c.WindowsMemoryCheck}
	}
	switch c.Provider {
	case "fake":
		if c.Fake == nil {
			return nil, errors.New("capacity.provider is fake but capacity.fake is not set")
		}
		return FakeFromConfig(c.Fake), nil
	case "linux":
		return lin, nil
	case "wsl2":
		return wsl(), nil
	case "auto", "":
		if runtime.GOOS != "linux" {
			return nil, fmt.Errorf("capacity.provider auto: %s nodes are not supported in v1 (only Linux/WSL2); set capacity.provider: fake to try the agent locally", runtime.GOOS)
		}
		if Detect("") {
			return wsl(), nil
		}
		return lin, nil
	case "macos-vm":
		return nil, errors.New("capacity.provider macos-vm is planned for v2; use linux, wsl2 or fake")
	}
	return nil, fmt.Errorf("unknown capacity.provider %q: use auto, linux, wsl2 or fake", c.Provider)
}

// DefaultWindowsPowerShell is powershell.exe under WSL's default automount
// root (/mnt/c).
const DefaultWindowsPowerShell = "/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe"

// FindPowerShell returns the interop powershell binary. Under systemd the
// dol-node service's PATH has no Windows directories (WSL appends them only
// to login shells), so a bare "powershell.exe" is not found there; fall back
// to the absolute path (found on a real Windows 10 tower).
func FindPowerShell(lookPath func(string) (string, error), exists func(string) bool) string {
	if p, err := lookPath("powershell.exe"); err == nil {
		return p
	}
	if exists(DefaultWindowsPowerShell) {
		return DefaultWindowsPowerShell
	}
	return "powershell.exe"
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// DefaultInteropSockets lists /run/WSL/*_interop sockets, session sockets
// (higher pids, created by wsl.exe launches) before the init's.
func DefaultInteropSockets() []string {
	m, _ := filepath.Glob("/run/WSL/*_interop")
	return m
}

// orderInterop returns candidates: last working, current env, then session
// sockets by descending pid, then low-pid (init) sockets; deduplicated.
func orderInterop(last, env string, found []string) []string {
	pid := func(p string) int {
		b := filepath.Base(p)
		n, _ := strconv.Atoi(strings.TrimSuffix(b, "_interop"))
		return n
	}
	rest := append([]string(nil), found...)
	sort.SliceStable(rest, func(i, j int) bool { return pid(rest[i]) > pid(rest[j]) })
	var out []string
	seen := map[string]bool{}
	for _, c := range append([]string{last, env}, rest...) {
		if c != "" && !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}
