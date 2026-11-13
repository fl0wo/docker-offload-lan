package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

const nodeYAML = `version: 1
name: tower
socket: /run/docker-offload-lan/node.sock
sync_root: /srv/dol
`

func nodeEnv(t *testing.T, wsl bool) (*NodeEnv, *fakeRunner) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "node.yaml")
	if err := os.WriteFile(cfg, []byte(nodeYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{}
	f.on(`^docker info`, `{"ServerVersion":"29.9.3","DockerRootDir":"/var/lib/docker","Architecture":"x86_64","LoggingDriver":"local"}`)
	f.on(`^id -nG dol`, "dol docker\n")
	f.on(`^systemctl is-active`, "active\n")
	f.on(`^systemctl is-enabled`, "enabled\n")
	f.on(`kind version`, "kind v0.33.0 go1.25.1 linux/amd64\n")
	f.on(`^kubectl version`, `{"clientVersion":{"gitVersion":"v1.37.2"}}`)
	f.on(`^rsync --version`, "rsync  version 3.2.7  protocol version 31\n")
	f.on(`^cmd.exe /c ver`, "\r\nMicrosoft Windows [Version 10.0.26100.1]\r\n")
	uid := os.Getuid()
	env := &NodeEnv{
		ConfigPath: cfg, Run: f,
		Stat: func(p string) (FileMode, error) {
			switch p {
			case "/run/docker-offload-lan/node.sock":
				return FileMode{Mode: os.ModeSocket | 0o660, GID: 1001, Exist: true}, nil
			case "/srv/dol":
				return FileMode{Mode: os.ModeDir | 0o750, UID: uid, Exist: true}, nil
			case "/proc/sys/fs/binfmt_misc/WSLInterop":
				return FileMode{Exist: wsl}, nil
			}
			return FileMode{}, nil
		},
		DiskFree: func(p string) (uint64, uint64, error) {
			return uint64(150 * v1.GiB), uint64(1000 * v1.GiB), nil
		},
		ListeningTCPPorts: func() (map[int]bool, error) { return map[int]bool{2222: true}, nil },
		IsWSL:             func() bool { return wsl },
		HasSystemd:        func() bool { return true },
		LookupGroupID:     func(string) (int, error) { return 1001, nil },
		NewNodeAPI: func(string) NodeAPI {
			return &fakeAPI{h: &v1.Health{Status: "ok", Version: "0.1.0", Provider: "wsl2", Accepting: true}}
		},
	}
	return env, f
}

func TestNodeAllGreenWSL(t *testing.T) {
	env, _ := nodeEnv(t, true)
	r := RunNode(context.Background(), env, NodeOptions{})
	for _, res := range r.Results {
		// sync-root owner check depends on a real "dol" user; accept fail only there.
		if res.ID == "sync-root" {
			continue
		}
		if res.Status != StatusOK {
			t.Errorf("%s: %s %s (fix %s)", res.ID, res.Status, res.Message, res.Fix)
		}
	}
	if len(r.Results) != len(nodeModeChecks) {
		t.Fatalf("got %d results", len(r.Results))
	}
}

func TestNodeNotWSLSkipsWSLChecks(t *testing.T) {
	env, _ := nodeEnv(t, false)
	got := byID(RunNode(context.Background(), env, NodeOptions{}))
	if got["disk-wsl-host"].Status != StatusSkip || got["wsl-interop"].Status != StatusSkip {
		t.Fatalf("%+v %+v", got["disk-wsl-host"], got["wsl-interop"])
	}
}

func TestNodeProblems(t *testing.T) {
	tests := []struct {
		name  string
		mod   func(e *NodeEnv, f *fakeRunner)
		check string
		want  Status
		text  string
	}{
		{"socket wrong mode", func(e *NodeEnv, _ *fakeRunner) {
			st := e.Stat
			e.Stat = func(p string) (FileMode, error) {
				if strings.HasSuffix(p, "node.sock") {
					return FileMode{Mode: os.ModeSocket | 0o666, GID: 0, Exist: true}, nil
				}
				return st(p)
			}
		}, "node-socket", StatusFail, "0666"},
		{"socket missing", func(e *NodeEnv, _ *fakeRunner) {
			st := e.Stat
			e.Stat = func(p string) (FileMode, error) {
				if strings.HasSuffix(p, "node.sock") {
					return FileMode{}, nil
				}
				return st(p)
			}
		}, "node-socket", StatusFail, "does not exist"},
		{"no systemd", func(e *NodeEnv, _ *fakeRunner) { e.HasSystemd = func() bool { return false } }, "systemd-unit", StatusSkip, "systemd"},
		{"unit failed", func(_ *NodeEnv, f *fakeRunner) {
			f.rules = append([]rule{{re: regexp.MustCompile(`^systemctl is-active`), stdout: "failed\n", err: errors.New("exit 3")}}, f.rules...)
		}, "systemd-unit", StatusFail, "failed"},
		{"sshd missing", func(e *NodeEnv, _ *fakeRunner) {
			e.ListeningTCPPorts = func() (map[int]bool, error) { return map[int]bool{22: true}, nil }
		}, "sshd-port", StatusFail, "2222"},
		{"sshd on 22 in wsl", func(e *NodeEnv, _ *fakeRunner) {
			e.ListeningTCPPorts = func() (map[int]bool, error) { return map[int]bool{22: true, 2222: true}, nil }
		}, "sshd-port", StatusWarn, "22"},
		{"kind old", func(_ *NodeEnv, f *fakeRunner) {
			f.rules = append([]rule{{re: regexp.MustCompile(`kind version`), stdout: "kind v0.20.0 go1.20 linux/amd64\n"}}, f.rules...)
		}, "kind", StatusWarn, "v0.20.0"},
		{"docker down", func(_ *NodeEnv, f *fakeRunner) {
			f.rules = append([]rule{{re: regexp.MustCompile(`^docker info`), stderr: "Cannot connect to the Docker daemon", err: errors.New("exit 1")}}, f.rules...)
		}, "docker-daemon", StatusFail, "Cannot connect"},
		{"log driver", func(_ *NodeEnv, f *fakeRunner) {
			f.rules = append([]rule{{re: regexp.MustCompile(`^docker info`), stdout: `{"ServerVersion":"29","LoggingDriver":"json-file"}`}}, f.rules...)
		}, "docker-daemon", StatusWarn, "size-capped"},
		{"no docker group", func(_ *NodeEnv, f *fakeRunner) {
			f.rules = append([]rule{{re: regexp.MustCompile(`^id -nG dol`), stdout: "dol\n"}}, f.rules...)
		}, "dol-user", StatusFail, "docker"},
		{"C: full", func(e *NodeEnv, _ *fakeRunner) {
			e.DiskFree = func(p string) (uint64, uint64, error) {
				if p == "/mnt/c" {
					return uint64(5 * v1.GiB), uint64(238 * v1.GiB), nil
				}
				return uint64(900 * v1.GiB), uint64(1000 * v1.GiB), nil
			}
		}, "disk-wsl-host", StatusFail, "refuses"},
		{"interop off", func(e *NodeEnv, _ *fakeRunner) {
			st := e.Stat
			e.Stat = func(p string) (FileMode, error) {
				if strings.Contains(p, "WSLInterop") {
					return FileMode{}, nil
				}
				return st(p)
			}
		}, "wsl-interop", StatusWarn, "disabled"},
		{"bad config", func(e *NodeEnv, _ *fakeRunner) { e.ConfigPath = "/nonexistent/node.yaml" }, "node-config", StatusFail, "node init"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, f := nodeEnv(t, true)
			tc.mod(env, f)
			got := byID(RunNode(context.Background(), env, NodeOptions{}))[tc.check]
			if got.Status != tc.want || !strings.Contains(got.Message+" "+got.Fix, tc.text) {
				t.Fatalf("%s: %s %q fix %q", tc.check, got.Status, got.Message, got.Fix)
			}
			if (got.Status == StatusFail || got.Status == StatusWarn) && got.Fix == "" {
				t.Fatalf("%s has no fix", tc.check)
			}
		})
	}
}

func TestParseProcNetTCP(t *testing.T) {
	in := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:08AE 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0 100 0 0 10 0
   1: 0100007F:0035 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2 1 0 100 0 0 10 0
   2: 0100007F:1F90 0100007F:C350 01 00000000:00000000 00:00000000 00000000     0        0 3 1 0 100 0 0 10 0
`
	got := parseProcNetTCP(strings.NewReader(in))
	if !got[2222] || !got[53] || got[8080] || len(got) != 2 {
		t.Fatalf("%v", got)
	}
}

// The pinned versions here and in install.sh must agree.
func TestPinsMatchInstallScript(t *testing.T) {
	b, err := os.ReadFile("../../deploy/linux/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`KIND_VERSION="` + KindVersion + `"`, `KUBECTL_VERSION="` + KubectlVersion + `"`, `SSH_PORT="2222"`, `DOL_USER="dol"`} {
		if !strings.Contains(s, want) {
			t.Errorf("install.sh lacks %s", want)
		}
	}
}
