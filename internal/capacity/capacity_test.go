package capacity

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/config"
)

func TestParseMeminfo(t *testing.T) {
	b, err := os.ReadFile("testdata/proc-linux/meminfo")
	if err != nil {
		t.Fatal(err)
	}
	m, err := ParseMeminfo(b)
	if err != nil {
		t.Fatal(err)
	}
	if m.Total != 32795132*1024 || m.Available != 20482048*1024 {
		t.Fatalf("got %+v", m)
	}
	if _, err := ParseMeminfo([]byte("MemTotal: 1 kB\n")); err == nil {
		t.Fatal("missing MemAvailable must be an error")
	}
	if _, err := ParseMeminfo([]byte("MemTotal: x kB\nMemAvailable: 1 kB\n")); err == nil {
		t.Fatal("garbage must be an error")
	}
}

func TestParseLoadavg(t *testing.T) {
	b, _ := os.ReadFile("testdata/proc-linux/loadavg")
	v, err := ParseLoadavg(b)
	if err != nil || v != 0.52 {
		t.Fatalf("got %v %v", v, err)
	}
	if _, err := ParseLoadavg(nil); err == nil {
		t.Fatal("empty must fail")
	}
}

func TestParseWindowsFreeKiB(t *testing.T) {
	// powershell.exe output through interop: BOM + CRLF.
	v, err := ParseWindowsFreeKiB([]byte("\xef\xbb\xbf31457280\r\n"))
	if err != nil || v != 31457280*1024 {
		t.Fatalf("got %v %v", v, err)
	}
	for _, bad := range []string{"", "Get-CimInstance : not found", "-5"} {
		if _, err := ParseWindowsFreeKiB([]byte(bad)); err == nil {
			t.Errorf("%q should fail", bad)
		}
	}
}

func TestDetectWSL(t *testing.T) {
	if !Detect("testdata/proc-wsl") {
		t.Fatal("WSL sample not detected")
	}
	if Detect("testdata/proc-linux") {
		t.Fatal("plain Linux detected as WSL")
	}
	if !IsWSL([]byte("Linux version 4.4.0-19041-Microsoft"), false) {
		t.Fatal("WSL1-style version string not detected")
	}
}

func TestLinuxProviderOnSamples(t *testing.T) {
	var asked string
	l := &Linux{
		ProcDir:  "testdata/proc-linux",
		RootDir:  func(context.Context) (string, error) { return "/data/docker", nil },
		DiskFree: func(p string) (int64, error) { asked = p; return 100 << 30, nil },
	}
	s, err := l.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if asked != "/data/docker" || s.DiskPath != "/data/docker" || s.DiskFree != 100<<30 {
		t.Fatalf("disk: %+v (asked %s)", s, asked)
	}
	if s.MemTotal != 32795132*1024 || s.Load1 != 0.52 || s.CPUCores != runtime.NumCPU() || s.HostMemKnown || s.HostDiskKnown {
		t.Fatalf("snapshot: %+v", s)
	}
}

func TestLinuxProviderFallsBackToDefaultRoot(t *testing.T) {
	var asked string
	l := &Linux{
		ProcDir:  "testdata/proc-linux",
		RootDir:  func(context.Context) (string, error) { return "", errors.New("daemon down") },
		DiskFree: func(p string) (int64, error) { asked = p; return 1, nil },
	}
	s, err := l.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if asked != DefaultDockerRoot || len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "daemon down") {
		t.Fatalf("asked %s warnings %v", asked, s.Warnings)
	}
}

func TestWSL2Provider(t *testing.T) {
	var calls atomic.Int32
	now := time.Unix(1000, 0)
	w := &WSL2{
		Linux: &Linux{ProcDir: "testdata/proc-wsl", DiskFree: func(p string) (int64, error) {
			if p == "/mnt/c" {
				return 50 << 30, nil
			}
			return 900 << 30, nil // the sparse VHD's misleading size
		}},
		WindowsMemory: true,
		Now:           func() time.Time { return now },
		Command: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls.Add(1)
			if name != "powershell.exe" || !strings.Contains(strings.Join(args, " "), "FreePhysicalMemory") {
				t.Errorf("unexpected command %s %v", name, args)
			}
			return []byte("\xef\xbb\xbf8388608\r\n"), nil // 8 GiB
		},
	}
	s, err := w.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !s.HostDiskKnown || s.HostDiskFree != 50<<30 || s.HostDiskPath != "/mnt/c" || s.DiskFree != 900<<30 {
		t.Fatalf("disk: %+v", s)
	}
	if !s.HostMemKnown || s.HostMemFree != 8<<30 {
		t.Fatalf("mem: %+v", s)
	}
	// Cached for TTL.
	now = now.Add(5 * time.Second)
	_, _ = w.Snapshot(context.Background())
	if calls.Load() != 1 {
		t.Fatalf("expected cache hit, got %d calls", calls.Load())
	}
	now = now.Add(6 * time.Second)
	_, _ = w.Snapshot(context.Background())
	if calls.Load() != 2 {
		t.Fatalf("expected refresh after TTL, got %d calls", calls.Load())
	}
}

func TestWSL2DegradesWithoutInterop(t *testing.T) {
	w := &WSL2{
		Linux:         &Linux{ProcDir: "testdata/proc-wsl", DiskFree: func(string) (int64, error) { return 1 << 30, nil }},
		WindowsMemory: true,
		Timeout:       50 * time.Millisecond,
		Command: func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
			<-ctx.Done() // hangs like a broken interop
			return nil, ctx.Err()
		},
	}
	s, err := w.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.HostMemKnown || len(s.Warnings) != 1 || !strings.Contains(s.Warnings[0], "Windows free memory unavailable") ||
		!strings.Contains(s.Warnings[0], "did not answer") {
		t.Fatalf("got %+v", s)
	}
}

func TestFake(t *testing.T) {
	f := FakeFromConfig(&config.FakeCapacity{TotalMemoryGB: 48, AvailableMemoryGB: 40, FreeDiskGB: 100, HostFreeMemoryGB: 20})
	s, _ := f.Snapshot(context.Background())
	if s.MemTotal != 48<<30 || !s.HostMemKnown || s.HostDiskKnown {
		t.Fatalf("got %+v", s)
	}
	f.Update(func(s *Snapshot) { s.MemAvailable = 1 })
	f.SetErr(errors.New("boom"))
	s, err := f.Snapshot(context.Background())
	if s.MemAvailable != 1 || err == nil {
		t.Fatal("update/err not applied")
	}
}

func TestNewSelectsProvider(t *testing.T) {
	p, err := New(config.CapacityConfig{Provider: "fake", Fake: &config.FakeCapacity{TotalMemoryGB: 1}}, nil)
	if err != nil || p.Name() != "fake" {
		t.Fatal(p, err)
	}
	if p, _ := New(config.CapacityConfig{Provider: "wsl2"}, nil); p.Name() != "wsl2" {
		t.Fatal(p.Name())
	}
	if _, err := New(config.CapacityConfig{Provider: "macos-vm"}, nil); err == nil {
		t.Fatal("macos-vm must be refused in v1")
	}
}

func TestStatfsFreeOnTempDir(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("statfs not supported here")
	}
	v, err := StatfsFree(t.TempDir())
	if err != nil || v <= 0 {
		t.Fatalf("got %d %v", v, err)
	}
}
