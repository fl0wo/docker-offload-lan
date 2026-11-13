package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/config"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// fakeRunner answers commands by matching "name args..." against regexps,
// first match wins. Unmatched commands fail like a missing binary.
type fakeRunner struct {
	rules []rule
	calls []string
}

type rule struct {
	re     *regexp.Regexp
	stdout string
	stderr string
	err    error
}

func (f *fakeRunner) on(pattern, stdout string) *fakeRunner {
	f.rules = append(f.rules, rule{re: regexp.MustCompile(pattern), stdout: stdout})
	return f
}

func (f *fakeRunner) onErr(pattern, stderr string) *fakeRunner {
	f.rules = append(f.rules, rule{re: regexp.MustCompile(pattern), stderr: stderr, err: errors.New("exit status 255")})
	return f
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) (string, string, error) {
	line := name + " " + strings.Join(args, " ")
	f.calls = append(f.calls, line)
	for _, r := range f.rules {
		if r.re.MatchString(line) {
			return r.stdout, r.stderr, r.err
		}
	}
	return "", "", &os.PathError{Op: "exec", Path: name, Err: os.ErrNotExist}
}

type fakeAPI struct {
	h   *v1.Health
	c   *v1.Capacity
	err error
}

func (f *fakeAPI) Health(context.Context) (*v1.Health, error)     { return f.h, f.err }
func (f *fakeAPI) Capacity(context.Context) (*v1.Capacity, error) { return f.c, f.err }
func (f *fakeAPI) Close()                                         {}

const sshG = `user dol
hostname 192.168.1.50
port 2222
controlmaster auto
controlpath /Users/x/.ssh/cm-%C
`

func healthyRunner() *fakeRunner {
	f := &fakeRunner{}
	f.on(`^docker context ls`, `{"Name":"default","Current":false}
{"Name":"desktop-linux","Current":true,"Description":"Docker Desktop"}
{"Name":"dol-tower","Current":false,"Description":"managed by docker-offload-lan"}`+"\n")
	f.on(`^docker --context desktop-linux version`, "29.0.1 linux/arm64\n")
	f.on(`^rsync --version`, "openrsync: protocol version 29\nrsync version 2.6.9 compatible\n")
	f.on(`^ssh .*-G -- tower$`, sshG)
	f.rules = append(f.rules, rule{re: regexp.MustCompile(`^ssh .*-O check -- tower$`), stderr: "Master running (pid=4242)\n"})
	f.on(`^ssh .*-- tower echo dol-ok`, "dol-ok dol\n")
	f.on(`^ssh .*-- tower id -nG`, "dol docker\n")
	f.on(`^docker context inspect dol-tower`, `{"Name":"dol-tower","Metadata":{"Description":"managed by docker-offload-lan"},"Endpoints":{"docker":{"Host":"ssh://tower"}}}`)
	f.on(`^docker --context dol-tower version`, "29.9.3|amd64\n")
	f.on(`^ssh .*-- tower kind version; kubectl`, "kind v0.33.0 go1.25 linux/amd64\n{\"clientVersion\":{\"gitVersion\":\"v1.37.2\"}}\n")
	f.on(`^ssh .*-- tower rsync --version`, "rsync  version 3.2.7  protocol version 31\n")
	return f
}

func healthyAPI(now time.Time) *fakeAPI {
	return &fakeAPI{
		h: &v1.Health{Status: "ok", Version: "0.1.0", APIVersion: "v1", Node: "tower", OS: "linux", Arch: "amd64", Provider: "wsl2", Time: now, Accepting: true},
		c: &v1.Capacity{Node: "tower", Accepting: true,
			Memory: v1.MemoryCapacity{TotalBytes: 40 * v1.GiB, SchedulableFreeBytes: 30 * v1.GiB},
			Disk:   v1.DiskCapacity{Path: "/var/lib/docker", FreeBytes: 200 * v1.GiB, HostFreeBytes: 100 * v1.GiB, MinFreeBytes: 30 * v1.GiB}},
	}
}

func clientEnv(r Runner, api NodeAPI, now time.Time) *ClientEnv {
	cfg := config.DefaultClient()
	cfg.Nodes = []config.NodeRef{{Name: "tower", SSHHost: "tower", DockerContext: "dol-tower", Priority: 100}}
	return &ClientEnv{
		ConfigPath: "/tmp/config.yaml", Config: cfg, Run: r,
		NewNodeAPI:    func(config.NodeRef) NodeAPI { return api },
		Now:           func() time.Time { return now },
		ClientVersion: "0.1.3", GOARCH: "arm64",
		Stat: func(string) (os.FileInfo, error) { return nil, os.ErrNotExist },
	}
}

func byID(r *Report) map[string]Result {
	m := map[string]Result{}
	for _, x := range r.Results {
		m[x.ID] = x
	}
	return m
}

func TestClientAllGreen(t *testing.T) {
	now := time.Now()
	r := RunClient(context.Background(), clientEnv(healthyRunner(), healthyAPI(now), now), ClientOptions{})
	got := byID(r)
	want := map[string]Status{
		"config": StatusOK, "local-docker": StatusOK, "local-rsync": StatusInfo,
		"ssh-config": StatusOK, "ssh": StatusOK, "ssh-control-master": StatusOK, "remote-docker-group": StatusOK,
		"docker-context": StatusOK, "node-agent": StatusOK, "version": StatusOK, "arch": StatusInfo,
		"clock-skew": StatusOK, "capacity": StatusOK, "disk": StatusOK, "unlabeled": StatusOK,
		"remote-kind": StatusOK, "remote-rsync": StatusOK,
	}
	for id, st := range want {
		if got[id].Status != st {
			t.Errorf("%s: status %s, want %s (%s; fix: %s)", id, got[id].Status, st, got[id].Message, got[id].Fix)
		}
	}
	if len(r.Results) != len(want) {
		t.Errorf("got %d results, want %d", len(r.Results), len(want))
	}
	if r.Failed() {
		t.Fatal("report must not fail")
	}
	if !strings.Contains(got["local-docker"].Message, "desktop-linux") {
		t.Errorf("local-docker: %s", got["local-docker"].Message)
	}
	if !strings.Contains(got["arch"].Message, "amd64") || !strings.Contains(got["arch"].Message, "arm64") {
		t.Errorf("arch: %s", got["arch"].Message)
	}
}

func TestClientSSHDownSkipsDependents(t *testing.T) {
	f := healthyRunner()
	// Prepend so it wins over the healthy rule.
	f.rules = append([]rule{{re: regexp.MustCompile(`^ssh .*-- tower echo`), stderr: "ssh: connect to host 192.168.1.50 port 2222: Connection refused", err: errors.New("exit status 255")}}, f.rules...)
	now := time.Now()
	r := RunClient(context.Background(), clientEnv(f, healthyAPI(now), now), ClientOptions{})
	got := byID(r)
	if got["ssh"].Status != StatusFail || !strings.Contains(got["ssh"].Fix, "2222") {
		t.Fatalf("ssh: %+v", got["ssh"])
	}
	for _, id := range []string{"ssh-control-master", "remote-docker-group", "node-agent", "version", "clock-skew", "capacity", "disk", "unlabeled", "remote-kind", "remote-rsync"} {
		if got[id].Status != StatusSkip {
			t.Errorf("%s: %s, want skip", id, got[id].Status)
		}
	}
	// docker-context: exists but not exercised; arch comes from nowhere.
	if got["docker-context"].Status != StatusSkip || got["arch"].Status != StatusSkip {
		t.Errorf("docker-context %s, arch %s", got["docker-context"].Status, got["arch"].Status)
	}
	if !r.Failed() {
		t.Fatal("report must fail")
	}
}

func TestClientNodeAgentProblems(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name  string
		api   *fakeAPI
		check string
		want  Status
		msg   string
	}{
		{"agent down", &fakeAPI{err: v1.Errorf(v1.CodeUnreachable, "the node agent is not running", "cannot reach node tower: connect to node agent")}, "node-agent", StatusFail, "not running"},
		{"not implemented", &fakeAPI{err: errors.New("dial-stdio is not implemented yet (milestone M2)")}, "node-agent", StatusFail, "node agent"},
		{"api mismatch", func() *fakeAPI { a := healthyAPI(now); a.h.APIVersion = "v2"; return a }(), "version", StatusFail, "v2"},
		{"minor mismatch", func() *fakeAPI { a := healthyAPI(now); a.h.Version = "0.2.0"; return a }(), "version", StatusWarn, "minor"},
		{"skew warn", func() *fakeAPI { a := healthyAPI(now); a.h.Time = now.Add(10 * time.Second); return a }(), "clock-skew", StatusWarn, "ahead"},
		{"skew fail", func() *fakeAPI { a := healthyAPI(now); a.h.Time = now.Add(-5 * time.Minute); return a }(), "clock-skew", StatusFail, "behind"},
		{"not accepting", func() *fakeAPI { a := healthyAPI(now); a.c.Accepting = false; a.c.Reason = "draining"; return a }(), "capacity", StatusWarn, "draining"},
		{"host disk low", func() *fakeAPI { a := healthyAPI(now); a.c.Disk.HostFreeBytes = 10 * v1.GiB; return a }(), "disk", StatusFail, "Windows C:"},
		{"disk close", func() *fakeAPI { a := healthyAPI(now); a.c.Disk.HostFreeBytes = 0; a.c.Disk.FreeBytes = 45 * v1.GiB; return a }(), "disk", StatusWarn, "close"},
		{"unlabeled", func() *fakeAPI { a := healthyAPI(now); a.c.Unlabeled.Containers = 3; return a }(), "unlabeled", StatusInfo, "3 container"},
		{"degraded", func() *fakeAPI { a := healthyAPI(now); a.h.Status = "degraded"; a.h.Problems = []string{"docker down"}; return a }(), "node-agent", StatusWarn, "docker down"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := RunClient(context.Background(), clientEnv(healthyRunner(), tc.api, now), ClientOptions{})
			got := byID(r)[tc.check]
			if got.Status != tc.want || !strings.Contains(got.Message+" "+got.Fix, tc.msg) {
				t.Fatalf("%s: %s %q fix %q", tc.check, got.Status, got.Message, got.Fix)
			}
		})
	}
}

func TestClientSSHConfigAndContextProblems(t *testing.T) {
	now := time.Now()
	f := healthyRunner()
	f.rules = append([]rule{
		{re: regexp.MustCompile(`^ssh .*-G -- tower$`), stdout: "user flo\nhostname tower\nport 22\ncontrolmaster false\n"},
		{re: regexp.MustCompile(`^docker context inspect dol-tower`), stdout: `{"Endpoints":{"docker":{"Host":"ssh://other"}}}`},
		{re: regexp.MustCompile(`^ssh .*-- tower id -nG`), stdout: "flo staff\n"},
		{re: regexp.MustCompile(`^ssh .*-O check`), stderr: "Control socket connect(/x): No such file or directory", err: errors.New("exit status 255")},
	}, f.rules...)
	r := RunClient(context.Background(), clientEnv(f, healthyAPI(now), now), ClientOptions{})
	got := byID(r)
	if g := got["ssh-config"]; g.Status != StatusWarn || !strings.Contains(g.Message, "port 22") || !strings.Contains(g.Message, "ControlMaster off") {
		t.Errorf("ssh-config: %+v", g)
	}
	if g := got["docker-context"]; g.Status != StatusFail || !strings.Contains(g.Fix, "docker context update") {
		t.Errorf("docker-context: %+v", g)
	}
	if g := got["remote-docker-group"]; g.Status != StatusFail || !strings.Contains(g.Fix, "usermod -aG docker") {
		t.Errorf("remote-docker-group: %+v", g)
	}
	if g := got["ssh-control-master"]; g.Status != StatusWarn {
		t.Errorf("ssh-control-master: %+v", g)
	}
}

func TestClientConfigErrorsAndNodeFilter(t *testing.T) {
	now := time.Now()
	env := clientEnv(healthyRunner(), healthyAPI(now), now)
	env.Config, env.ConfigErr = nil, errors.New("line 3: unknown key \"nodez\"")
	r := RunClient(context.Background(), env, ClientOptions{})
	if g := byID(r)["config"]; g.Status != StatusFail || !strings.Contains(g.Message, "nodez") {
		t.Fatalf("config: %+v", g)
	}
	env = clientEnv(healthyRunner(), healthyAPI(now), now)
	r = RunClient(context.Background(), env, ClientOptions{NodeName: "nope"})
	if !r.Failed() || r.Results[len(r.Results)-1].Fix == "" {
		t.Fatalf("unknown node must fail with a fix: %+v", r.Results[len(r.Results)-1])
	}
	env = clientEnv(healthyRunner(), healthyAPI(now), now)
	env.Config.Nodes = nil
	r = RunClient(context.Background(), env, ClientOptions{})
	if g := byID(r)["config"]; g.Status != StatusWarn || !strings.Contains(g.Fix, "dol init") {
		t.Fatalf("config: %+v", g)
	}
}

func TestLocalRsyncPrefersHomebrew(t *testing.T) {
	f := healthyRunner().on(`^/opt/homebrew/bin/rsync --version`, "rsync  version 3.5.2  protocol version 32\n")
	env := clientEnv(f, nil, time.Now())
	env.Stat = func(p string) (os.FileInfo, error) {
		if p == "/opt/homebrew/bin/rsync" {
			return nil, nil
		}
		return nil, os.ErrNotExist
	}
	r := RunClient(context.Background(), env, ClientOptions{NodeName: "tower"})
	if g := byID(r)["local-rsync"]; g.Status != StatusOK || !strings.Contains(g.Message, "3.5.2") {
		t.Fatalf("local-rsync: %+v", g)
	}
}

func TestDetectLocalContext(t *testing.T) {
	tests := []struct {
		name, ls, configured, want string
	}{
		{"configured wins", "", "colima", "colima"},
		{"current", `{"Name":"orbstack","Current":true}`, "auto", "orbstack"},
		{"current is dol", `{"Name":"dol-tower","Current":true}` + "\n" + `{"Name":"colima"}` + "\n" + `{"Name":"desktop-linux"}`, "auto", "desktop-linux"},
		{"fallback default", `{"Name":"dol-tower","Current":true}`, "", "default"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := (&fakeRunner{}).on(`^docker context ls`, tc.ls)
			got, err := DetectLocalContext(context.Background(), f, tc.configured)
			if err != nil || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

func TestPrinterAndJSON(t *testing.T) {
	now := time.Now()
	var buf bytes.Buffer
	p := &Printer{W: &buf, first: true}
	r := RunClient(context.Background(), clientEnv(healthyRunner(), &fakeAPI{err: errors.New("boom")}, now), ClientOptions{Progress: p.Result})
	p.Summary(r)
	out := buf.String()
	for _, want := range []string{"✓ config", "Node tower", "✗ node-agent", "fix:", "· version", "Summary:", "1 failed", "troubleshooting.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	var jb bytes.Buffer
	if err := r.WriteJSON(&jb); err != nil {
		t.Fatal(err)
	}
	var back Report
	if err := json.Unmarshal(jb.Bytes(), &back); err != nil || back.Summary.Fail != 1 || back.Mode != "client" {
		t.Fatalf("json roundtrip: %v %+v", err, back.Summary)
	}
}

func TestPanickingCheckBecomesFailure(t *testing.T) {
	r := &Report{}
	runTable(context.Background(), r, "", 0, []check[int]{{ID: "x", Run: func(context.Context, int) Outcome { panic("oops") }}}, nil)
	if r.Results[0].Status != StatusFail || !strings.Contains(r.Results[0].Message, "oops") {
		t.Fatalf("%+v", r.Results[0])
	}
}
