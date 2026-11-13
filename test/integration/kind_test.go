package integration

// Golden path steps 4-5 (brief §15) with a real kind cluster inside the
// containerized node: kubectl on this machine reaches the API server through
// the ssh tunnel, images are built on the node and loaded with kind, and a
// service answers on localhost. Opt-in: DOL_INTEGRATION_KIND=1 (pulls the
// kindest/node image, ~1 GB, inside the fake node).

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
	"github.com/fl0wo/docker-offload-lan/test/integration/harness"
)

func TestGoldenPathKind(t *testing.T) {
	if os.Getenv("DOL_INTEGRATION_KIND") != "1" {
		t.Skip("set DOL_INTEGRATION_KIND=1 to run kind inside the fake node (~1 GB pull)")
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not installed on this machine")
	}
	n := harness.Start(t)
	ensureLocalContext(t, n)
	env, project := clientEnv(t, n, "")
	a := startAgent(t, n, env, project)

	acq := ok[v1.AcquireResult](t, a.call("dol_acquire", map[string]any{"workload": "kind", "est_memory_gb": 3, "project": "demo"}))
	if acq.Placement != v1.PlacementRemote {
		t.Fatalf("want remote, got %s: %s", acq.Placement, acq.Reason)
	}
	lease := acq.LeaseID

	start := time.Now()
	kc := ok[map[string]any](t, a.call("dol_kind_create", map[string]any{"lease_id": lease, "name": "e2e"}))
	kubectl := kc["commands"].(map[string]any)["kubectl"].(string)
	t.Logf("kind cluster %v ready in %s; kubectl prefix: %s", kc["cluster"], time.Since(start).Round(time.Second), kubectl)

	// 4. kubectl from this machine through the tunnel.
	out := a.mustSh(kubectl + " get nodes -o wide")
	t.Logf("kubectl get nodes:\n%s", out)
	if !strings.Contains(out, "control-plane") || !strings.Contains(out, "Ready") {
		t.Fatalf("cluster not ready:\n%s", out)
	}

	// 5. Build a service with the returned docker prefix (built on the node),
	// load it with dol_kind_load, deploy, reach it on localhost.
	svc := filepath.Join(project, "svc")
	if err := os.MkdirAll(svc, 0o755); err != nil {
		t.Fatal(err)
	}
	dockerfile := fmt.Sprintf("FROM %s\nRUN mkdir -p /www && echo hello-from-kind > /www/index.html\nCMD [\"httpd\", \"-f\", \"-p\", \"8080\", \"-h\", \"/www\"]\n", httpdImage)
	if err := os.WriteFile(filepath.Join(svc, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	a.mustSh(fmt.Sprintf("%s build -t demo-svc:dev %s", acq.Commands.Docker, svc))
	if r := a.call("dol_kind_load", map[string]any{"lease_id": lease, "cluster": "e2e", "images": []string{"demo-svc:dev"}}); r.IsError {
		t.Fatal(text(r))
	}
	manifest := filepath.Join(project, "svc.yaml")
	if err := os.WriteFile(manifest, []byte(`apiVersion: apps/v1
kind: Deployment
metadata: {name: demo}
spec:
  replicas: 1
  selector: {matchLabels: {app: demo}}
  template:
    metadata: {labels: {app: demo}}
    spec:
      containers:
      - name: demo
        image: demo-svc:dev
        imagePullPolicy: Never
        ports: [{containerPort: 8080}]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	a.mustSh(kubectl + " apply -f " + manifest)
	a.mustSh(kubectl + " rollout status deploy/demo --timeout=180s")

	// kubectl port-forward runs on this machine and goes through the API
	// server tunnel.
	pf := exec.Command("sh", "-c", kubectl+" port-forward deploy/demo 18090:8080")
	pf.Env = append(os.Environ(), env...)
	if err := pf.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pf.Process.Kill(); _ = pf.Wait() }()
	if got := httpGet(t, "http://127.0.0.1:18090/"); !strings.Contains(got, "hello-from-kind") {
		t.Fatalf("unexpected body %q", got)
	}

	// 7. release removes the cluster.
	rel := ok[v1.ReleaseResult](t, a.call("dol_release", map[string]any{"lease_id": lease}))
	t.Logf("release removed kind clusters: %v", rel.Removed.KindClusters)
	out2, _ := n.Exec(t.Context(), "sh", "-c", "kind get clusters 2>&1; docker ps -a --format '{{.Names}}'")
	if strings.Contains(out2, "dol-"+lease) {
		t.Fatalf("kind cluster left after release:\n%s", out2)
	}
}
