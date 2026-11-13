package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClientDefaultsWhenMissing(t *testing.T) {
	c, err := LoadClient(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Policy.Fallback != "always" || len(c.Nodes) != 0 || c.Identity.Owner == "" {
		t.Fatalf("bad defaults: %+v", c)
	}
}

func TestClientExampleFromBrief(t *testing.T) {
	c, err := LoadClient(write(t, `
version: 1
identity:
  owner: ""
nodes:
  - name: tower
    ssh_host: tower
    docker_context: dol-tower
    priority: 100
    wake_on_lan:
      mac: ""
      broadcast: 192.168.1.255
local:
  docker_context: auto
  max_memory_gb: 8
policy:
  prefer: remote
  fallback: always
  health_timeout_ms: 2000
  lease_request_timeout_ms: 5000
  heartbeat_interval_s: 30
forwarding:
  auto_forward: false
sync:
  remote_root: /srv/dol
  respect_gitignore: true
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Nodes[0].DockerContext != "dol-tower" || c.Nodes[0].WakeOnLAN.Broadcast != "192.168.1.255" {
		t.Fatalf("%+v", c.Nodes[0])
	}
}

func TestClientUnknownKey(t *testing.T) {
	_, err := LoadClient(write(t, "version: 1\npolicy:\n  fallbak: never\n"))
	if err == nil || !strings.Contains(err.Error(), `unknown key "fallbak"`) || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("want helpful unknown-key error, got %v", err)
	}
}

func TestClientRejectsInjectionInSSHHost(t *testing.T) {
	for _, h := range []string{"-oProxyCommand=evil", "a b", "x;rm"} {
		_, err := LoadClient(write(t, "version: 1\nnodes:\n  - name: t\n    ssh_host: '"+h+"'\n"))
		if err == nil {
			t.Errorf("ssh_host %q should be rejected", h)
		}
	}
}

func TestNodeExampleFromBrief(t *testing.T) {
	c, err := LoadNode(write(t, `
version: 1
name: tower
socket: /run/docker-offload-lan/node.sock
data_dir: /var/lib/docker-offload-lan
capacity:
  provider: auto
  reserve_memory_gb: 4
  min_free_disk_gb: 30
  windows_memory_check: true
leases:
  heartbeat_grace_s: 600
  max_ttl_s: 86400
  max_active: 32
reaper:
  interval_s: 60
  reap_unlabeled_after_s: 0
kind:
  binary: /usr/local/bin/kind
  port_range: [16443, 16999]
  access_mode: tunnel
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind.PortRange != [2]int{16443, 16999} {
		t.Fatal(c.Kind.PortRange)
	}
}

func TestNodeValidation(t *testing.T) {
	for _, body := range []string{
		"version: 2\nname: t\n",
		"version: 1\nname: t\ncapacity:\n  provider: fake\n",
		"version: 1\nname: t\nkind:\n  port_range: [20000, 10000]\n",
		"version: 1\nname: t\nkind:\n  access_mode: public\n",
	} {
		if _, err := LoadNode(write(t, body)); err == nil {
			t.Errorf("expected error for:\n%s", body)
		}
	}
}

func TestSchemasUpToDate(t *testing.T) {
	s, err := Schemas()
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range s {
		got, err := os.ReadFile(filepath.Join("..", "..", "schemas", name))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("schemas/%s is stale: run `go generate ./internal/config`", name)
		}
	}
}
