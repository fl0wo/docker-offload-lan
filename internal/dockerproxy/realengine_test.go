package dockerproxy_test

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/fl0wo/docker-offload-lan/internal/dockerproxy"
	"github.com/fl0wo/docker-offload-lan/internal/dockerproxy/proxytest"
	"github.com/fl0wo/docker-offload-lan/internal/sshx"
)

// TestRealEngine runs the ADR-0040 measurement and verification suite
// against a real local engine through the proxy. Gated: DOL_PROXY_REAL=1.
// DOL_PROXY_CONTEXT selects the docker context (default desktop-linux),
// DOL_PROXY_N the sample count, DOL_PROXY_BUILD_MB the build context size
// (default 200; 0 skips builds), DOL_PROXY_IMAGE a small image that already
// exists locally (default alpine:3.20; it is never pulled).
func TestRealEngine(t *testing.T) {
	if os.Getenv("DOL_PROXY_REAL") != "1" {
		t.Skip("set DOL_PROXY_REAL=1 to measure the proxy against a real local Docker engine")
	}
	name := env("DOL_PROXY_CONTEXT", "desktop-linux")
	out, err := exec.Command("docker", "context", "inspect", name, "--format", "{{.Endpoints.docker.Host}}").Output()
	if err != nil {
		t.Fatalf("docker context inspect %s: %v", name, err)
	}
	endpoint := strings.TrimSpace(string(out))
	dial, err := dockerproxy.EndpointDialer(sshx.Options{}, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	img := env("DOL_PROXY_IMAGE", "alpine:3.20")
	if err := exec.CommandContext(context.Background(), "docker", "--context", name, "image", "inspect", img).Run(); err != nil {
		t.Skipf("image %s is not present on %s (the suite never pulls)", img, name)
	}
	// Direct uses -H like the proxied path: `--context` alone costs the CLI
	// ~2 ms (context store lookup) and would flatter the proxy.
	direct := []string{"-H", endpoint}
	if os.Getenv("DOL_PROXY_DIRECT_CONTEXT") == "1" {
		direct = []string{"--context", name}
	}
	rep := proxytest.Run(t, proxytest.Target{
		Name: "local engine (" + name + ", " + endpoint + ")", Direct: direct,
		Dial: dial, Image: img, N: atoi(env("DOL_PROXY_N", "50")), BuildMB: atoi(env("DOL_PROXY_BUILD_MB", "200")),
		BuildRuns: atoi(env("DOL_PROXY_BUILD_RUNS", "3")),
	})
	t.Log("\n" + rep.Markdown())
	if p := os.Getenv("DOL_PROXY_REPORT"); p != "" {
		_ = os.WriteFile(p, []byte(rep.Markdown()), 0o600)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }
