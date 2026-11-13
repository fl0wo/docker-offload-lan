package clientsetup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/config"
)

func paths(t *testing.T) SSHPaths {
	t.Helper()
	d := filepath.Join(t.TempDir(), ".ssh")
	return SSHPaths{Dir: d, Config: filepath.Join(d, "config"), Fragment: filepath.Join(d, "config.d", "docker-offload-lan")}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

var tower = HostSpec{Alias: "tower", HostName: "192.168.1.50", User: "dol", Port: 2222}

func TestUpsertHostBlockIdempotentAndReplaces(t *testing.T) {
	p := paths(t)
	c, err := UpsertHostBlock(p, tower)
	if err != nil || !c.Changed {
		t.Fatalf("first: %v %+v", err, c)
	}
	s := read(t, p.Fragment)
	for _, want := range []string{"Host tower\n", "HostName 192.168.1.50", "Port 2222", "User dol", "ControlMaster auto", "ControlPath ~/.ssh/cm-%C", "ControlPersist 10m", "ServerAliveInterval 15"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if fi, _ := os.Stat(p.Fragment); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode())
	}
	c, err = UpsertHostBlock(p, tower)
	if err != nil || c.Changed {
		t.Fatalf("second run must be unchanged: %v %+v", err, c)
	}
	// Another node is appended; changing tower's IP replaces only its block.
	if _, err := UpsertHostBlock(p, HostSpec{Alias: "box", HostName: "box.lan", User: "dol", Port: 2222}); err != nil {
		t.Fatal(err)
	}
	t2 := tower
	t2.HostName = "192.168.1.51"
	if c, err = UpsertHostBlock(p, t2); err != nil || !c.Changed {
		t.Fatalf("update: %v %+v", err, c)
	}
	s = read(t, p.Fragment)
	if strings.Count(s, "Host tower\n") != 1 || !strings.Contains(s, "192.168.1.51") || strings.Contains(s, "192.168.1.50") || !strings.Contains(s, "Host box\n") {
		t.Fatalf("bad replace:\n%s", s)
	}
}

func TestUpsertHostBlockRefusesUnmanagedAndInjection(t *testing.T) {
	p := paths(t)
	if err := os.MkdirAll(filepath.Dir(p.Fragment), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Fragment, []byte("Host tower\n  HostName x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := UpsertHostBlock(p, tower); err == nil || !strings.Contains(err.Error(), "outside dol's markers") {
		t.Fatalf("want refusal, got %v", err)
	}
	bad := tower
	bad.HostName = "1.2.3.4\n  ProxyCommand evil"
	if _, err := UpsertHostBlock(paths(t), bad); err == nil {
		t.Fatal("newline in host must be rejected")
	}
}

func TestEnsureIncludeTopAndBackup(t *testing.T) {
	p := paths(t)
	now := time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)
	// Missing config: created, no backup.
	c, err := EnsureInclude(p, now)
	if err != nil || !c.Changed || c.Backup != "" {
		t.Fatalf("%v %+v", err, c)
	}
	if c, _ = EnsureInclude(p, now); c.Changed {
		t.Fatal("second run must be unchanged")
	}

	p = paths(t)
	orig := "Host github.com\n  User git\n"
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Config, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = EnsureInclude(p, now)
	if err != nil || !c.Changed || c.Backup == "" {
		t.Fatalf("%v %+v", err, c)
	}
	if read(t, c.Backup) != orig {
		t.Fatal("backup must hold the original")
	}
	s := read(t, p.Config)
	if !strings.HasPrefix(s, "# Added by") || strings.Index(s, IncludeLine) > strings.Index(s, "Host github.com") || !strings.HasSuffix(s, orig) {
		t.Fatalf("include must be at the top, original kept:\n%s", s)
	}
}

func TestEnsureIncludeRecognizesExisting(t *testing.T) {
	for _, line := range []string{"Include config.d/*", "include ~/.ssh/config.d/*", "Include ~/.ssh/config.d/docker-offload-lan", `Include "config.d/*" other`} {
		t.Run(line, func(t *testing.T) {
			p := paths(t)
			home := filepath.Dir(p.Dir)
			t.Setenv("HOME", home)
			if err := os.MkdirAll(p.Dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p.Config, []byte(line+"\n\nHost x\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if c, err := EnsureInclude(p, time.Now()); err != nil || c.Changed {
				t.Fatalf("%v %+v", err, c)
			}
		})
	}
	// An Include inside a Host block does not count.
	p := paths(t)
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Config, []byte("Host x\n  Include config.d/*\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, _ := EnsureInclude(p, time.Now()); !c.Changed {
		t.Fatal("include scoped to a Host must not count")
	}
}

func TestUpsertClientNode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "docker-offload-lan", "config.yaml")
	n := config.NodeRef{Name: "tower", SSHHost: "tower", DockerContext: "dol-tower", Priority: 100}
	c, err := UpsertClientNode(path, n)
	if err != nil || !c.Changed {
		t.Fatalf("%v %+v", err, c)
	}
	cfg, err := config.LoadClient(path)
	if err != nil || len(cfg.Nodes) != 1 || cfg.Nodes[0] != n {
		t.Fatalf("%v %+v", err, cfg)
	}
	if !strings.Contains(read(t, path), "# docker-offload-lan client config") {
		t.Fatal("new file should carry the commented template")
	}
	if c, err = UpsertClientNode(path, n); err != nil || c.Changed {
		t.Fatalf("second run must be unchanged: %v %+v", err, c)
	}

	// Existing user file with comments and an extra node key: both survive.
	user := `# my settings
version: 1
nodes:
  - name: tower
    ssh_host: old-alias
    wake_on_lan:
      mac: "aa:bb:cc:dd:ee:ff"
policy:
  prefer: local # I like local
  fallback: always
  health_timeout_ms: 2000
  lease_request_timeout_ms: 5000
  heartbeat_interval_s: 30
`
	if err := os.WriteFile(path, []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, err = UpsertClientNode(path, n); err != nil || !c.Changed || c.Backup == "" {
		t.Fatalf("%v %+v", err, c)
	}
	s := read(t, path)
	for _, want := range []string{"# my settings", "# I like local", "aa:bb:cc:dd:ee:ff", "ssh_host: tower"} {
		if !strings.Contains(s, want) {
			t.Errorf("lost %q:\n%s", want, s)
		}
	}
	cfg, err = config.LoadClient(path)
	if err != nil || cfg.Nodes[0].WakeOnLAN == nil || cfg.Policy.Prefer != "local" {
		t.Fatalf("%v %+v", err, cfg)
	}

	// Adding a second node appends.
	if _, err := UpsertClientNode(path, config.NodeRef{Name: "box", SSHHost: "box"}); err != nil {
		t.Fatal(err)
	}
	if cfg, _ = config.LoadClient(path); len(cfg.Nodes) != 2 {
		t.Fatalf("%+v", cfg.Nodes)
	}

	// Invalid result is refused, file untouched.
	before := read(t, path)
	if _, err := UpsertClientNode(path, config.NodeRef{Name: "Bad Name", SSHHost: "x"}); err == nil {
		t.Fatal("invalid node must be refused")
	}
	if read(t, path) != before {
		t.Fatal("file changed on error")
	}
}

type fakeDocker struct {
	contexts map[string]string // name -> json
	calls    []string
}

func (f *fakeDocker) Run(_ context.Context, name string, args ...string) (string, string, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	switch {
	case len(args) >= 3 && args[0] == "context" && args[1] == "inspect":
		if j, ok := f.contexts[args[2]]; ok {
			return j, "", nil
		}
		return "", "context \"" + args[2] + "\" does not exist", errors.New("exit status 1")
	case len(args) >= 3 && args[0] == "context" && (args[1] == "create" || args[1] == "update"):
		return args[2] + "\n", "", nil
	}
	return "", "unexpected", errors.New("unexpected call")
}

func TestEnsureDockerContext(t *testing.T) {
	ctx := context.Background()
	f := &fakeDocker{contexts: map[string]string{}}
	c, err := EnsureDockerContext(ctx, f, "dol-tower", "tower")
	if err != nil || !c.Changed || !strings.Contains(f.calls[1], "context create dol-tower --description managed by docker-offload-lan --docker host=ssh://tower") {
		t.Fatalf("%v %+v %v", err, c, f.calls)
	}
	for _, call := range f.calls {
		if strings.Contains(call, "context use") {
			t.Fatal("must never switch the current context")
		}
	}
	f.contexts["dol-tower"] = `{"Metadata":{"Description":"managed by docker-offload-lan"},"Endpoints":{"docker":{"Host":"ssh://tower"}}}`
	if c, err = EnsureDockerContext(ctx, f, "dol-tower", "tower"); err != nil || c.Changed {
		t.Fatalf("unchanged expected: %v %+v", err, c)
	}
	if c, err = EnsureDockerContext(ctx, f, "dol-tower", "tower2"); err != nil || !c.Changed || !strings.Contains(f.calls[len(f.calls)-1], "context update dol-tower --docker host=ssh://tower2") {
		t.Fatalf("update expected: %v %+v %v", err, c, f.calls)
	}
	f.contexts["dol-x"] = `{"Metadata":{"Description":"mine"},"Endpoints":{"docker":{"Host":"ssh://elsewhere"}}}`
	if _, err = EnsureDockerContext(ctx, f, "dol-x", "x"); err == nil || !strings.Contains(err.Error(), "not created by dol") {
		t.Fatalf("foreign context must be refused: %v", err)
	}
}

func TestRenderHostBlockWithJump(t *testing.T) {
	h := HostSpec{Alias: "tower", HostName: "localhost", User: "dol", Port: 2222, IdentityFile: "/k", Jump: "dol-jump@192.0.2.10"}
	if err := h.Validate(); err != nil {
		t.Fatal(err)
	}
	b := RenderHostBlock(h)
	for _, want := range []string{"Host tower-jump\n    HostName 192.0.2.10\n    User dol-jump\n    IdentityFile \"/k\"",
		"Host tower\n    HostName localhost\n    Port 2222\n    User dol\n    ProxyJump tower-jump\n    HostKeyAlias dol-tower"} {
		if !strings.Contains(b, want) {
			t.Fatalf("missing %q in:\n%s", want, b)
		}
	}
	for _, bad := range []string{"x@y z", "-oProxyCommand=x@y", "user@host\nHost evil", "@host"} {
		if (HostSpec{Alias: "t", HostName: "localhost", User: "dol", Port: 22, Jump: bad}).Validate() == nil {
			t.Errorf("jump %q must be rejected", bad)
		}
	}
}
