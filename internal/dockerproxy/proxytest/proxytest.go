// Package proxytest measures and verifies the Docker API proxy against a real
// engine with the real docker CLI (ADR-0040). It is used by gated tests:
// internal/dockerproxy (DOL_PROXY_REAL=1, local engine) and test/integration
// (DOL_INTEGRATION=1, fake node over ssh).
//
// Every resource it creates carries the label dolproxy.spike=<run id> (direct)
// or the spike lease label (through the proxy) and is removed at the end.
package proxytest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/dockerproxy"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// Target is the engine under test.
type Target struct {
	Name string
	// Direct are the docker CLI global flags reaching the engine without the
	// proxy (e.g. --context desktop-linux).
	Direct []string
	// Env is added to every docker process (e.g. PATH with an ssh wrapper).
	Env []string
	// Dial reaches the engine; it is the proxy's upstream and the "direct"
	// path of the API-level latency measurement.
	Dial dockerproxy.DialFunc
	// Image is a small image that already exists on the engine (never
	// pulled by the suite unless PullImage).
	Image string
	// BuildMB is the size of the build context; 0 skips build measurements.
	BuildMB int
	// BuildRuns per mode and path.
	BuildRuns int
	// N is the number of samples for latency measurements.
	N int
	// RemoteDir is where the suite's project dir is "synced" to on the
	// engine side (bind rewrite target). Default: a temp dir on this host
	// (right for a local engine that shares the host filesystem).
	RemoteDir string
}

// Stat summarizes samples.
type Stat struct {
	N             int
	P50, P95, Avg time.Duration
}

func stat(d []time.Duration) Stat {
	s := slices.Clone(d)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	var sum time.Duration
	for _, x := range s {
		sum += x
	}
	pct := func(p float64) time.Duration { return s[min(len(s)-1, int(p*float64(len(s)-1)+0.5))] }
	return Stat{N: len(s), P50: pct(0.50), P95: pct(0.95), Avg: sum / time.Duration(len(s))}
}

// Row is one line of the report.
type Row struct {
	What           string
	Direct, Proxy  Stat
	Unit           string // "" = latency; "MB/s" = throughput (Avg holds the time)
	MB             float64
	DirectThroughp float64
	ProxyThroughp  float64
}

// Check is a correctness verification.
type Check struct {
	What string
	OK   bool
	Note string
}

// Report is the outcome of Run.
type Report struct {
	Target string
	Rows   []Row
	Checks []Check
}

// Markdown renders the report.
func (r Report) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "### %s\n\n| measurement | N | direct p50 | direct p95 | proxy p50 | proxy p95 | Δ p50 |\n|---|---|---|---|---|---|---|\n", r.Target)
	for _, row := range r.Rows {
		if row.Unit == "MB/s" {
			continue
		}
		fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %s | %+s |\n", row.What, row.Direct.N,
			ms(row.Direct.P50), ms(row.Direct.P95), ms(row.Proxy.P50), ms(row.Proxy.P95), ms(row.Proxy.P50-row.Direct.P50))
	}
	var tp []Row
	for _, row := range r.Rows {
		if row.Unit == "MB/s" {
			tp = append(tp, row)
		}
	}
	if len(tp) > 0 {
		b.WriteString("\n| build (wall time of `docker build`) | runs | direct p50 | direct avg | direct MB/s (avg) | proxy p50 | proxy avg | proxy MB/s (avg) |\n|---|---|---|---|---|---|---|---|\n")
		for _, row := range tp {
			fmt.Fprintf(&b, "| %s | %d | %s | %s | %.0f | %s | %s | %.0f |\n", row.What, row.Direct.N,
				sec(row.Direct.P50), sec(row.Direct.Avg), row.DirectThroughp, sec(row.Proxy.P50), sec(row.Proxy.Avg), row.ProxyThroughp)
		}
	}
	b.WriteString("\n| check | result | note |\n|---|---|---|\n")
	for _, c := range r.Checks {
		res := "PASS"
		if !c.OK {
			res = "FAIL"
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", c.What, res, strings.ReplaceAll(c.Note, "|", "/"))
	}
	return b.String()
}

func ms(d time.Duration) string  { return fmt.Sprintf("%.2f ms", float64(d)/1e6) }
func sec(d time.Duration) string { return fmt.Sprintf("%.2f s", d.Seconds()) }

type suite struct {
	t       *testing.T
	tg      Target
	run     string
	lease   string
	labels  map[string]string
	sock    string
	proxy   []string
	rep     *Report
	tmp     string
	remote  string
	images  []string
	volumes []string // created implicitly without labels; removed by name
	project string
}

// Run starts a proxy for tg, runs every measurement and check, cleans up and
// returns the report. Failures of checks are reported with t.Errorf.
func Run(t *testing.T, tg Target) Report {
	t.Helper()
	if tg.N == 0 {
		tg.N = 50
	}
	if tg.BuildRuns == 0 {
		tg.BuildRuns = 3
	}
	tmp, err := os.MkdirTemp("", "dps-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	s := &suite{t: t, tg: tg, tmp: tmp, rep: &Report{Target: tg.Name}}
	s.run = fmt.Sprintf("%08x", rand.Uint32())
	s.lease = "l-spike" + s.run[:5]
	s.labels = v1.LeaseLabels(s.lease, "spike@proxytest", "s-spike"+s.run[:5], "dolproxy-spike")
	s.project = "dolproxyspike" + s.run
	s.sock = filepath.Join(tmp, "p.sock")
	s.remote = tg.RemoteDir
	if s.remote == "" {
		s.remote = filepath.Join(tmp, "remote")
		_ = os.MkdirAll(filepath.Join(s.remote, "sub"), 0o755)
	}
	p, err := dockerproxy.New(dockerproxy.Options{
		Dial: tg.Dial, Labels: s.labels,
		PathMaps: []dockerproxy.PathMap{{Local: filepath.Join(tmp, "proj"), Remote: s.remote}},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := dockerproxy.Listen(s.sock, p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	s.proxy = []string{"-H", srv.DockerHost()}
	t.Cleanup(s.cleanup)

	s.apiLatency()
	s.cliLatency("docker ps", func(pfx []string, _ int) error { _, err := s.docker(pfx, nil, "ps", "-q"); return err })
	s.cliLatency("container create+rm", func(pfx []string, i int) error {
		id, err := s.docker(pfx, nil, "create", "--label", "dolproxy.spike="+s.run, tg.Image, "true")
		if err != nil {
			return err
		}
		_, err = s.docker(pfx, nil, "rm", strings.TrimSpace(id))
		return err
	})
	s.checkRunEcho()
	s.checkLabels()
	s.checkBindRewrite()
	s.checkAttachStdin()
	s.checkExec()
	s.checkLogsFollow()
	s.checkEvents()
	s.checkCompose()
	if tg.BuildMB > 0 {
		before := s.buildCacheIDs()
		defer s.pruneBuildCache(before)
		s.build("legacy builder (POST /build)", map[string]string{"DOCKER_BUILDKIT": "0"})
		s.build("BuildKit (buildx, /session + /grpc)", map[string]string{"DOCKER_BUILDKIT": "1"})
	}
	return *s.rep
}

func (s *suite) check(what string, ok bool, format string, a ...any) {
	note := fmt.Sprintf(format, a...)
	s.rep.Checks = append(s.rep.Checks, Check{What: what, OK: ok, Note: note})
	if !ok {
		s.t.Errorf("%s: %s: %s", s.tg.Name, what, note)
	}
}

// observe records a result that documents behavior (a known limitation)
// without failing the test.
func (s *suite) observe(what string, ok bool, format string, a ...any) {
	s.rep.Checks = append(s.rep.Checks, Check{What: what + " (observed, not required)", OK: ok, Note: fmt.Sprintf(format, a...)})
}

func (s *suite) cmd(pfx []string, env map[string]string, args ...string) *exec.Cmd {
	cmd := exec.Command("docker", append(slices.Clone(pfx), args...)...)
	cmd.Env = append(os.Environ(), s.tg.Env...)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func (s *suite) docker(pfx []string, stdin io.Reader, args ...string) (string, error) {
	cmd := s.cmd(pfx, nil, args...)
	cmd.Stdin = stdin
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return out.String(), fmt.Errorf("docker %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

func (s *suite) cleanup() {
	ctx := []string(nil)
	if len(s.tg.Direct) > 0 {
		ctx = s.tg.Direct
	}
	for _, f := range []string{"label=dolproxy.spike=" + s.run, v1.LeaseFilter(s.lease)} {
		if ids, _ := s.docker(ctx, nil, "ps", "-aq", "--filter", f); strings.TrimSpace(ids) != "" {
			_, _ = s.docker(ctx, nil, append([]string{"rm", "-f", "-v"}, strings.Fields(ids)...)...)
		}
		if ids, _ := s.docker(ctx, nil, "network", "ls", "-q", "--filter", f); strings.TrimSpace(ids) != "" {
			_, _ = s.docker(ctx, nil, append([]string{"network", "rm"}, strings.Fields(ids)...)...)
		}
		if ids, _ := s.docker(ctx, nil, "volume", "ls", "-q", "--filter", f); strings.TrimSpace(ids) != "" {
			_, _ = s.docker(ctx, nil, append([]string{"volume", "rm", "-f"}, strings.Fields(ids)...)...)
		}
	}
	for _, img := range s.images {
		_, _ = s.docker(ctx, nil, "rmi", "-f", img)
	}
	for _, v := range s.volumes {
		_, _ = s.docker(ctx, nil, "volume", "rm", "-f", v)
	}
}

// apiLatency measures raw API round trips (no CLI process start), so the
// proxy's own overhead is visible.
func (s *suite) apiLatency() {
	direct := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return s.tg.Dial(ctx) }}}
	viaProxy := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", s.sock)
	}}}
	defer direct.CloseIdleConnections()
	defer viaProxy.CloseIdleConnections()
	for _, path := range []string{"/_ping", "/containers/json?all=1"} {
		get := func(c *http.Client) (time.Duration, error) {
			t0 := time.Now()
			resp, err := c.Get("http://docker" + path)
			if err != nil {
				return 0, err
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			return time.Since(t0), nil
		}
		// warm up both (connection setup, ssh process start)
		_, _ = get(direct)
		_, _ = get(viaProxy)
		n := s.tg.N * 4
		var d, p []time.Duration
		for range n {
			x, err := get(direct)
			if err != nil {
				s.t.Fatalf("direct GET %s: %v", path, err)
			}
			d = append(d, x)
			y, err := get(viaProxy)
			if err != nil {
				s.t.Fatalf("proxy GET %s: %v", path, err)
			}
			p = append(p, y)
		}
		s.rep.Rows = append(s.rep.Rows, Row{What: "API GET " + path + " (keep-alive, no CLI)", Direct: stat(d), Proxy: stat(p)})
	}
}

func (s *suite) cliLatency(what string, fn func(pfx []string, i int) error) {
	_ = fn(s.tg.Direct, -1)
	_ = fn(s.proxy, -1)
	var d, p []time.Duration
	for i := range s.tg.N {
		t0 := time.Now()
		if err := fn(s.tg.Direct, i); err != nil {
			s.t.Fatalf("%s direct: %v", what, err)
		}
		d = append(d, time.Since(t0))
		t0 = time.Now()
		if err := fn(s.proxy, i); err != nil {
			s.t.Fatalf("%s proxy: %v", what, err)
		}
		p = append(p, time.Since(t0))
	}
	s.rep.Rows = append(s.rep.Rows, Row{What: "CLI " + what, Direct: stat(d), Proxy: stat(p)})
}

func (s *suite) checkRunEcho() {
	out, err := s.docker(s.proxy, nil, "run", "--rm", s.tg.Image, "echo", "hello from proxy")
	s.check("docker run --rm IMAGE echo", err == nil && out == "hello from proxy\n", "out=%q err=%v", out, err)
}

func (s *suite) inspectLabels(kind, name string) map[string]string {
	args := []string{"inspect", "--format", "{{json .Config.Labels}}", name}
	switch kind {
	case "network":
		args = []string{"network", "inspect", "--format", "{{json .Labels}}", name}
	case "volume":
		args = []string{"volume", "inspect", "--format", "{{json .Labels}}", name}
	case "image":
		args = []string{"image", "inspect", "--format", "{{json .Config.Labels}}", name}
	}
	out, err := s.docker(s.tg.Direct, nil, args...)
	if err != nil {
		s.t.Logf("inspect %s %s: %v", kind, name, err)
		return nil
	}
	var m map[string]string
	_ = json.Unmarshal([]byte(out), &m)
	return m
}

func (s *suite) hasLease(m map[string]string) bool {
	for k, v := range s.labels {
		if m[k] != v {
			return false
		}
	}
	return true
}

func (s *suite) checkLabels() {
	name := "dps-" + s.run + "-labels"
	_, err := s.docker(s.proxy, nil, "create", "--name", name, "--label", "app=web", s.tg.Image, "true")
	if err != nil {
		s.check("container labels injected", false, "%v", err)
		return
	}
	m := s.inspectLabels("container", name)
	s.check("container labels injected (docker inspect), user label kept", s.hasLease(m) && m["app"] == "web", "labels=%v", m)
	net := "dps-" + s.run + "-net"
	vol := "dps-" + s.run + "-vol"
	_, err1 := s.docker(s.proxy, nil, "network", "create", "--label", "team=x", net)
	_, err2 := s.docker(s.proxy, nil, "volume", "create", "--label", "team=x", vol)
	nm, vm := s.inspectLabels("network", net), s.inspectLabels("volume", vol)
	s.check("network + volume labels injected", err1 == nil && err2 == nil && s.hasLease(nm) && s.hasLease(vm) && nm["team"] == "x" && vm["team"] == "x",
		"net=%v vol=%v errs=%v %v", nm, vm, err1, err2)
	// A dol-managed resource is visible with the lease filter (what the
	// reaper and dol_list use).
	out, _ := s.docker(s.tg.Direct, nil, "ps", "-a", "--filter", v1.LeaseFilter(s.lease), "--format", "{{.Names}}")
	s.check("lease filter finds proxied container", strings.Contains(out, name), "ps=%q", out)

	// Named volumes the engine creates implicitly on container create.
	mv, bv := "dps-"+s.run+"-mountvol", "dps-"+s.run+"-bindvol"
	s.volumes = append(s.volumes, mv, bv)
	_, err1 = s.docker(s.proxy, nil, "create", "--mount", "type=volume,src="+mv+",dst=/x", s.tg.Image, "true")
	_, err2 = s.docker(s.proxy, nil, "create", "-v", bv+":/x", s.tg.Image, "true")
	m1, m2 := s.inspectLabels("volume", mv), s.inspectLabels("volume", bv)
	s.check("implicit volume of --mount type=volume labeled (VolumeOptions.Labels)", err1 == nil && s.hasLease(m1), "labels=%v err=%v", m1, err1)
	s.observe("implicit volume of -v name:/path labeled", err2 == nil && s.hasLease(m2), "labels=%v err=%v", m2, err2)
}

func (s *suite) checkBindRewrite() {
	// The suite maps <tmp>/proj → s.remote on the engine.
	proj := filepath.Join(s.tmp, "proj")
	_ = os.MkdirAll(filepath.Join(proj, "sub"), 0o755)
	name := "dps-" + s.run + "-bind"
	_, err := s.docker(s.proxy, nil, "create", "--name", name, "-v", filepath.Join(proj, "sub")+":/data:ro",
		"--mount", "type=bind,source="+proj+",target=/m", s.tg.Image, "true")
	if err != nil {
		s.check("bind mounts rewritten", false, "%v", err)
		return
	}
	out, _ := s.docker(s.tg.Direct, nil, "inspect", "--format", "{{json .HostConfig.Binds}} {{range .Mounts}}{{.Source}}->{{.Destination}} {{end}}", name)
	s.check("bind mounts rewritten (-v and --mount)", strings.Contains(out, `"`+s.remote+`/sub:/data:ro"`) && strings.Contains(out, s.remote+"->/m"), "%s", strings.TrimSpace(out))
	_, err = s.docker(s.proxy, nil, "create", "-v", "/etc:/x", s.tg.Image, "true")
	s.check("bind outside project refused", err != nil && strings.Contains(err.Error(), "outside the synced project"), "%v", err)
}

func (s *suite) checkAttachStdin() {
	data := randBytes(10 << 20)
	sum := sha256.Sum256(data)
	out, err := s.docker(s.proxy, bytes.NewReader(data), "run", "--rm", "-i", "--label", "dolproxy.spike="+s.run, s.tg.Image, "sha256sum")
	got := strings.Fields(out)
	s.check("docker run -i: 10 MiB stdin → sha256sum (attach, half-close)", err == nil && len(got) > 0 && got[0] == hex.EncodeToString(sum[:]),
		"out=%q err=%v", out, err)
	// Output direction: 10 MiB from the container, byte-exact.
	out, err = s.docker(s.proxy, nil, "run", "--rm", "--label", "dolproxy.spike="+s.run, s.tg.Image, "sh", "-c", "head -c 10485760 /dev/zero | tr '\\0' 'x'")
	s.check("docker run: 10 MiB stdout byte-exact (attach, multiplexed)", err == nil && len(out) == 10<<20 && strings.Count(out, "x") == 10<<20, "len=%d err=%v", len(out), err)
}

func (s *suite) checkExec() {
	name := "dps-" + s.run + "-exec"
	if _, err := s.docker(s.proxy, nil, "run", "-d", "--name", name, s.tg.Image, "sleep", "600"); err != nil {
		s.check("docker exec", false, "%v", err)
		return
	}
	data := randBytes(4 << 20)
	sum := sha256.Sum256(data)
	out, err := s.docker(s.proxy, bytes.NewReader(data), "exec", "-i", name, "sha256sum")
	got := strings.Fields(out)
	s.check("docker exec -i: 4 MiB stdin (no tty)", err == nil && len(got) > 0 && got[0] == hex.EncodeToString(sum[:]), "out=%q err=%v", out, err)
	out, err = s.docker(s.proxy, strings.NewReader("ping\n"), "exec", "-i", name, "cat")
	s.check("echo ping | docker exec -i cat (half-close keeps output)", err == nil && out == "ping\n", "out=%q err=%v", out, err)
	// TTY: allocate a pty for the CLI with script(1) (BSD and util-linux
	// syntaxes differ).
	var cmd *exec.Cmd
	dargs := append(slices.Clone(s.proxy), "exec", "-it", name, "sh", "-c", "tty; echo tty-ok")
	if _, err := exec.LookPath("script"); err == nil {
		if isBSDScript() {
			cmd = exec.Command("script", append([]string{"-q", "/dev/null", "docker"}, dargs...)...)
		} else {
			cmd = exec.Command("script", "-qec", "docker "+strings.Join(dargs, " "), "/dev/null")
		}
		cmd.Env = append(os.Environ(), s.tg.Env...)
		cmd.Stdin = strings.NewReader("")
		b, err := cmd.CombinedOutput()
		o := string(b)
		s.check("docker exec -it under a pty (tty stream)", err == nil && strings.Contains(o, "/dev/pts/") && strings.Contains(o, "tty-ok"), "out=%q err=%v", o, err)
	}
	// exit code propagation
	_, err = s.docker(s.proxy, nil, "exec", name, "sh", "-c", "exit 7")
	code := -1
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	}
	s.check("docker exec exit code propagated", code == 7, "code=%d", code)
}

func isBSDScript() bool {
	out, _ := exec.Command("uname").Output()
	return strings.TrimSpace(string(out)) == "Darwin"
}

func (s *suite) checkLogsFollow() {
	name := "dps-" + s.run + "-logs"
	if _, err := s.docker(s.proxy, nil, "run", "-d", "--name", name, s.tg.Image, "sh", "-c", "for i in 1 2 3 4 5; do echo line$i; sleep 0.3; done"); err != nil {
		s.check("docker logs -f", false, "%v", err)
		return
	}
	cmd := s.cmd(s.proxy, nil, "logs", "-f", name)
	stdout, _ := cmd.StdoutPipe()
	t0 := time.Now()
	if err := cmd.Start(); err != nil {
		s.check("docker logs -f", false, "%v", err)
		return
	}
	buf := make([]byte, 64)
	n, _ := io.ReadAtLeast(stdout, buf, 6)
	first := time.Since(t0)
	rest, _ := io.ReadAll(stdout)
	err := cmd.Wait()
	all := string(buf[:n]) + string(rest)
	s.check("docker logs -f streams and ends with the container", err == nil && strings.Count(all, "line") == 5 && first < 1200*time.Millisecond,
		"first line after %v, total %q, err=%v", first.Round(time.Millisecond), all, err)
}

func (s *suite) checkEvents() {
	since := time.Now().Add(-time.Second).Unix()
	name := "dps-" + s.run + "-ev"
	_, _ = s.docker(s.proxy, nil, "create", "--name", name, s.tg.Image, "true")
	out, err := s.docker(s.proxy, nil, "events", "--since", fmt.Sprint(since), "--until", fmt.Sprint(time.Now().Add(time.Second).Unix()),
		"--filter", v1.LeaseFilter(s.lease), "--format", "{{.Action}} {{.Actor.Attributes.name}}")
	s.check("docker events (streamed, filtered by lease label)", err == nil && strings.Contains(out, "create "+name), "out=%q err=%v", out, err)
}

func (s *suite) checkCompose() {
	dir := filepath.Join(s.tmp, "proj")
	_ = os.MkdirAll(dir, 0o755)
	yml := fmt.Sprintf(`services:
  app:
    image: %s
    command: ["sleep", "600"]
    labels: {app: web}
    networks: [backend]
    volumes: ["data:/data", "./sub:/sub:ro"]
networks:
  backend: {}
volumes:
  data: {}
`, s.tg.Image)
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(yml), 0o644); err != nil {
		s.t.Fatal(err)
	}
	base := append(slices.Clone(s.proxy), "compose", "--project-directory", dir, "-p", s.project)
	t0 := time.Now()
	_, err := s.docker(base, nil, "up", "-d", "--wait")
	up := time.Since(t0)
	if err != nil {
		s.check("docker compose up -d through the proxy", false, "%v", err)
		return
	}
	cid, _ := s.docker(s.tg.Direct, nil, "ps", "-q", "--filter", "label=com.docker.compose.project="+s.project)
	cm := s.inspectLabels("container", strings.TrimSpace(cid))
	nm := s.inspectLabels("network", s.project+"_backend")
	vm := s.inspectLabels("volume", s.project+"_data")
	binds, _ := s.docker(s.tg.Direct, nil, "inspect", "--format", "{{range .Mounts}}{{.Source}} {{end}}", strings.TrimSpace(cid))
	s.check(fmt.Sprintf("docker compose up -d (%.1fs): container/network/volume labeled, compose labels kept, ./sub bind rewritten", up.Seconds()),
		s.hasLease(cm) && cm["app"] == "web" && cm["com.docker.compose.project"] == s.project && s.hasLease(nm) && s.hasLease(vm) &&
			strings.Contains(binds, s.remote+"/sub"),
		"container=%v network=%v volume=%v mounts=%s", len(cm), nm, vm, strings.TrimSpace(binds))
	// A second `up` must not see a config drift (labels are deterministic).
	out, err := s.docker(base, nil, "up", "-d")
	_ = out
	cid2, _ := s.docker(s.tg.Direct, nil, "ps", "-q", "--filter", "label=com.docker.compose.project="+s.project)
	s.check("docker compose up again is a no-op (no recreate)", err == nil && strings.TrimSpace(cid2) == strings.TrimSpace(cid), "before=%s after=%s err=%v", strings.TrimSpace(cid), strings.TrimSpace(cid2), err)
	_, err = s.docker(base, nil, "down", "-v")
	left, _ := s.docker(s.tg.Direct, nil, "ps", "-aq", "--filter", "label=com.docker.compose.project="+s.project)
	s.check("docker compose down -v through the proxy", err == nil && strings.TrimSpace(left) == "", "left=%q err=%v", left, err)
}

type cacheRecord struct {
	ID          string `json:"ID"`
	Description string `json:"Description"`
	Size        string `json:"Size"`
}

func (s *suite) buildCacheRecords() []cacheRecord {
	out, err := s.docker(s.tg.Direct, nil, "buildx", "du", "--format", "{{json .}}")
	if err != nil {
		return nil
	}
	var recs []cacheRecord
	for l := range strings.Lines(out) {
		var r cacheRecord
		if json.Unmarshal([]byte(l), &r) == nil && r.ID != "" {
			recs = append(recs, r)
		}
	}
	return recs
}

func (s *suite) buildCacheIDs() map[string]bool {
	ids := map[string]bool{}
	for _, r := range s.buildCacheRecords() {
		ids[r.ID] = true
	}
	return ids
}

// pruneBuildCache removes BuildKit cache records created by the suite's
// builds, identified narrowly so that concurrent builds of other projects are
// never touched: new since the snapshot AND either the suite's COPY step or a
// context transfer at least as large as the suite's context. Tiny records
// (the Dockerfile transfer, a few KB) are left behind.
func (s *suite) pruneBuildCache(before map[string]bool) {
	if before == nil {
		return
	}
	for _, r := range s.buildCacheRecords() {
		if before[r.ID] {
			continue
		}
		mine := r.Description == "[1/1] COPY blob /blob"
		if r.Description == "local source for context" && strings.HasSuffix(r.Size, "MB") {
			var mb float64
			_, _ = fmt.Sscanf(strings.TrimSuffix(r.Size, "MB"), "%g", &mb)
			mine = mb >= float64(s.tg.BuildMB)
		}
		if mine {
			_, _ = s.docker(s.tg.Direct, nil, "buildx", "prune", "-f", "--filter", "id="+r.ID)
		}
	}
}

func (s *suite) build(mode string, env map[string]string) {
	dir := filepath.Join(s.tmp, "build")
	_ = os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM scratch\nCOPY blob /blob\n"), 0o644); err != nil {
		s.t.Fatal(err)
	}
	mb := float64(s.tg.BuildMB)
	var d, p []time.Duration
	runOne := func(pfx []string, i int, viaProxy bool) (time.Duration, error) {
		// Fresh random content every run: no layer cache, no incremental
		// context sync.
		if err := os.WriteFile(filepath.Join(dir, "blob"), randBytes(s.tg.BuildMB<<20), 0o644); err != nil {
			return 0, err
		}
		tag := fmt.Sprintf("dolproxy-spike:%s-%s-%d-%v", s.run, env["DOCKER_BUILDKIT"], i, viaProxy)
		s.images = append(s.images, tag)
		cmd := s.cmd(pfx, env, "build", "-q", "--label", "dolproxy.spike="+s.run, "-t", tag, dir)
		var errb bytes.Buffer
		cmd.Stderr = &errb
		t0 := time.Now()
		err := cmd.Run()
		el := time.Since(t0)
		if err != nil {
			return 0, fmt.Errorf("%w: %s", err, strings.TrimSpace(errb.String()))
		}
		if viaProxy && i == 0 {
			m := s.inspectLabels("image", tag)
			if env["DOCKER_BUILDKIT"] == "0" {
				s.check("image labels injected via "+mode, s.hasLease(m) && m["dolproxy.spike"] == s.run, "labels=%v", m)
			} else {
				s.observe("image labels injected via "+mode, s.hasLease(m) && m["dolproxy.spike"] == s.run, "labels=%v", m)
			}
		}
		_, _ = s.docker(s.tg.Direct, nil, "rmi", "-f", tag)
		return el, nil
	}
	for i := range s.tg.BuildRuns {
		x, err := runOne(s.tg.Direct, i, false)
		if err != nil {
			s.check("docker build "+mode, false, "direct: %v", err)
			return
		}
		d = append(d, x)
		y, err := runOne(s.proxy, i, true)
		if err != nil {
			s.check("docker build "+mode+" through the proxy", false, "%v", err)
			return
		}
		p = append(p, y)
	}
	ds, ps := stat(d), stat(p)
	s.rep.Rows = append(s.rep.Rows, Row{What: fmt.Sprintf("%s, %d MiB context", mode, s.tg.BuildMB), Direct: ds, Proxy: ps, Unit: "MB/s", MB: mb,
		DirectThroughp: mb / ds.Avg.Seconds(), ProxyThroughp: mb / ps.Avg.Seconds()})
	s.check("docker build "+mode+" through the proxy", true, "")
}

func randBytes(n int) []byte {
	r := rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64()))
	b := make([]byte, n)
	for i := 0; i+8 <= n; i += 8 {
		v := r.Uint64()
		for j := range 8 {
			b[i+j] = byte(v >> (8 * j))
		}
	}
	return b
}
