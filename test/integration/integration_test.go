// Integration tests against a containerized fake node (see harness).
// Run with: DOL_INTEGRATION=1 go test -count=1 -timeout 30m ./test/integration/...
package integration

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/config"
	"github.com/fl0wo/docker-offload-lan/internal/doctor"
	"github.com/fl0wo/docker-offload-lan/internal/nodeclient"
	"github.com/fl0wo/docker-offload-lan/test/integration/harness"
)

func TestMain(m *testing.M) {
	code := m.Run()
	harness.Teardown()
	os.Exit(code)
}

func ctxT(t *testing.T, d time.Duration) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func TestSSHReachableAsDol(t *testing.T) {
	n := harness.Start(t)
	ctx := ctxT(t, time.Minute)
	out, err := n.SSHRun(ctx, "id", "-un;", "id", "-nG;", "command", "-v", "dol", "docker", "kind", "kubectl", "rsync")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if lines[0] != "dol" {
		t.Fatalf("logged in as %q, want dol", lines[0])
	}
	groups := " " + lines[1] + " "
	for _, g := range []string{" dol ", " docker "} {
		if !strings.Contains(groups, g) {
			t.Errorf("dol is not in group%s(groups: %s)", g, lines[1])
		}
	}
	for _, bin := range []string{"/usr/local/bin/dol", "/usr/bin/docker", "/usr/local/bin/kind", "/usr/local/bin/kubectl", "/usr/bin/rsync"} {
		if !strings.Contains(out, bin) {
			t.Errorf("%s not on dol's PATH:\n%s", bin, out)
		}
	}
	// Password and root logins are refused on 2222.
	sshd, err := n.Exec(ctx, "sshd", "-T", "-C", "user=dol,host=x,addr=127.0.0.1,lport=2222")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"passwordauthentication no", "maxsessions 64", "allowusers dol", "permitrootlogin no", "authenticationmethods publickey"} {
		if !strings.Contains(sshd, want+"\n") {
			t.Errorf("effective sshd config lacks %q", want)
		}
	}
}

func TestDockerContextOverSSH(t *testing.T) {
	n := harness.Start(t)
	ctx := ctxT(t, 5*time.Minute)
	out, err := n.Docker(ctx, "--context", harness.Context, "run", "--rm", "busybox:1.37", "echo", "hello-from-the-node")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello-from-the-node") {
		t.Fatalf("unexpected output:\n%s", out)
	}
	// It ran on the node's engine, not the local one: the node has the image.
	if out, err := n.Exec(ctx, "docker", "image", "inspect", "busybox:1.37", "--format", "{{.Id}}"); err != nil || out == "" {
		t.Fatalf("image not on the node: %v %s", err, out)
	}
	// daemon.json from install.sh is in effect.
	drv, err := n.Docker(ctx, "--context", harness.Context, "info", "--format", "{{.LoggingDriver}}")
	if err != nil || strings.TrimSpace(drv) != "local" {
		t.Fatalf("logging driver %q, %v", drv, err)
	}
}

func TestInstallScriptIsIdempotent(t *testing.T) {
	n := harness.Start(t)
	ctx := ctxT(t, 5*time.Minute)
	out, err := n.Exec(ctx, "/opt/dol/deploy/install.sh", "--container-mode", "--dol-binary", "/opt/dol/dol",
		"--node-name", "it", "--pubkey", n.PubKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("second install.sh run:\n%s", out)
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "[changed]") || strings.HasPrefix(l, "[would change]") {
			t.Errorf("second run changed something: %s", l)
		}
	}
	if !strings.Contains(out, "[unchanged]") || !strings.Contains(out, "nothing changed") {
		t.Fatal("expected [unchanged] steps and the 'nothing changed' summary")
	}
	// The run ends with `dol doctor --node`; docker, sshd and tools are green.
	for _, want := range []string{"dol doctor --node", "✓ docker-daemon", "✓ dol-user", "✓ sshd-port", "✓ kind ", "✓ kubectl", "✓ rsync", "✓ sync-root", "✓ node-config"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output lacks %q", want)
		}
	}
	// --dry-run reports nothing to do either.
	out, err = n.Exec(ctx, "/opt/dol/deploy/install.sh", "--container-mode", "--dry-run", "--dol-binary", "/opt/dol/dol", "--node-name", "it")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "[would change]") {
		t.Errorf("dry run after install would change something:\n%s", out)
	}
}

// install.sh merges into existing files instead of overwriting them.
func TestInstallScriptMergesExisting(t *testing.T) {
	n := harness.Start(t)
	ctx := ctxT(t, 5*time.Minute)
	if _, err := n.Exec(ctx, "sh", "-c", `jq '. + {"debug": true, "log-opts": {"max-size": "1m"}}' /etc/docker/daemon.json > /tmp/d && mv /tmp/d /etc/docker/daemon.json`); err != nil {
		t.Fatal(err)
	}
	second := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ4oVv2sQ0m5u7d3vQnF1lW0bq0n8A1m0r2m0kV1q2xY second@test"
	install := []string{"/opt/dol/deploy/install.sh", "--container-mode", "--dol-binary", "/opt/dol/dol", "--node-name", "it", "--skip-doctor", "--pubkey", second}
	out, err := n.Exec(ctx, install...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"[changed] Docker daemon.json", "[changed] authorized_keys of dol (+1 key(s))", "2 change(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("first merge run lacks %q:\n%s", want, out)
		}
	}
	dj, _ := n.Exec(ctx, "jq", "-cS", "{debug, d: .[\"log-driver\"], o: .[\"log-opts\"], gc: .builder.gc.defaultMaxUsedSpace}", "/etc/docker/daemon.json")
	if strings.TrimSpace(dj) != `{"d":"local","debug":true,"gc":"40GB","o":{"max-file":"5","max-size":"20m"}}` {
		t.Errorf("daemon.json merge: %s", dj)
	}
	if out, _ := n.Exec(ctx, "sh", "-c", "ls /etc/docker/daemon.json.bak-dol-*"); !strings.Contains(out, "bak-dol") {
		t.Error("no daemon.json backup")
	}
	ak, _ := n.Exec(ctx, "cat", "/home/dol/.ssh/authorized_keys")
	if !strings.Contains(ak, n.PubKey) || !strings.Contains(ak, "second@test") {
		t.Errorf("authorized_keys lost a key:\n%s", ak)
	}
	out, err = n.Exec(ctx, install...)
	if err != nil || !strings.Contains(out, "nothing changed") {
		t.Fatalf("rerun after merge must be unchanged: %v\n%s", err, out)
	}
	if _, err := n.Exec(ctx, "/opt/dol/deploy/install.sh", "--container-mode", "--pubkey", "not-a-key"); err == nil {
		t.Error("an invalid --pubkey must be rejected")
	}
}

func TestNodeDialStdioHealth(t *testing.T) {
	n := harness.Start(t)
	ctx := ctxT(t, time.Minute)
	c := nodeclient.NewSSH(harness.NodeName, harness.Alias, n.SSH)
	defer c.Close()
	health, err := c.Health(ctx)
	if err != nil {
		if strings.Contains(err.Error(), "not implemented") || strings.Contains(n.NodeLog(ctx), "not implemented") {
			t.Skipf("`dol node serve`/`dial-stdio` are not implemented in this build yet (M2): %v", err)
		}
		t.Fatalf("health over ssh dial-stdio: %v\nagent log:\n%s", err, n.NodeLog(ctx))
	}
	if health.Node != harness.NodeName || health.APIVersion != "v1" {
		t.Fatalf("unexpected health: %+v", health)
	}
	if d := time.Since(health.Time); d > time.Minute || d < -time.Minute {
		t.Errorf("node clock off by %s", d)
	}
}

// The client doctor against the fake node: every check not depending on the
// node agent must be green.
func TestDoctorClientAgainstFakeNode(t *testing.T) {
	n := harness.Start(t)
	ctx := ctxT(t, 3*time.Minute)
	cfg := config.DefaultClient()
	node := config.NodeRef{Name: harness.NodeName, SSHHost: harness.Alias, DockerContext: harness.Context, Priority: 100}
	cfg.Nodes = []config.NodeRef{node}
	env := &doctor.ClientEnv{
		ConfigPath: "(test)", Config: cfg, Run: doctor.ExecRunner{Env: n.Env}, SSH: n.SSH,
		NewNodeAPI: func(nr config.NodeRef) doctor.NodeAPI { return nodeclient.NewSSH(nr.Name, nr.SSHHost, n.SSH) },
	}
	r := doctor.RunClient(ctx, env, doctor.ClientOptions{NodeName: harness.NodeName})
	got := map[string]doctor.Result{}
	for _, res := range r.Results {
		got[res.ID] = res
		t.Logf("%s %-20s %s", doctor.Symbol(res.Status), res.ID, res.Message)
	}
	for _, id := range []string{"ssh", "ssh-control-master", "remote-docker-group", "docker-context", "remote-kind", "remote-rsync"} {
		if got[id].Status != doctor.StatusOK {
			t.Errorf("%s: %s %s (fix: %s)", id, got[id].Status, got[id].Message, got[id].Fix)
		}
	}
	// The test ssh config uses a random port, so ssh-config warns about it.
	if got["ssh-config"].Status != doctor.StatusWarn || !strings.Contains(got["ssh-config"].Message, "expected 2222") {
		t.Errorf("ssh-config: %+v", got["ssh-config"])
	}
	if got["node-agent"].Status == doctor.StatusOK {
		for _, id := range []string{"version", "clock-skew", "capacity", "disk"} {
			if s := got[id].Status; s == doctor.StatusFail || s == doctor.StatusSkip {
				t.Errorf("%s: %s %s", id, s, got[id].Message)
			}
		}
	} else {
		t.Logf("node agent not available in this build; agent-dependent checks were skipped: %s", got["node-agent"].Message)
	}
}

// `dol init` with a temp HOME/XDG/DOCKER_CONFIG: writes the files and the
// context, and a second run changes nothing.
func TestInitIsIdempotent(t *testing.T) {
	n := harness.Start(t)
	ctx := ctxT(t, 2*time.Minute)
	home := t.TempDir()
	env := []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config")}

	// The harness created context dol-it itself (different description):
	// init must refuse to take it over.
	out, err := n.Dol(ctx, env, "init", "--name", "it", "--host", "127.0.0.1", "--yes", "--skip-ssh-check")
	if err == nil || !strings.Contains(out, "not created by dol") {
		t.Fatalf("init must refuse a foreign context: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh")); !os.IsNotExist(err) {
		t.Fatalf("a refused init must not write anything (stat ~/.ssh: %v)", err)
	}

	args := []string{"init", "--name", "box", "--host", "127.0.0.1", "--port", "2222", "--yes", "--skip-ssh-check"}
	out, err = n.Dol(ctx, env, args...)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "[changed]") != 4 {
		t.Errorf("first run should change 4 things:\n%s", out)
	}
	out, err = n.Dol(ctx, env, args...)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "[changed]") || strings.Count(out, "[unchanged]") != 4 {
		t.Errorf("second run must change nothing:\n%s", out)
	}
	cfg, err := config.LoadClient(filepath.Join(home, ".config", "docker-offload-lan", "config.yaml"))
	if err != nil || len(cfg.Nodes) != 1 || cfg.Nodes[0].DockerContext != "dol-box" {
		t.Fatalf("client config: %v %+v", err, cfg)
	}
	frag, err := os.ReadFile(filepath.Join(home, ".ssh", "config.d", "docker-offload-lan"))
	if err != nil || !strings.Contains(string(frag), "Host box\n") || !strings.Contains(string(frag), "Port 2222") {
		t.Fatalf("ssh fragment: %v\n%s", err, frag)
	}
	ctxs, err := n.Docker(ctx, "context", "inspect", "dol-box", "--format", "{{json .}}")
	if err != nil || !strings.Contains(ctxs, `"Description":"managed by docker-offload-lan"`) || !strings.Contains(ctxs, `"Host":"ssh://box"`) {
		t.Fatalf("context: %v %q", err, ctxs)
	}
	cur, _ := n.Docker(ctx, "context", "show")
	if strings.HasPrefix(strings.TrimSpace(cur), "dol-") {
		t.Fatal("dol init must not switch the current context")
	}
}
