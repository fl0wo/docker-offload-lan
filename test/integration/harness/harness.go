// Package harness runs a containerized "fake node" for integration tests:
// Ubuntu 24.04 provisioned by the real deploy/linux/install.sh
// (--container-mode), with Docker-in-Docker, sshd on 2222 and the dol agent
// (ADR-0035).
//
// Tests are gated by DOL_INTEGRATION=1 and need a local Docker engine that
// can run privileged containers. Nothing outside temp dirs is modified: the
// ssh config, key, known_hosts, ControlMaster sockets and DOCKER_CONFIG
// (contexts) all live in per-test temp dirs.
//
// Why a wrapper `ssh`: docker's ssh connhelper runs `ssh` from PATH and
// OpenSSH reads ~/.ssh/config from the passwd home directory, ignoring $HOME.
// So the harness puts a tiny `ssh` script first in PATH that adds `-F <test
// config>`; docker and dol then reach the node through the test config only.
package harness

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/sshx"
)

const (
	// Alias is the ssh Host alias of the fake node in the test ssh config.
	Alias = "dol-it"
	// Context is the docker context created in the test DOCKER_CONFIG.
	Context = "dol-it"
	// Image is the tag of the fake node image.
	Image = "docker-offload-lan-it-node:dev"
	// NodeName is the node name written to node.yaml in the image.
	NodeName = "it"
	// ContainerLabel marks containers started by the harness.
	ContainerLabel = "io.github.fl0wo.dol.integration=true"
)

// Enabled reports whether integration tests should run.
func Enabled() bool { return os.Getenv("DOL_INTEGRATION") == "1" }

// SkipUnlessEnabled skips t unless DOL_INTEGRATION=1.
func SkipUnlessEnabled(t testing.TB) {
	t.Helper()
	if !Enabled() {
		t.Skip("integration test: set DOL_INTEGRATION=1 (needs Docker that can run privileged containers)")
	}
}

// Node is a running fake node.
type Node struct {
	Container string
	Port      int // host port mapped to the container's 2222
	Dir       string
	KeyPath   string
	PubKey    string
	SSHConfig string
	// SSH makes sshx (and nodeclient.NewSSH) use the test ssh config.
	SSH sshx.Options
	// DockerConfig holds the test docker contexts.
	DockerConfig string
	// Env (PATH with the ssh wrapper first, DOCKER_CONFIG) for docker/dol
	// processes that must reach the node.
	Env []string
	// DolBinary is a host build of dol (for running CLI commands like init).
	DolBinary string

	t *testing.T
}

var (
	buildOnce sync.Once
	buildErr  error
	hostDol   string
)

// RepoRoot finds the module root (directory containing go.mod).
func RepoRoot() (string, error) {
	d, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, nil
		}
		p := filepath.Dir(d)
		if p == d {
			return "", fmt.Errorf("go.mod not found above the working directory")
		}
		d = p
	}
}

func run(ctx context.Context, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, tail(out.String(), 60))
	}
	return out.String(), nil
}

func tail(s string, n int) string {
	l := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(l) > n {
		l = l[len(l)-n:]
	}
	return strings.Join(l, "\n")
}

// dockerArch returns the GOARCH of the local Docker engine.
func dockerArch(ctx context.Context) (string, error) {
	out, err := run(ctx, nil, "docker", "info", "--format", "{{.Architecture}}")
	if err != nil {
		return "", err
	}
	switch a := strings.TrimSpace(out); a {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported docker architecture %q", a)
	}
}

// BuildImage cross-compiles dol for the engine's architecture and builds the
// fake node image (once per test binary; docker's layer cache makes rebuilds
// cheap). It also builds a host dol binary.
func BuildImage(t testing.TB) {
	t.Helper()
	buildOnce.Do(func() { buildErr = buildImage() })
	if buildErr != nil {
		t.Fatalf("building the fake node image: %v", buildErr)
	}
}

func buildImage() error {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	root, err := RepoRoot()
	if err != nil {
		return err
	}
	arch, err := dockerArch(ctx)
	if err != nil {
		return err
	}
	bctx, err := os.MkdirTemp("", "dol-it-build-")
	if err != nil {
		return err
	}
	// The build context is only needed during the build; the host binary is
	// kept in a separate dir for the whole test run (removed by the OS / CI).
	defer os.RemoveAll(bctx)
	if _, err := run(ctx, []string{"GOOS=linux", "GOARCH=" + arch, "CGO_ENABLED=0"},
		"go", "build", "-trimpath", "-o", filepath.Join(bctx, "dol"), filepath.Join(root, "cmd", "dol")); err != nil {
		return err
	}
	hostDir, err := os.MkdirTemp("", "dol-it-bin-")
	if err != nil {
		return err
	}
	hostDol = filepath.Join(hostDir, "dol")
	if _, err := run(ctx, []string{"CGO_ENABLED=0"}, "go", "build", "-o", hostDol, filepath.Join(root, "cmd", "dol")); err != nil {
		return err
	}
	for src, dst := range map[string]string{
		"test/integration/Dockerfile":    "Dockerfile",
		"test/integration/entrypoint.sh": "entrypoint.sh",
		"deploy/linux/install.sh":        "install.sh",
		"deploy/linux/dol-node.service":  "dol-node.service",
	} {
		b, err := os.ReadFile(filepath.Join(root, src))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(bctx, dst), b, 0o755); err != nil {
			return err
		}
	}
	_, err = run(ctx, nil, "docker", "build", "--progress=plain", "-t", Image, bctx)
	return err
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Start builds the image if needed, starts a fake node and registers
// cleanup. The node is ready for ssh and docker when Start returns.
func Start(t *testing.T) *Node {
	t.Helper()
	SkipUnlessEnabled(t)
	BuildImage(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Short base dir: ControlPath sockets must stay under the ~104 byte
	// unix socket path limit on macOS (t.TempDir() is too long there).
	dir, err := os.MkdirTemp("/tmp", "dolit-")
	if err != nil {
		t.Fatal(err)
	}
	n := &Node{Dir: dir, KeyPath: filepath.Join(dir, "id"), DolBinary: hostDol, t: t}
	t.Cleanup(n.cleanup)

	if _, err := run(ctx, nil, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", "dol-it", "-f", n.KeyPath); err != nil {
		t.Fatal(err)
	}
	pub, err := os.ReadFile(n.KeyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	n.PubKey = strings.TrimSpace(string(pub))

	name := "dol-it-" + randHex(4)
	if _, err := run(ctx, nil, "docker", "run", "-d", "--privileged", "--init", "--name", name,
		"--label", ContainerLabel, "-p", "127.0.0.1::2222", "-e", "DOL_PUBKEY="+n.PubKey, Image); err != nil {
		t.Fatal(err)
	}
	n.Container = name
	out, err := run(ctx, nil, "docker", "port", name, "2222/tcp")
	if err != nil {
		t.Fatal(err)
	}
	hp := firstLine(out)
	p, err := strconv.Atoi(hp[strings.LastIndexByte(hp, ':')+1:])
	if err != nil {
		t.Fatalf("parsing docker port output %q: %v", out, err)
	}
	n.Port = p

	n.SSHConfig = filepath.Join(dir, "ssh_config")
	cfg := fmt.Sprintf(`Host %s
  HostName 127.0.0.1
  Port %d
  User dol
  IdentityFile %s
  IdentitiesOnly yes
  IdentityAgent none
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  LogLevel ERROR
  ControlMaster auto
  ControlPath %s/cm-%%C
  ControlPersist 60
`, Alias, n.Port, n.KeyPath, dir)
	if err := os.WriteFile(n.SSHConfig, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	n.SSH = sshx.Options{ExtraArgs: []string{"-F", n.SSHConfig}, ConnectTimeout: 10 * time.Second}

	realSSH, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	wrapper := fmt.Sprintf("#!/bin/sh\nexec %q -F %q \"$@\"\n", realSSH, n.SSHConfig)
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	n.DockerConfig = filepath.Join(dir, "docker")
	n.Env = []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH"), "DOCKER_CONFIG=" + n.DockerConfig}

	if err := n.waitReady(ctx); err != nil {
		t.Fatalf("fake node did not become ready: %v\n%s", err, n.Logs())
	}
	if _, err := run(ctx, n.Env, "docker", "context", "create", Context,
		"--description", "docker-offload-lan integration test", "--docker", "host=ssh://"+Alias); err != nil {
		t.Fatal(err)
	}
	return n
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

func (n *Node) waitReady(ctx context.Context) error {
	var last error
	for ctx.Err() == nil {
		if _, err := run(ctx, nil, "docker", "exec", n.Container, "test", "-f", "/run/dol-it-ready"); err == nil {
			if _, err := n.SSHRun(ctx, "true"); err == nil {
				return nil
			} else {
				last = err
			}
		} else {
			last = err
			if st, _ := run(ctx, nil, "docker", "inspect", "-f", "{{.State.Running}}", n.Container); strings.TrimSpace(st) == "false" {
				return fmt.Errorf("container exited")
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timeout: %w", last)
}

// SSHRun runs a command on the node as dol through the test ssh config.
func (n *Node) SSHRun(ctx context.Context, remote ...string) (string, error) {
	out, err := n.SSH.Run(ctx, Alias, remote...)
	return string(out), err
}

// Exec runs a command in the container as root (docker exec).
func (n *Node) Exec(ctx context.Context, args ...string) (string, error) {
	return run(ctx, nil, "docker", append([]string{"exec", n.Container}, args...)...)
}

// Docker runs the docker CLI with the test DOCKER_CONFIG and ssh wrapper.
func (n *Node) Docker(ctx context.Context, args ...string) (string, error) {
	return run(ctx, n.Env, "docker", args...)
}

// Dol runs the host dol binary with extra env (appended to n.Env).
func (n *Node) Dol(ctx context.Context, env []string, args ...string) (string, error) {
	return run(ctx, append(append([]string{}, n.Env...), env...), n.DolBinary, args...)
}

// Logs returns the container's entrypoint and service logs.
func (n *Node) Logs() string {
	if n.Container == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var b strings.Builder
	out, _ := run(ctx, nil, "docker", "logs", n.Container)
	b.WriteString("--- docker logs ---\n" + tail(out, 40) + "\n")
	for _, f := range []string{"/var/log/dol-install.log", "/var/log/sshd.log", "/var/log/dol-node.log", "/var/log/dockerd.log"} {
		out, _ := run(ctx, nil, "docker", "exec", n.Container, "tail", "-n", "30", f)
		b.WriteString("--- " + f + " ---\n" + out + "\n")
	}
	return b.String()
}

// NodeLog returns the node agent log from the container.
func (n *Node) NodeLog(ctx context.Context) string {
	out, _ := n.Exec(ctx, "cat", "/var/log/dol-node.log")
	return out
}

func (n *Node) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if n.t.Failed() {
		n.t.Logf("fake node logs:\n%s", n.Logs())
	}
	if n.SSHConfig != "" {
		_, _ = run(ctx, nil, "ssh", "-F", n.SSHConfig, "-O", "exit", Alias)
	}
	if n.Container != "" && os.Getenv("DOL_IT_KEEP") != "1" {
		// -v removes the anonymous /var/lib/docker volume too.
		if _, err := run(ctx, nil, "docker", "rm", "-f", "-v", n.Container); err != nil {
			n.t.Logf("cleanup: %v", err)
		}
	}
	_ = os.RemoveAll(n.Dir)
}

// Teardown runs once after all tests (from TestMain): removes the host dol
// build and, with DOL_IT_RMI=1, the fake node image.
func Teardown() {
	if hostDol != "" {
		_ = os.RemoveAll(filepath.Dir(hostDol))
	}
	if os.Getenv("DOL_IT_RMI") != "1" || !Enabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, _ = run(ctx, nil, "docker", "rmi", "-f", Image)
}
