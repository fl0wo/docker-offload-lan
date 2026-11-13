package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/config"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// FileMode is the subset of file metadata the node checks need.
type FileMode struct {
	Mode  os.FileMode
	UID   int
	GID   int
	Exist bool
}

// NodeEnv is everything node-mode checks touch.
type NodeEnv struct {
	ConfigPath string
	Run        Runner
	// Stat returns metadata of a path (Exist=false if missing).
	Stat func(path string) (FileMode, error)
	// DiskFree returns free and total bytes of the filesystem holding path.
	DiskFree func(path string) (free, total uint64, err error)
	// ListeningTCPPorts returns local TCP ports in LISTEN state.
	ListeningTCPPorts func() (map[int]bool, error)
	// IsWSL reports whether this is a WSL distro.
	IsWSL func() bool
	// HasSystemd reports whether systemd is PID 1.
	HasSystemd func() bool
	// LookupGroupID resolves a group name.
	LookupGroupID func(name string) (int, error)
	// NewNodeAPI connects to the local node agent socket.
	NewNodeAPI func(socket string) NodeAPI
}

func (e *NodeEnv) defaults() {
	if e.ConfigPath == "" {
		e.ConfigPath = config.NodeConfigPath
	}
	if e.Run == nil {
		e.Run = ExecRunner{}
	}
	if e.Stat == nil {
		e.Stat = statPath
	}
	if e.DiskFree == nil {
		e.DiskFree = diskFree
	}
	if e.ListeningTCPPorts == nil {
		e.ListeningTCPPorts = listeningTCPPorts
	}
	if e.IsWSL == nil {
		e.IsWSL = isWSL
	}
	if e.HasSystemd == nil {
		e.HasSystemd = func() bool { fi, err := os.Stat("/run/systemd/system"); return err == nil && fi.IsDir() }
	}
	if e.LookupGroupID == nil {
		e.LookupGroupID = func(name string) (int, error) {
			g, err := user.LookupGroup(name)
			if err != nil {
				return 0, err
			}
			return strconv.Atoi(g.Gid)
		}
	}
}

func isWSL() bool {
	if _, err := os.Stat("/proc/sys/fs/binfmt_misc/WSLInterop"); err == nil {
		return true
	}
	b, _ := os.ReadFile("/proc/version")
	return strings.Contains(strings.ToLower(string(b)), "microsoft")
}

type nodeState struct {
	env           *NodeEnv
	cfg           *config.NodeConfig
	dockerOK      bool
	dockerRootDir string
}

// NodeOptions tune a node-mode run.
type NodeOptions struct {
	Progress func(Result)
	Timeout  time.Duration
}

// RunNode runs node-mode checks (`dol doctor --node`, on the node itself).
func RunNode(ctx context.Context, env *NodeEnv, opts NodeOptions) *Report {
	env.defaults()
	r := &Report{Mode: "node"}
	s := &nodeState{env: env}
	runTable(ctx, r, "", s, withTimeout(nodeModeChecks, opts.Timeout), opts.Progress)
	return r
}

var nodeModeChecks = []check[*nodeState]{
	{ID: "node-config", Title: "Node config", Run: checkNodeConfig},
	{ID: "docker-daemon", Title: "Docker daemon", Run: checkDockerDaemon},
	{ID: "dol-user", Title: "dol user and groups", Run: checkDolUser},
	{ID: "systemd-unit", Title: "dol-node.service", Run: checkSystemdUnit},
	{ID: "node-socket", Title: "Node agent socket", Run: checkNodeSocket},
	{ID: "node-agent-local", Title: "Node agent answers", Run: checkNodeAgentLocal},
	{ID: "sshd-port", Title: "sshd on port 2222", Run: checkSSHDPort},
	{ID: "kind", Title: "kind", Run: checkKindLocal},
	{ID: "kubectl", Title: "kubectl", Run: checkKubectlLocal},
	{ID: "rsync", Title: "rsync", Run: checkRsyncLocal},
	{ID: "sync-root", Title: "Workspace sync root", Run: checkSyncRoot},
	{ID: "disk-docker", Title: "Disk (Docker data root)", Run: checkDiskDocker},
	{ID: "disk-wsl-host", Title: "Disk (Windows C:)", Run: checkDiskWSLHost},
	{ID: "wsl-interop", Title: "WSL interop", Run: checkWSLInterop},
}

func (s *nodeState) minFree() uint64 {
	gb := 30.0
	if s.cfg != nil {
		gb = s.cfg.Capacity.MinFreeDiskGB
	}
	return uint64(v1.GBToBytes(gb))
}

func checkNodeConfig(_ context.Context, s *nodeState) Outcome {
	c, err := config.LoadNode(s.env.ConfigPath)
	if err != nil {
		return fail("re-run deploy/linux/install.sh (writes a default) or fix the file", "%v", err)
	}
	s.cfg = c
	return ok("%s: node %s, provider %s, socket %s", s.env.ConfigPath, c.Name, c.Capacity.Provider, c.Socket)
}

func checkDockerDaemon(ctx context.Context, s *nodeState) Outcome {
	out, se, err := s.env.Run.Run(ctx, "docker", "info", "--format", "{{json .}}")
	if err != nil {
		fix := "`sudo systemctl enable --now docker` and check `journalctl -u docker -n 50`"
		if isNotFound(err) {
			fix = "install Docker Engine: re-run deploy/linux/install.sh"
		}
		return fail(fix, "docker info failed: %s", errText(err, se))
	}
	var i struct {
		ServerVersion string   `json:"ServerVersion"`
		DockerRootDir string   `json:"DockerRootDir"`
		Architecture  string   `json:"Architecture"`
		LoggingDriver string   `json:"LoggingDriver"`
		ServerErrors  []string `json:"ServerErrors"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &i); err != nil {
		return fail("check `docker info` by hand", "cannot parse docker info: %v", err)
	}
	if len(i.ServerErrors) > 0 {
		return fail("`sudo systemctl restart docker`; see `journalctl -u docker -n 50`", "docker daemon: %s", strings.Join(i.ServerErrors, "; "))
	}
	s.dockerOK = true
	s.dockerRootDir = i.DockerRootDir
	msg := fmt.Sprintf("Engine %s (%s), data root %s, log driver %s", i.ServerVersion, i.Architecture, i.DockerRootDir, i.LoggingDriver)
	if i.LoggingDriver != "local" {
		return warn("re-run install.sh to merge log-driver \"local\" with size caps into /etc/docker/daemon.json", "%s (container logs are not size-capped)", msg)
	}
	return ok("%s", msg)
}

func checkDolUser(ctx context.Context, s *nodeState) Outcome {
	out, se, err := s.env.Run.Run(ctx, "id", "-nG", NodeUser)
	if err != nil {
		return fail("re-run deploy/linux/install.sh (creates the dol user)", "user %s: %s", NodeUser, errText(err, se))
	}
	g := " " + strings.TrimSpace(out) + " "
	var missing []string
	for _, want := range []string{"docker", "dol"} {
		if !strings.Contains(g, " "+want+" ") {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return fail(fmt.Sprintf("`sudo usermod -aG %s %s`", strings.Join(missing, ","), NodeUser),
			"%s is not in group(s) %s (groups: %s)", NodeUser, strings.Join(missing, ", "), strings.TrimSpace(out))
	}
	return ok("%s in groups %s", NodeUser, strings.TrimSpace(out))
}

func checkSystemdUnit(ctx context.Context, s *nodeState) Outcome {
	if !s.env.HasSystemd() {
		return skip("systemd is not running (container or WSL without systemd); the agent must be started another way")
	}
	out, _, err := s.env.Run.Run(ctx, "systemctl", "is-active", "dol-node.service")
	state := strings.TrimSpace(out)
	if err != nil || state != "active" {
		return fail("`sudo systemctl enable --now dol-node` and `journalctl -u dol-node -n 50`", "dol-node.service is %s", orUnknown(state))
	}
	en, _, _ := s.env.Run.Run(ctx, "systemctl", "is-enabled", "dol-node.service")
	if strings.TrimSpace(en) != "enabled" {
		return warn("`sudo systemctl enable dol-node`", "dol-node.service is active but %s (won't start at boot)", orUnknown(strings.TrimSpace(en)))
	}
	return ok("dol-node.service active and enabled")
}

func (s *nodeState) socketPath() string {
	if s.cfg != nil {
		return s.cfg.Socket
	}
	return config.DefaultNode().Socket
}

func checkNodeSocket(_ context.Context, s *nodeState) Outcome {
	p := s.socketPath()
	fm, err := s.env.Stat(p)
	if err != nil || !fm.Exist {
		return fail("start the node agent: `sudo systemctl restart dol-node`; logs: `journalctl -u dol-node -n 50`", "%s does not exist (node agent not running)", p)
	}
	if fm.Mode&os.ModeSocket == 0 {
		return fail("stop the agent, remove "+p+", start it again", "%s exists but is not a socket", p)
	}
	var problems []string
	if perm := fm.Mode.Perm(); perm != 0o660 {
		problems = append(problems, fmt.Sprintf("mode %04o (want 0660)", perm))
	}
	if gid, err := s.env.LookupGroupID("dol"); err != nil {
		problems = append(problems, "group dol does not exist")
	} else if fm.GID != gid {
		problems = append(problems, fmt.Sprintf("group id %d (want dol=%d)", fm.GID, gid))
	}
	if len(problems) > 0 {
		return fail("the agent sets mode 0660 group dol on its socket; run it as user dol (dol-node.service does)", "%s: %s", p, strings.Join(problems, ", "))
	}
	return ok("%s (srw-rw---- group dol)", p)
}

func checkNodeAgentLocal(ctx context.Context, s *nodeState) Outcome {
	p := s.socketPath()
	if fm, err := s.env.Stat(p); err != nil || !fm.Exist || s.env.NewNodeAPI == nil {
		return skip("socket missing")
	}
	api := s.env.NewNodeAPI(p)
	defer api.Close()
	h, err := api.Health(ctx)
	if err != nil {
		return fail("`journalctl -u dol-node -n 50`", "health request failed: %v", err)
	}
	if h.Status != "ok" {
		return warn("`journalctl -u dol-node -n 50`", "agent %s is %s: %s", h.Version, h.Status, strings.Join(h.Problems, "; "))
	}
	return ok("agent %s (%s provider, accepting=%v)", h.Version, h.Provider, h.Accepting)
}

func checkSSHDPort(_ context.Context, s *nodeState) Outcome {
	ports, err := s.env.ListeningTCPPorts()
	if err != nil {
		return warn("check by hand: `ss -ltnp | grep 2222`", "cannot read listening ports: %v", err)
	}
	if !ports[SSHPort] {
		return fail("`sudo sshd -t` then `sudo systemctl restart ssh`; the installer writes /etc/ssh/sshd_config.d/10-docker-offload-lan.conf",
			"nothing listens on TCP %d", SSHPort)
	}
	msg := fmt.Sprintf("TCP %d is listening", SSHPort)
	if s.env.IsWSL() && ports[22] {
		return warn("remove `Port 22` from /etc/ssh/sshd_config (in mirrored networking port 22 belongs to Windows OpenSSH)",
			"%s, but sshd in WSL also listens on 22", msg)
	}
	return ok("%s", msg)
}

func checkToolVersion(ctx context.Context, s *nodeState, name, want string, args ...string) Outcome {
	out, se, err := s.env.Run.Run(ctx, name, args...)
	if err != nil {
		return fail("re-run deploy/linux/install.sh (installs "+name+" "+want+")", "%s not found or failing: %s", name, errText(err, se))
	}
	got := ""
	if name == "kubectl" {
		var v struct {
			ClientVersion struct {
				GitVersion string `json:"gitVersion"`
			} `json:"clientVersion"`
		}
		if json.Unmarshal([]byte(out), &v) == nil {
			got = v.ClientVersion.GitVersion
		}
	} else if f := strings.Fields(firstLine(out)); len(f) >= 2 {
		got = f[1]
	}
	if got != want {
		return warn("re-run deploy/linux/install.sh to install the pinned version", "%s %s installed, pinned %s", name, orUnknown(got), want)
	}
	return ok("%s %s", name, got)
}

func checkKindLocal(ctx context.Context, s *nodeState) Outcome {
	bin := "kind"
	if s.cfg != nil && s.cfg.Kind.Binary != "" {
		bin = s.cfg.Kind.Binary
	}
	return checkToolVersion(ctx, s, bin, KindVersion, "version")
}

func checkKubectlLocal(ctx context.Context, s *nodeState) Outcome {
	return checkToolVersion(ctx, s, "kubectl", KubectlVersion, "version", "--client", "-o", "json")
}

func checkRsyncLocal(ctx context.Context, s *nodeState) Outcome {
	out, se, err := s.env.Run.Run(ctx, "rsync", "--version")
	if err != nil {
		return fail("`sudo apt-get install rsync`", "rsync not found: %s", errText(err, se))
	}
	return ok("%s", firstLine(out))
}

func checkSyncRoot(_ context.Context, s *nodeState) Outcome {
	root := "/srv/dol"
	if s.cfg != nil {
		root = s.cfg.SyncRoot
	}
	fm, err := s.env.Stat(root)
	if err != nil || !fm.Exist {
		return fail("re-run deploy/linux/install.sh (creates "+root+" owned by dol)", "%s does not exist", root)
	}
	if !fm.Mode.IsDir() {
		return fail("move it away and re-run install.sh", "%s is not a directory", root)
	}
	if u, err := user.Lookup(NodeUser); err == nil {
		if uid, _ := strconv.Atoi(u.Uid); uid != fm.UID {
			return fail(fmt.Sprintf("`sudo chown dol:dol %s`", root), "%s is owned by uid %d, not %s", root, fm.UID, NodeUser)
		}
	}
	return ok("%s (owner %s, mode %04o)", root, NodeUser, fm.Mode.Perm())
}

func diskOutcome(free, total, min uint64, path, fix string) Outcome {
	msg := fmt.Sprintf("%s free of %s on %s (minimum %s)", v1.FormatBytes(int64(free)), v1.FormatBytes(int64(total)), path, v1.FormatBytes(int64(min)))
	switch {
	case free < min:
		return fail(fix, "%s; the node refuses new leases", msg)
	case free < 2*min:
		return warn(fix, "%s; getting close to the minimum", msg)
	}
	return ok("%s", msg)
}

func checkDiskDocker(_ context.Context, s *nodeState) Outcome {
	p := s.dockerRootDir
	if p == "" {
		p = "/var/lib/docker"
	}
	free, total, err := s.env.DiskFree(p)
	if err != nil {
		return warn("check `df -h "+p+"`", "statfs %s: %v", p, err)
	}
	o := diskOutcome(free, total, s.minFree(), p, "`docker system df`, then `docker builder prune` and `docker image prune`")
	if s.env.IsWSL() && o.Status == StatusOK {
		o.Message += " (WSL: the virtual disk's size, not real free space; see disk-wsl-host)"
	}
	return o
}

func checkDiskWSLHost(_ context.Context, s *nodeState) Outcome {
	if !s.env.IsWSL() {
		return skip("not WSL")
	}
	free, total, err := s.env.DiskFree("/mnt/c")
	if err != nil {
		return warn("make sure C: is mounted in WSL (/etc/wsl.conf [automount] enabled=true)", "statfs /mnt/c: %v", err)
	}
	return diskOutcome(free, total, s.minFree(), "/mnt/c (Windows C:)",
		"free space on C: (the WSL virtual disk grows there); with sparseVhd, `docker system prune` returns space to Windows")
}

func checkWSLInterop(ctx context.Context, s *nodeState) Outcome {
	if !s.env.IsWSL() {
		return skip("not WSL")
	}
	if fm, err := s.env.Stat("/proc/sys/fs/binfmt_misc/WSLInterop"); err != nil || !fm.Exist {
		return warn("enable interop in /etc/wsl.conf ([interop] enabled=true) and `wsl --shutdown`; without it the node cannot see Windows free memory",
			"WSL interop is disabled")
	}
	out, se, err := s.env.Run.Run(ctx, "cmd.exe", "/c", "ver")
	if err != nil {
		return warn("check that Windows executables run from WSL (`cmd.exe /c ver`); the capacity provider then ignores Windows free memory",
			"cannot run Windows executables: %s", errText(err, se))
	}
	return ok("interop works (%s)", firstLine(out))
}
