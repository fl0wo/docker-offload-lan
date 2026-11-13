// Package e2e runs the golden path (brief §15) on REAL hardware: this Mac and
// the configured node (e.g. the Windows tower). It uses your real dol config,
// ssh config and docker contexts, and only ever touches resources it creates
// (all labeled with its own lease).
//
//	DOL_E2E=1 go test -count=1 -v -timeout 40m ./test/e2e/
//
// Optional:
//
//	DOL_E2E_KIND=0            skip the kind steps (4-5)
//	DOL_E2E_REAP=1            run step 10 (waits heartbeat_grace + interval, ~11 min by default)
//	DOL_E2E_NODE_STOP="ssh admin@tower sudo systemctl stop dol-node"
//	DOL_E2E_NODE_START="ssh admin@tower sudo systemctl start dol-node"
//	                          run step 8 (the dol user cannot stop the service itself)
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

type agent struct {
	t   *testing.T
	cs  *mcp.ClientSession
	cmd *exec.Cmd
	dir string
}

func dolBinary(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("DOL_E2E_BINARY"); p != "" {
		return p
	}
	out := filepath.Join(t.TempDir(), "dol")
	b, err := exec.Command("go", "build", "-o", out, "../../cmd/dol").CombinedOutput()
	if err != nil {
		t.Fatalf("building dol: %v\n%s", err, b)
	}
	return out
}

func start(t *testing.T, bin, dir string) *agent {
	t.Helper()
	cmd := exec.Command(bin, "mcp")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CLAUDE_PROJECT_DIR="+dir)
	cmd.Stderr = os.Stderr
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "dol-e2e", Version: "1"}, nil).
		Connect(context.Background(), &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return &agent{t: t, cs: cs, cmd: cmd, dir: dir}
}

func (a *agent) call(name string, args map[string]any) *mcp.CallToolResult {
	a.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	r, err := a.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		a.t.Fatalf("%s: %v", name, err)
	}
	return r
}

func text(r *mcp.CallToolResult) string {
	var s []string
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			s = append(s, tc.Text)
		}
	}
	return strings.Join(s, "\n")
}

func ok[T any](t *testing.T, r *mcp.CallToolResult) T {
	t.Helper()
	if r.IsError {
		t.Fatalf("tool error: %s", text(r))
	}
	var out T
	b, _ := json.Marshal(r.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (a *agent) sh(cmdline string) string {
	a.t.Helper()
	c := exec.Command("sh", "-c", cmdline)
	c.Dir = a.dir
	out, err := c.CombinedOutput()
	if err != nil {
		a.t.Fatalf("%s: %v\n%s", cmdline, err, out)
	}
	return string(out)
}

func get(t *testing.T, url string) string {
	t.Helper()
	var last error
	for range 60 {
		resp, err := http.Get(url)
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == 200 {
				return string(b)
			}
			last = fmt.Errorf("HTTP %d", resp.StatusCode)
		} else {
			last = err
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("GET %s: %v", url, last)
	return ""
}

var pressureRe = regexp.MustCompile(`System-wide memory free percentage:\s*(\d+)%`)

// memFree returns macOS's system-wide memory free percentage (memory_pressure).
func memFree(t *testing.T) string {
	if runtime.GOOS != "darwin" {
		return "n/a"
	}
	out, err := exec.Command("memory_pressure", "-Q").CombinedOutput()
	if err != nil {
		return "unknown"
	}
	if m := pressureRe.FindSubmatch(out); m != nil {
		return string(m[1]) + "%"
	}
	return strings.TrimSpace(string(out))
}

func TestGoldenPathRealHardware(t *testing.T) {
	if os.Getenv("DOL_E2E") != "1" {
		t.Skip("real-hardware end-to-end test: set DOL_E2E=1 (uses your real dol config and node)")
	}
	bin := dolBinary(t)
	dir := t.TempDir()
	memBefore := memFree(t)

	// 1. doctor
	if out, err := exec.Command(bin, "doctor").CombinedOutput(); err != nil {
		t.Fatalf("step 1: dol doctor is not green: %v\n%s", err, out)
	}
	a := start(t, bin, dir)

	// 2. status
	st := ok[v1.StatusResult](t, a.call("dol_status", map[string]any{}))
	if len(st.Nodes) == 0 || !st.Nodes[0].Healthy {
		t.Fatalf("step 2: no healthy node: %s", text(a.call("dol_status", map[string]any{})))
	}
	t.Logf("step 2: %s", text(a.call("dol_status", map[string]any{})))

	// 3. acquire kind, 12 GB
	acq := ok[v1.AcquireResult](t, a.call("dol_acquire", map[string]any{"workload": "kind", "est_memory_gb": 12, "project": "demo"}))
	if acq.Placement != v1.PlacementRemote {
		t.Fatalf("step 3: want remote, got local: %s", acq.Reason)
	}
	lease := acq.LeaseID
	t.Logf("step 3: lease %s on %s (%s)", lease, acq.Node, acq.Commands.Docker)
	released := false
	t.Cleanup(func() {
		if !released {
			_ = a.call("dol_release", map[string]any{"lease_id": lease})
		}
	})

	if os.Getenv("DOL_E2E_KIND") != "0" {
		// 4. kind cluster, kubectl from the Mac
		kc := ok[map[string]any](t, a.call("dol_kind_create", map[string]any{"lease_id": lease, "name": "e2e"}))
		kubectl := kc["commands"].(map[string]any)["kubectl"].(string)
		out := a.sh(kubectl + " get nodes")
		if !strings.Contains(out, "Ready") {
			t.Fatalf("step 4: %s", out)
		}
		t.Logf("step 4: %s", out)

		// 5. two services built on the node, loaded, deployed, reached on localhost
		for i, name := range []string{"svc-a", "svc-b"} {
			d := filepath.Join(dir, name)
			_ = os.MkdirAll(d, 0o755)
			_ = os.WriteFile(filepath.Join(d, "Dockerfile"), []byte(fmt.Sprintf(
				"FROM busybox:1.37\nRUN mkdir -p /www && echo hello-%s > /www/index.html\nCMD [\"httpd\",\"-f\",\"-p\",\"8080\",\"-h\",\"/www\"]\n", name)), 0o644)
			a.sh(fmt.Sprintf("%s build -t %s:e2e %s", acq.Commands.Docker, name, d))
			if r := a.call("dol_kind_load", map[string]any{"lease_id": lease, "cluster": "e2e", "images": []string{name + ":e2e"}}); r.IsError {
				t.Fatal(text(r))
			}
			a.sh(fmt.Sprintf("%s create deployment %s --image=%s:e2e --port=8080 && %s patch deployment %s -p '{\"spec\":{\"template\":{\"spec\":{\"containers\":[{\"name\":\"%s\",\"imagePullPolicy\":\"Never\"}]}}}}' && %s rollout status deploy/%s --timeout=180s",
				kubectl, name, name, kubectl, name, name, kubectl, name))
			port := 18090 + i
			pf := exec.Command("sh", "-c", fmt.Sprintf("%s port-forward deploy/%s %d:8080", kubectl, name, port))
			if err := pf.Start(); err != nil {
				t.Fatal(err)
			}
			body := get(t, fmt.Sprintf("http://127.0.0.1:%d/", port))
			_ = pf.Process.Kill()
			_ = pf.Wait()
			if !strings.Contains(body, "hello-"+name) {
				t.Fatalf("step 5: %s returned %q", name, body)
			}
		}
		t.Log("step 5: both services answered 200 on localhost")
	}

	// 6. Mac memory pressure
	t.Logf("step 6: macOS memory free before %s, with workload running %s", memBefore, memFree(t))

	// 7. release → nothing left
	rel := ok[v1.ReleaseResult](t, a.call("dol_release", map[string]any{"lease_id": lease}))
	released = true
	t.Logf("step 7: %s", text(a.call("dol_status", map[string]any{})))
	out := a.sh(fmt.Sprintf("%s ps -a --filter label=%s=%s -q", acq.Commands.Docker, v1.LabelLease, lease))
	if strings.TrimSpace(out) != "" || len(rel.Removed.Errors) > 0 {
		t.Fatalf("step 7: resources left: %s %v", out, rel.Removed.Errors)
	}

	// 9. too big → 409 → local with numbers
	big := ok[v1.AcquireResult](t, a.call("dol_acquire", map[string]any{"workload": "container", "est_memory_gb": 100}))
	if big.Placement != v1.PlacementLocal || !strings.Contains(big.Reason, "GiB") {
		t.Fatalf("step 9: %s %s", big.Placement, big.Reason)
	}
	t.Logf("step 9: %s", big.Reason)
	ok[v1.ReleaseResult](t, a.call("dol_release", map[string]any{"lease_id": big.LeaseID}))

	// 8. node agent stopped → local within health_timeout + overhead
	if stop, startCmd := os.Getenv("DOL_E2E_NODE_STOP"), os.Getenv("DOL_E2E_NODE_START"); stop != "" && startCmd != "" {
		a.sh(stop)
		t.Cleanup(func() { _ = exec.Command("sh", "-c", startCmd).Run() })
		t0 := time.Now()
		down := ok[v1.AcquireResult](t, a.call("dol_acquire", map[string]any{"workload": "container", "est_memory_gb": 0.5}))
		took := time.Since(t0)
		a.sh(startCmd)
		if down.Placement != v1.PlacementLocal {
			t.Fatalf("step 8: want local, got %s", down.Placement)
		}
		t.Logf("step 8: local in %s: %s", took.Round(time.Millisecond), down.Reason)
		ok[v1.ReleaseResult](t, a.call("dol_release", map[string]any{"lease_id": down.LeaseID}))
	} else {
		t.Log("step 8: skipped (set DOL_E2E_NODE_STOP and DOL_E2E_NODE_START)")
	}

	// 10. abandoned session is reaped
	if os.Getenv("DOL_E2E_REAP") == "1" {
		b := start(t, bin, dir)
		l2 := ok[v1.AcquireResult](t, b.call("dol_acquire", map[string]any{"workload": "container", "est_memory_gb": 0.25}))
		if l2.Placement != v1.PlacementRemote {
			t.Fatalf("step 10: %s", l2.Reason)
		}
		b.sh(fmt.Sprintf("%s run -d %s busybox:1.37 sleep 7200", l2.Commands.Docker, l2.Commands.LabelFlags))
		_ = b.cmd.Process.Kill()
		t0 := time.Now()
		for time.Since(t0) < 20*time.Minute {
			if strings.TrimSpace(a.sh(fmt.Sprintf("%s ps -a --filter label=%s=%s -q", l2.Commands.Docker, v1.LabelLease, l2.LeaseID))) == "" {
				t.Logf("step 10: reaped %s after the session died", time.Since(t0).Round(time.Second))
				return
			}
			time.Sleep(15 * time.Second)
		}
		t.Fatal("step 10: not reaped within 20 minutes")
	} else {
		t.Log("step 10: skipped (set DOL_E2E_REAP=1; takes heartbeat_grace + reaper interval)")
	}
}
