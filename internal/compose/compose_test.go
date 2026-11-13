package compose

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

const sample = `{
 "name": "demo",
 "networks": {"default": {"name": "demo_default"}, "ext": {"external": true, "name": "shared"}},
 "secrets": {"s1": {"name": "demo_s1", "file": "/proj/secret.txt"}},
 "services": {"web": {
   "image": "nginx", "labels": {"a": "b"},
   "ports": [{"host_ip": "127.0.0.1", "target": 80, "published": "8080", "protocol": "tcp"},
             {"target": 81, "published": "8081", "protocol": "tcp"}],
   "volumes": [
     {"type": "bind", "source": "/proj/data", "target": "/data"},
     {"type": "volume", "source": "cache", "target": "/cache"},
     {"type": "bind", "source": "/etc/hosts", "target": "/h"}]}},
 "volumes": {"cache": {"name": "demo_cache"}}
}`

func doc(t *testing.T) Doc {
	var d Doc
	if err := json.Unmarshal([]byte(sample), &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRewriteRejectsExternal(t *testing.T) {
	_, err := doc(t).Rewrite("/proj", "/srv/dol/l-a/proj", false)
	if v1.CodeOf(err) != v1.CodeBadRequest || !strings.Contains(err.Error(), "/etc/hosts") {
		t.Fatalf("want bad_request naming /etc/hosts, got %v", err)
	}
}

func TestRewriteAndLabels(t *testing.T) {
	d := doc(t)
	w, err := d.Rewrite("/proj", "/srv/dol/l-a/proj", true)
	if err != nil || len(w) != 1 {
		t.Fatal(err, w)
	}
	labels := v1.LeaseLabels("l-aaaaaaaaaa", "o", "s", "p")
	d.InjectLabels(labels)
	b, _ := d.Marshal()
	s := string(b)
	for _, want := range []string{`"source": "/srv/dol/l-a/proj/data"`, `"file": "/srv/dol/l-a/proj/secret.txt"`, `"source": "/etc/hosts"`, `"a": "b"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %s", want)
		}
	}
	svc := m(m(d["services"])["web"])
	if m(svc["labels"])[v1.LabelLease] != "l-aaaaaaaaaa" {
		t.Fatal("service labels")
	}
	if m(m(m(d["networks"])["default"])["labels"])[v1.LabelManaged] != "true" {
		t.Fatal("network labels")
	}
	if _, ok := m(m(d["networks"])["ext"])["labels"]; ok {
		t.Fatal("external network must not be labeled")
	}
	if m(m(m(d["volumes"])["cache"])["labels"])[v1.LabelManaged] != "true" {
		t.Fatal("volume labels")
	}
	ports, warns := d.PublishedPorts()
	if len(ports) != 2 || ports[0].Published != 8080 || len(warns) != 1 || !strings.Contains(warns[0], "8081") {
		t.Fatalf("%+v %v", ports, warns)
	}
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, _ io.Reader, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "docker", args...).Output()
}

// TestRealComposeConfig uses the real docker compose CLI (no daemon needed)
// to make sure our assumptions about its JSON hold, including macOS /tmp
// symlinks.
func TestRealComposeConfig(t *testing.T) {
	if exec.Command("docker", "compose", "version").Run() != nil {
		t.Skip("docker compose not installed")
	}
	dir := t.TempDir()
	b, _ := os.ReadFile("testdata/compose.yaml")
	os.WriteFile(filepath.Join(dir, "compose.yaml"), b, 0o644)
	os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("x"), 0o644)
	os.MkdirAll(filepath.Join(dir, "web"), 0o755)
	d, err := Render(context.Background(), execRunner{}, dir, "itest", []string{"compose.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != "itest" {
		t.Fatal(d.Name())
	}
	if _, err := d.Rewrite(dir, "/srv/dol/l-x/itest", false); err == nil {
		t.Fatal("expected /etc/hosts to be rejected")
	}
	if _, err := d.Rewrite(dir, "/srv/dol/l-x/itest", true); err != nil {
		t.Fatal(err)
	}
	out, _ := d.Marshal()
	if !strings.Contains(string(out), `"/srv/dol/l-x/itest/data"`) {
		t.Fatalf("data mount not rewritten:\n%s", out)
	}
	// The rewritten file must still be accepted by compose.
	rf := filepath.Join(t.TempDir(), "rendered.json")
	os.WriteFile(rf, out, 0o644)
	if o, err := exec.Command("docker", "compose", "-p", "itest", "-f", rf, "config", "-q").CombinedOutput(); err != nil {
		t.Fatalf("compose rejects rendered file: %v %s", err, o)
	}
}
