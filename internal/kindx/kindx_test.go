package kindx

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
	"go.yaml.in/yaml/v3"
)

var update = flag.Bool("update", false, "rewrite golden files")

const userConfig = `kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  apiServerAddress: "10.0.0.1"
  podSubnet: "10.244.0.0/16"
kubeadmConfigPatches:
- |
  kind: InitConfiguration
  nodeRegistration:
    kubeletExtraArgs:
      node-labels: "ingress-ready=true"
nodes:
- role: control-plane
  extraPortMappings:
  - containerPort: 80
    hostPort: 8080
    listenAddress: "127.0.0.1"
    protocol: TCP
- role: worker
`

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/kindx -update)", err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s mismatch:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func TestGenerateConfigGolden(t *testing.T) {
	cases := []struct {
		name, user string
		opts       ConfigOptions
		warnings   int
	}{
		{"tunnel-empty.yaml", "", ConfigOptions{AccessMode: v1.AccessTunnel, Port: 16443}, 0},
		{"tunnel-user.yaml", userConfig, ConfigOptions{AccessMode: v1.AccessTunnel, Port: 16444}, 1},
		{"lan-empty.yaml", "", ConfigOptions{AccessMode: v1.AccessLAN, Port: 16445, CertSANs: []string{"192.168.1.50", "tower"}}, 0},
		{"lan-user.yaml", userConfig, ConfigOptions{AccessMode: v1.AccessLAN, Port: 16446, CertSANs: []string{"192.168.1.50", "tower"}}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, warns, err := GenerateConfig(c.user, c.opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(warns) != c.warnings {
				t.Fatalf("warnings: %v", warns)
			}
			golden(t, c.name, out)
			// Semantic checks independent of formatting.
			var doc struct {
				Networking struct {
					Address string `yaml:"apiServerAddress"`
					Port    int    `yaml:"apiServerPort"`
				} `yaml:"networking"`
				Patches []string `yaml:"kubeadmConfigPatches"`
			}
			if err := yaml.Unmarshal(out, &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Networking.Port != c.opts.Port {
				t.Fatalf("port %d", doc.Networking.Port)
			}
			if c.opts.AccessMode == v1.AccessLAN {
				if doc.Networking.Address != "0.0.0.0" {
					t.Fatal(doc.Networking.Address)
				}
				last := doc.Patches[len(doc.Patches)-1]
				var patch map[string]any
				if err := yaml.Unmarshal([]byte(last), &patch); err != nil {
					t.Fatal(err)
				}
				if _, hasAV := patch["apiVersion"]; hasAV || patch["kind"] != "ClusterConfiguration" {
					t.Fatalf("certSANs patch must be a ClusterConfiguration without apiVersion: %v", patch)
				}
				sans := patch["apiServer"].(map[string]any)["certSANs"].([]any)
				if !reflect.DeepEqual(sans, []any{"192.168.1.50", "tower"}) {
					t.Fatalf("sans %v", sans)
				}
			} else {
				if doc.Networking.Address != "127.0.0.1" {
					t.Fatal(doc.Networking.Address)
				}
				for _, p := range doc.Patches {
					if strings.Contains(p, "certSANs") {
						t.Fatal("tunnel mode must not add certSANs")
					}
				}
			}
		})
	}
}

func TestGenerateConfigRejectsBadInput(t *testing.T) {
	for _, bad := range []string{
		"kind: Pod\n",
		"apiVersion: kind.x-k8s.io/v1alpha3\n",
		"networking: [1]\n",
		"kubeadmConfigPatches: nope\n",
		"nodes:\n- extraPortMappings:\n  - containerPort: http\n    hostPort: 1\n",
		"a: 1\n---\nb: 2\n",
		"{{",
	} {
		if _, _, err := GenerateConfig(bad, ConfigOptions{Port: 16443}); v1.CodeOf(err) != v1.CodeBadRequest {
			t.Errorf("%q: want bad_request, got %v", bad, err)
		}
	}
	if _, _, err := GenerateConfig("", ConfigOptions{Port: 16443, AccessMode: "public"}); err == nil {
		t.Fatal("unknown access mode accepted")
	}
}

func TestPortMappings(t *testing.T) {
	pm, err := PortMappings([]byte(userConfig))
	if err != nil {
		t.Fatal(err)
	}
	want := []v1.PortMapping{{ContainerPort: 80, HostPort: 8080, ListenAddress: "127.0.0.1", Protocol: "TCP"}}
	if !reflect.DeepEqual(pm, want) {
		t.Fatalf("got %+v", pm)
	}
}

func TestClusterName(t *testing.T) {
	n, err := ClusterName("l-abcdefghij", "e2e")
	if err != nil || n != "dol-l-abcdefghij-e2e" {
		t.Fatal(n, err)
	}
	if id, ok := LeaseOfCluster(n); !ok || id != "l-abcdefghij" {
		t.Fatal(id, ok)
	}
	if _, ok := LeaseOfCluster("kind"); ok {
		t.Fatal("foreign cluster parsed as ours")
	}
	for _, c := range [][2]string{{"l-abcdefghij", "E2E"}, {"bad", "e2e"}, {"l-abcdefghij", "a_b"}, {"l-abcdefghij", strings.Repeat("a", 17)}} {
		if _, err := ClusterName(c[0], c[1]); v1.CodeOf(err) != v1.CodeBadRequest {
			t.Errorf("%v accepted", c)
		}
	}
}

func TestAllocatePort(t *testing.T) {
	p, err := AllocatePort(100, 105, map[int]bool{100: true, 101: true}, func(p int) bool { return p != 102 })
	if err != nil || p != 103 {
		t.Fatal(p, err)
	}
	if _, err := AllocatePort(100, 101, map[int]bool{100: true, 101: true}, nil); err == nil {
		t.Fatal("exhausted range must fail")
	}
}

type recRunner struct{ calls [][]string }

func (r *recRunner) Run(_ context.Context, args ...string) ([]byte, error) {
	r.calls = append(r.calls, args)
	if args[0] == "get" {
		return []byte("dol-l-abcdefghij-e2e\nkind\n"), nil
	}
	return nil, nil
}

func TestKindArgs(t *testing.T) {
	r := &recRunner{}
	k := &Kind{R: r}
	ctx := context.Background()
	_ = k.Create(ctx, "c", "/cfg", "/kc", 180e9)
	_ = k.Load(ctx, "c", []string{"a:1", "b:2"})
	_ = k.Delete(ctx, "c")
	cs, _ := k.Clusters(ctx)
	want := [][]string{
		{"create", "cluster", "--name", "c", "--config", "/cfg", "--kubeconfig", "/kc", "--wait", "180s"},
		{"load", "docker-image", "--name", "c", "a:1", "b:2"},
		{"delete", "cluster", "--name", "c"},
		{"get", "clusters"},
	}
	if !reflect.DeepEqual(r.calls, want) || len(cs) != 2 {
		t.Fatalf("%v %v", r.calls, cs)
	}
}
