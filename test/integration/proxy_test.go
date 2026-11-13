package integration

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/dockerproxy"
	"github.com/fl0wo/docker-offload-lan/internal/dockerproxy/proxytest"
	"github.com/fl0wo/docker-offload-lan/test/integration/harness"
)

// TestDockerProxyOverSSH runs the ADR-0040 proxy suite against the fake
// node's engine over ssh (`ssh dol-it docker system dial-stdio`), comparing
// with the plain ssh:// docker context. DOL_PROXY_N / DOL_PROXY_BUILD_MB
// tune it like the local-engine variant.
func TestDockerProxyOverSSH(t *testing.T) {
	n := harness.Start(t)
	ctx := ctxT(t, 10*time.Minute)
	img := envOr("DOL_PROXY_IMAGE", "alpine:3.20")
	// The fake node has no internet-free way to get an image: copy a small
	// one from the local engine.
	save := exec.CommandContext(ctx, "docker", "save", img)
	load := exec.CommandContext(ctx, "docker", "--context", harness.Context, "load")
	load.Env = append(os.Environ(), n.Env...)
	pipe, err := save.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	load.Stdin = pipe
	if err := save.Start(); err != nil {
		t.Fatal(err)
	}
	if out, err := load.CombinedOutput(); err != nil {
		t.Skipf("loading %s into the fake node: %v: %s", img, err, out)
	}
	_ = save.Wait()
	if _, err := n.Exec(ctx, "mkdir", "-p", "/tmp/dps/sub"); err != nil {
		t.Fatal(err)
	}
	rep := proxytest.Run(t, proxytest.Target{
		Name:      "fake node over ssh (dial-stdio, ControlMaster)",
		Direct:    []string{"--context", harness.Context},
		Env:       n.Env,
		Dial:      dockerproxy.SSHDialer(n.SSH, harness.Alias),
		Image:     img,
		N:         atoiOr(os.Getenv("DOL_PROXY_N"), 50),
		BuildMB:   atoiOr(os.Getenv("DOL_PROXY_BUILD_MB"), 200),
		RemoteDir: "/tmp/dps",
	})
	t.Log("\n" + rep.Markdown())
	if p := os.Getenv("DOL_PROXY_REPORT"); p != "" {
		_ = os.WriteFile(p, []byte(rep.Markdown()), 0o600)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return def
}
