package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/config"
	"github.com/fl0wo/docker-offload-lan/internal/sshx"
	"github.com/fl0wo/docker-offload-lan/internal/version"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// NodeAPI is the subset of nodeclient.Client the doctor uses.
type NodeAPI interface {
	Health(ctx context.Context) (*v1.Health, error)
	Capacity(ctx context.Context) (*v1.Capacity, error)
	Close()
}

// ClientEnv is everything client-mode checks touch.
type ClientEnv struct {
	ConfigPath string
	Config     *config.ClientConfig // nil if loading failed
	ConfigErr  error
	// Run runs local processes (ssh, docker, rsync).
	Run Runner
	// SSH builds ssh argv (ExtraArgs such as -F in tests).
	SSH sshx.Options
	// NewNodeAPI connects to a node's API (nodeclient.NewSSH in production).
	NewNodeAPI func(n config.NodeRef) NodeAPI
	// Now defaults to time.Now.
	Now func() time.Time
	// ClientVersion defaults to version.Version; GOARCH to runtime.GOARCH.
	ClientVersion string
	GOARCH        string
	// Stat checks local files (Homebrew rsync); defaults to os.Stat.
	Stat func(string) (os.FileInfo, error)
}

func (e *ClientEnv) defaults() {
	if e.Now == nil {
		e.Now = time.Now
	}
	if e.ClientVersion == "" {
		e.ClientVersion = version.Version
	}
	if e.GOARCH == "" {
		e.GOARCH = runtime.GOARCH
	}
	if e.Stat == nil {
		e.Stat = os.Stat
	}
	if e.Run == nil {
		e.Run = ExecRunner{}
	}
}

// ClientOptions select what to check.
type ClientOptions struct {
	NodeName string        // only this node ("" = all)
	Progress func(Result)  // called after each check (for streaming output)
	Timeout  time.Duration // per check; 0 = default
}

// clientState is shared by the checks of one node.
type clientState struct {
	env  *ClientEnv
	node config.NodeRef

	sshOK      bool
	sshUser    string
	api        NodeAPI
	health     *v1.Health
	healthRTT  time.Duration
	healthAt   time.Time
	capacity   *v1.Capacity
	dockerArch string // from `docker version` over the context, fallback for arch
}

type globalState struct{ env *ClientEnv }

// RunClient runs all client-mode checks.
func RunClient(ctx context.Context, env *ClientEnv, opts ClientOptions) *Report {
	env.defaults()
	r := &Report{Mode: "client"}
	g := &globalState{env: env}
	runTable(ctx, r, "", g, withTimeout(globalChecks, opts.Timeout), opts.Progress)
	if env.Config == nil {
		return r
	}
	found := false
	for _, n := range env.Config.Nodes {
		if opts.NodeName != "" && n.Name != opts.NodeName {
			continue
		}
		found = true
		s := &clientState{env: env, node: n}
		runTable(ctx, r, n.Name, s, withTimeout(nodeChecks, opts.Timeout), opts.Progress)
		if s.api != nil {
			s.api.Close()
		}
	}
	if opts.NodeName != "" && !found {
		res := Result{ID: "config", Title: "Node selected", Status: StatusFail,
			Message: fmt.Sprintf("node %q is not in %s", opts.NodeName, env.ConfigPath),
			Fix:     fmt.Sprintf("run `dol init --name %s --host <ip>` or pick one of: %s", opts.NodeName, nodeNames(env.Config))}
		r.add(res)
		if opts.Progress != nil {
			opts.Progress(res)
		}
	}
	return r
}

func withTimeout[S any](t []check[S], d time.Duration) []check[S] {
	if d <= 0 {
		return t
	}
	out := make([]check[S], len(t))
	for i, c := range t {
		c.Timeout = d
		out[i] = c
	}
	return out
}

func nodeNames(c *config.ClientConfig) string {
	var n []string
	for _, x := range c.Nodes {
		n = append(n, x.Name)
	}
	if len(n) == 0 {
		return "(none configured)"
	}
	return strings.Join(n, ", ")
}

var globalChecks = []check[*globalState]{
	{ID: "config", Title: "Client config", Run: checkConfig},
	{ID: "local-docker", Title: "Local Docker (fallback)", Run: checkLocalDocker},
	{ID: "local-rsync", Title: "rsync on this machine", Run: checkLocalRsync},
}

func checkConfig(_ context.Context, g *globalState) Outcome {
	e := g.env
	if e.ConfigErr != nil {
		return fail("fix the file (see schemas/client-config.schema.json) or move it away and run `dol init`", "%v", e.ConfigErr)
	}
	if e.Config == nil {
		return fail("run `dol init`", "no client config loaded")
	}
	if len(e.Config.Nodes) == 0 {
		return warn("run `dol init --name tower --host <node LAN IP>` to add a node",
			"%s: no nodes configured; everything will run on local Docker", e.ConfigPath)
	}
	return ok("%s: %d node(s): %s; owner %s", e.ConfigPath, len(e.Config.Nodes), nodeNames(e.Config), e.Config.Identity.Owner)
}

func checkLocalDocker(ctx context.Context, g *globalState) Outcome {
	e := g.env
	configured := "auto"
	if e.Config != nil {
		configured = e.Config.Local.DockerContext
	}
	name, err := DetectLocalContext(ctx, e.Run, configured)
	if err != nil {
		if isNotFound(err) || strings.Contains(err.Error(), "executable file not found") {
			return warn("install Docker Desktop, OrbStack or Colima (local fallback needs it)", "docker CLI not found")
		}
		return warn("check that the docker CLI works: `docker context ls`", "cannot list docker contexts: %v", err)
	}
	out, se, err := e.Run.Run(ctx, "docker", "--context", name, "version", "--format", "{{.Server.Version}} {{.Server.Os}}/{{.Server.Arch}}")
	if err != nil {
		return warn(fmt.Sprintf("start your local Docker (context %q) or set local.docker_context in the client config; without it, fallback placements fail", name),
			"local context %q is not reachable: %s", name, errText(err, se))
	}
	how := "auto-detected"
	if configured != "auto" {
		how = "configured"
	}
	return ok("context %s (%s), engine %s", name, how, strings.TrimSpace(out))
}

// Homebrew rsync locations, preferred over macOS openrsync (docs/compat.md §5).
var homebrewRsync = []string{"/opt/homebrew/bin/rsync", "/usr/local/bin/rsync"}

func checkLocalRsync(ctx context.Context, g *globalState) Outcome {
	e := g.env
	for _, p := range homebrewRsync {
		if _, err := e.Stat(p); err == nil {
			out, _, err := e.Run.Run(ctx, p, "--version")
			if err == nil {
				return ok("%s: %s", p, firstLine(out))
			}
		}
	}
	out, se, err := e.Run.Run(ctx, "rsync", "--version")
	if err != nil {
		return warn("install rsync (`brew install rsync`); workspace sync for compose bind mounts needs it",
			"rsync not found: %s", errText(err, se))
	}
	v := firstLine(out)
	if strings.Contains(strings.ToLower(out), "openrsync") {
		return info("%s (macOS openrsync: works with dol's flags; `brew install rsync` is recommended)", v)
	}
	return ok("%s", v)
}

var nodeChecks = []check[*clientState]{
	{ID: "ssh-config", Title: "ssh config for the node", Timeout: 5 * time.Second, Run: checkSSHConfig},
	{ID: "ssh", Title: "ssh reachable", Timeout: 20 * time.Second, Run: checkSSH},
	{ID: "ssh-control-master", Title: "ssh ControlMaster", Timeout: 5 * time.Second, Run: checkControlMaster},
	{ID: "remote-docker-group", Title: "Remote user in docker group", Run: checkRemoteGroups},
	{ID: "docker-context", Title: "Docker context", Timeout: 30 * time.Second, Run: checkDockerContext},
	{ID: "node-agent", Title: "Node agent reachable", Timeout: 20 * time.Second, Run: checkNodeAgent},
	{ID: "version", Title: "Versions compatible", Run: checkVersion},
	{ID: "arch", Title: "Architecture", Run: checkArch},
	{ID: "clock-skew", Title: "Clock skew", Run: checkClockSkew},
	{ID: "capacity", Title: "Capacity", Run: checkCapacity},
	{ID: "disk", Title: "Disk", Run: checkDisk},
	{ID: "unlabeled", Title: "Unlabeled resources", Run: checkUnlabeled},
	{ID: "remote-kind", Title: "kind/kubectl on node", Run: checkRemoteKind},
	{ID: "remote-rsync", Title: "rsync on node", Run: checkRemoteRsync},
}

func (s *clientState) ssh(ctx context.Context, remote ...string) (string, string, error) {
	bin := s.env.SSH.Binary
	if bin == "" {
		bin = "ssh"
	}
	return s.env.Run.Run(ctx, bin, s.env.SSH.Args(s.node.SSHHost, remote...)...)
}

func (s *clientState) sshLocal(ctx context.Context, args ...string) (string, string, error) {
	bin := s.env.SSH.Binary
	if bin == "" {
		bin = "ssh"
	}
	a := append(append([]string{}, s.env.SSH.ExtraArgs...), args...)
	a = append(a, "--", s.node.SSHHost)
	return s.env.Run.Run(ctx, bin, a...)
}

// parseSSHG parses `ssh -G` output into lowercase key → value.
func parseSSHG(out string) map[string]string {
	m := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		k, v, found := strings.Cut(strings.TrimSpace(l), " ")
		k = strings.ToLower(k)
		if _, dup := m[k]; found && !dup {
			m[k] = v
		}
	}
	return m
}

func checkSSHConfig(ctx context.Context, s *clientState) Outcome {
	out, se, err := s.sshLocal(ctx, "-G")
	if err != nil {
		return warn("check ~/.ssh/config syntax (`ssh -G "+s.node.SSHHost+"`)", "ssh -G failed: %s", errText(err, se))
	}
	m := parseSSHG(out)
	s.sshUser = m["user"]
	var problems []string
	fixes := []string{}
	if p := m["port"]; p != strconv.Itoa(SSHPort) {
		problems = append(problems, "port "+p+" (expected "+strconv.Itoa(SSHPort)+")")
	}
	if m["user"] != NodeUser {
		problems = append(problems, "user "+m["user"]+" (expected "+NodeUser+")")
	}
	if cm := m["controlmaster"]; cm == "" || cm == "false" || cm == "no" {
		problems = append(problems, "ControlMaster off (every docker call pays a full ssh handshake)")
	}
	if len(problems) > 0 {
		fixes = append(fixes, "run `dol init --name "+s.node.Name+" --host <ip>` to (re)write the Host block in ~/.ssh/config.d/docker-offload-lan, and make sure ~/.ssh/config starts with `Include config.d/*`")
		return warn(strings.Join(fixes, "; "), "Host %s → %s: %s", s.node.SSHHost, m["hostname"], strings.Join(problems, ", "))
	}
	return ok("Host %s → %s:%s as %s, ControlMaster %s", s.node.SSHHost, m["hostname"], m["port"], m["user"], m["controlmaster"])
}

func checkSSH(ctx context.Context, s *clientState) Outcome {
	start := time.Now()
	out, se, err := s.ssh(ctx, "echo", "dol-ok", "$(id -un)")
	took := time.Since(start)
	if err != nil {
		hint := sshx.Hint(se)
		if hint == "" {
			hint = "try `ssh " + s.node.SSHHost + "` by hand; see docs/troubleshooting.md#ssh"
		}
		return fail(hint, "ssh %s failed after %s: %s", s.node.SSHHost, fmtDuration(took), errText(err, se))
	}
	f := strings.Fields(out)
	if len(f) < 2 || f[0] != "dol-ok" {
		return fail("the remote shell prints extra output or is not POSIX; check ~/.bashrc of the remote user", "unexpected output from ssh: %q", strings.TrimSpace(out))
	}
	s.sshOK = true
	s.sshUser = f[1]
	return ok("logged in as %s in %s (BatchMode)", f[1], fmtDuration(took))
}

func checkControlMaster(ctx context.Context, s *clientState) Outcome {
	if !s.sshOK {
		return skip("ssh failed")
	}
	_, se, err := s.sshLocal(ctx, "-O", "check")
	if err != nil {
		return warn("add `ControlMaster auto`, `ControlPath ~/.ssh/cm-%C` and `ControlPersist 10m` to the Host block (`dol init` writes them)",
			"no ControlMaster for %s: %s", s.node.SSHHost, errText(err, se))
	}
	return ok("%s", firstLine(se))
}

func checkRemoteGroups(ctx context.Context, s *clientState) Outcome {
	if !s.sshOK {
		return skip("ssh failed")
	}
	out, se, err := s.ssh(ctx, "id", "-nG")
	if err != nil {
		return fail("check the node over ssh", "id -nG failed: %s", errText(err, se))
	}
	groups := strings.Fields(out)
	has := func(g string) bool {
		for _, x := range groups {
			if x == g {
				return true
			}
		}
		return false
	}
	if !has("docker") {
		return fail(fmt.Sprintf("on the node: `sudo usermod -aG docker %s` (or re-run deploy/linux/install.sh), then reconnect: `ssh -O exit %s`", s.sshUser, s.node.SSHHost),
			"%s is not in the docker group (groups: %s)", s.sshUser, strings.Join(groups, " "))
	}
	if !has("dol") {
		return warn("re-run deploy/linux/install.sh on the node", "%s is in docker but not in group dol (needed for the node agent socket)", s.sshUser)
	}
	return ok("%s in groups %s", s.sshUser, strings.Join(groups, " "))
}

func checkDockerContext(ctx context.Context, s *clientState) Outcome {
	name := s.node.DockerContext
	out, se, err := s.env.Run.Run(ctx, "docker", "context", "inspect", name, "--format", "{{json .}}")
	if err != nil {
		return fail(fmt.Sprintf("run `dol init --name %s --host <ip>` (creates it), or `docker context create %s --description %q --docker host=ssh://%s`",
			s.node.Name, name, ManagedContextDescription, s.node.SSHHost), "docker context %s does not exist: %s", name, errText(err, se))
	}
	var ci struct {
		Metadata struct {
			Description string `json:"Description"`
		} `json:"Metadata"`
		Endpoints map[string]struct {
			Host string `json:"Host"`
		} `json:"Endpoints"`
	}
	_ = json.Unmarshal([]byte(strings.TrimSpace(out)), &ci)
	want := "ssh://" + s.node.SSHHost
	if h := ci.Endpoints["docker"].Host; h != "" && h != want {
		return fail(fmt.Sprintf("`docker context update %s --docker host=%s` (or re-run `dol init`)", name, want),
			"context %s points to %s, expected %s", name, h, want)
	}
	if !s.sshOK {
		return skip("context exists; not tested because ssh failed")
	}
	start := time.Now()
	vo, se, err := s.env.Run.Run(ctx, "docker", "--context", name, "version", "--format", "{{.Server.Version}}|{{.Server.Arch}}")
	if err != nil {
		hint := "on the node: `sudo systemctl status docker`; the docker CLI must be installed there (docker-ce-cli)"
		if h := sshx.Hint(se); h != "" {
			hint = h
		}
		return fail(hint, "docker --context %s version failed: %s", name, errText(err, se))
	}
	ver, arch, _ := strings.Cut(strings.TrimSpace(vo), "|")
	s.dockerArch = arch
	return ok("%s → Docker Engine %s (%s) in %s", name, ver, arch, fmtDuration(time.Since(start)))
}

func checkNodeAgent(ctx context.Context, s *clientState) Outcome {
	if !s.sshOK {
		return skip("ssh failed")
	}
	if s.env.NewNodeAPI == nil {
		return skip("no node API client configured")
	}
	s.api = s.env.NewNodeAPI(s.node)
	start := time.Now()
	h, err := s.api.Health(ctx)
	s.healthRTT = time.Since(start)
	s.healthAt = start.Add(s.healthRTT / 2)
	if err != nil {
		fix := "on the node: `sudo systemctl status dol-node` and `journalctl -u dol-node -n 50`"
		if e, ok := v1.AsError(err); ok && e.Hint != "" {
			fix = e.Hint
		}
		if strings.Contains(err.Error(), "not implemented") {
			fix = "the node runs a dol build without the node agent; install a release with `dol node serve` (deploy/linux/install.sh --dol-binary …)"
		}
		return fail(fix, "%v", err)
	}
	s.health = h
	if h.Status != "ok" {
		return warn("on the node: `journalctl -u dol-node -n 50`", "node %s is %s: %s", h.Node, h.Status, strings.Join(h.Problems, "; "))
	}
	return ok("node %s (%s, %s) answered in %s", h.Node, h.Provider, h.Version, fmtDuration(s.healthRTT))
}

func checkVersion(_ context.Context, s *clientState) Outcome {
	if s.health == nil {
		return skip("node agent not reachable")
	}
	h := s.health
	if h.APIVersion != v1.APIVersion {
		return fail("install the same dol release on client and node", "node speaks API %q, this client speaks %q", h.APIVersion, v1.APIVersion)
	}
	cv, nv := s.env.ClientVersion, h.Version
	if cv == "dev" || nv == "dev" || cv == "" || nv == "" {
		return ok("API %s; client %s, node %s (development build)", h.APIVersion, orUnknown(cv), orUnknown(nv))
	}
	if majorMinor(cv) != majorMinor(nv) {
		return warn("upgrade the older side so both run the same release (`brew upgrade dol` / re-run install.sh --dol-version …)",
			"client %s and node %s differ in minor version", cv, nv)
	}
	return ok("client %s, node %s (API %s)", cv, nv, h.APIVersion)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func majorMinor(v string) string {
	v = strings.TrimPrefix(v, "v")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}

func normArch(a string) string {
	switch a {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	}
	return a
}

func checkArch(_ context.Context, s *clientState) Outcome {
	na := ""
	if s.health != nil {
		na = s.health.Arch
	}
	if na == "" {
		na = s.dockerArch
	}
	if na == "" {
		return skip("node architecture unknown (node agent and docker unreachable)")
	}
	na, ca := normArch(na), normArch(s.env.GOARCH)
	if na != ca {
		return info("node is %s, this machine is %s: build images with the node's docker context (builds run on the node); never `docker save | docker load` across machines", na, ca)
	}
	return ok("node and client are both %s", na)
}

func checkClockSkew(_ context.Context, s *clientState) Outcome {
	if s.health == nil || s.health.Time.IsZero() {
		return skip("node time unknown (node agent not reachable)")
	}
	skew := s.health.Time.Sub(s.healthAt)
	abs := time.Duration(math.Abs(float64(skew)))
	msg := fmt.Sprintf("node clock is %s %s (±%s measurement)", fmtDuration(abs), aheadBehind(skew), fmtDuration(s.healthRTT/2))
	fix := "enable time sync on both machines (macOS: Settings → Date & Time → automatic; WSL: `sudo hwclock -s` or `wsl --shutdown` after sleep; Linux: `timedatectl set-ntp true`)"
	switch {
	case abs <= 2*time.Second+s.healthRTT/2:
		return ok("%s", msg)
	case abs <= 30*time.Second:
		return warn(fix, "%s; lease expiry times will be slightly off", msg)
	default:
		return fail(fix, "%s; leases may expire early or late", msg)
	}
}

func aheadBehind(d time.Duration) string {
	if d < 0 {
		return "behind"
	}
	return "ahead"
}

func (s *clientState) loadCapacity(ctx context.Context) (*v1.Capacity, error) {
	if s.capacity != nil {
		return s.capacity, nil
	}
	if s.api == nil || s.health == nil {
		return nil, errors.New("node agent not reachable")
	}
	c, err := s.api.Capacity(ctx)
	if err != nil {
		return nil, err
	}
	s.capacity = c
	return c, nil
}

func checkCapacity(ctx context.Context, s *clientState) Outcome {
	if s.health == nil {
		return skip("node agent not reachable")
	}
	c, err := s.loadCapacity(ctx)
	if err != nil {
		return fail("on the node: `journalctl -u dol-node -n 50`", "GET /v1/capacity failed: %v", err)
	}
	m := c.Memory
	msg := fmt.Sprintf("%s schedulable free of %s (reserved %s, committed %s), %d active lease(s), %d cores, load %.2f",
		v1.FormatBytes(m.SchedulableFreeBytes), v1.FormatBytes(m.TotalBytes), v1.FormatBytes(m.ReservedBytes),
		v1.FormatBytes(m.CommittedBytes), c.LeasesActive, c.CPU.LogicalCores, c.CPU.Load1)
	if m.HostFreeBytes > 0 {
		msg += fmt.Sprintf("; host free %s", v1.FormatBytes(m.HostFreeBytes))
	}
	if len(c.Warnings) > 0 {
		msg += "; warnings: " + strings.Join(c.Warnings, "; ")
	}
	if !c.Accepting {
		return warn("free memory/disk on the node, release leases (`dol leases`), or undrain it; new work falls back to local meanwhile",
			"node is not accepting leases: %s; %s", c.Reason, msg)
	}
	return ok("%s", msg)
}

func checkDisk(ctx context.Context, s *clientState) Outcome {
	if s.health == nil {
		return skip("node agent not reachable")
	}
	c, err := s.loadCapacity(ctx)
	if err != nil {
		return skip("capacity unavailable: %v", err)
	}
	d := c.Disk
	msg := fmt.Sprintf("%s free on %s (minimum %s)", v1.FormatBytes(d.FreeBytes), d.Path, v1.FormatBytes(d.MinFreeBytes))
	low := d.FreeBytes
	where := d.Path
	if d.HostFreeBytes > 0 {
		msg += fmt.Sprintf("; host drive %s free", v1.FormatBytes(d.HostFreeBytes))
		if d.HostFreeBytes < low {
			low, where = d.HostFreeBytes, "the host drive (Windows C:)"
		}
	}
	fix := "free disk on the node: `docker --context " + s.node.DockerContext + " system df`, then `docker builder prune` / `docker image prune`; on WSL also free space on C:"
	switch {
	case d.MinFreeBytes > 0 && low < d.MinFreeBytes:
		return fail(fix, "%s; below the minimum on %s, the node refuses new leases", msg, where)
	case d.MinFreeBytes > 0 && low < 2*d.MinFreeBytes:
		return warn(fix, "%s; getting close to the minimum on %s", msg, where)
	}
	return ok("%s", msg)
}

func checkUnlabeled(ctx context.Context, s *clientState) Outcome {
	if s.health == nil {
		return skip("node agent not reachable")
	}
	c, err := s.loadCapacity(ctx)
	if err != nil {
		return skip("capacity unavailable: %v", err)
	}
	u := c.Unlabeled
	if u.Containers+u.Volumes+u.Networks == 0 {
		return ok("no unlabeled containers, volumes or networks")
	}
	return info("%d container(s), %d volume(s), %d network(s) without dol labels; dol never deletes them (list: `docker --context %s ps -a --filter label=%s!=true`)",
		u.Containers, u.Volumes, u.Networks, s.node.DockerContext, v1.LabelManaged)
}

func checkRemoteKind(ctx context.Context, s *clientState) Outcome {
	if !s.sshOK {
		return skip("ssh failed")
	}
	out, se, err := s.ssh(ctx, "kind version; kubectl version --client -o json")
	if err != nil && !strings.Contains(out, "kind ") {
		return fail("re-run deploy/linux/install.sh on the node (installs kind "+KindVersion+" and kubectl "+KubectlVersion+")",
			"kind not found on the node: %s", errText(err, se))
	}
	kv := ""
	if f := strings.Fields(firstLine(out)); len(f) >= 2 {
		kv = f[1]
	}
	kc := ""
	if i := strings.Index(out, "{"); i >= 0 {
		var v struct {
			ClientVersion struct {
				GitVersion string `json:"gitVersion"`
			} `json:"clientVersion"`
		}
		if json.Unmarshal([]byte(out[i:]), &v) == nil {
			kc = v.ClientVersion.GitVersion
		}
	}
	msg := fmt.Sprintf("kind %s, kubectl %s", orUnknown(kv), orUnknown(kc))
	if kv != KindVersion || kc != KubectlVersion {
		return warn("re-run deploy/linux/install.sh on the node to install the pinned versions",
			"%s (pinned: kind %s, kubectl %s)", msg, KindVersion, KubectlVersion)
	}
	return ok("%s", msg)
}

func checkRemoteRsync(ctx context.Context, s *clientState) Outcome {
	if !s.sshOK {
		return skip("ssh failed")
	}
	out, se, err := s.ssh(ctx, "rsync", "--version")
	if err != nil {
		return fail("on the node: `sudo apt-get install rsync` (or re-run install.sh)", "rsync not found on the node: %s", errText(err, se))
	}
	return ok("%s", firstLine(out))
}
