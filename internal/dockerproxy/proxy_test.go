package dockerproxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/sshx"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// fakeEngine is a minimal Docker Engine API used to observe what the proxy
// forwards.
type fakeEngine struct {
	mu       sync.Mutex
	bodies   map[string][]byte // path -> last body
	queries  map[string]string // path -> last raw query
	buildN   int64
	buildSum string
	release  chan struct{} // logs -f: second line is written after this
	srv      *httptest.Server
}

const upgradedHead = "HTTP/1.1 101 UPGRADED\r\nContent-Type: application/vnd.docker.raw-stream\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n"

// pattern returns n deterministic pseudo-random bytes.
func pattern(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	fe := &fakeEngine{bodies: map[string][]byte{}, queries: map[string]string{}, release: make(chan struct{})}
	mux := http.NewServeMux()
	record := func(r *http.Request) []byte {
		b, _ := io.ReadAll(r.Body)
		fe.mu.Lock()
		fe.bodies[apiPath(r.URL.Path)] = b
		fe.queries[apiPath(r.URL.Path)] = r.URL.RawQuery
		fe.mu.Unlock()
		return b
	}
	created := func(w http.ResponseWriter, r *http.Request) {
		record(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"Id":"c0ffee","Warnings":[]}`)
	}
	for _, p := range []string{"/containers/create", "/networks/create", "/volumes/create"} {
		mux.HandleFunc("POST "+p, created)
		mux.HandleFunc("POST /{ver}"+p, created)
	}
	build := func(w http.ResponseWriter, r *http.Request) {
		h := sha256.New()
		n, _ := io.Copy(h, r.Body)
		fe.mu.Lock()
		fe.buildN, fe.buildSum = n, hex.EncodeToString(h.Sum(nil))
		fe.queries["/build"] = r.URL.RawQuery
		fe.mu.Unlock()
		_, _ = io.WriteString(w, `{"stream":"done"}`+"\n")
	}
	mux.HandleFunc("POST /build", build)
	mux.HandleFunc("POST /{ver}/build", build)
	// attach: streams 3 MiB of pattern(1) while consuming stdin; after stdin
	// EOF writes "\nsha256:<hex of stdin>" and closes.
	attach := func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "missing" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"No such container: missing"}`)
			return
		}
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.WriteString(conn, upgradedHead)
		var wg sync.WaitGroup
		wg.Go(func() { _, _ = conn.Write(pattern(1, 3<<20)) })
		h := sha256.New()
		_, _ = io.Copy(h, buf)
		wg.Wait()
		_, _ = fmt.Fprintf(conn, "\nsha256:%x", h.Sum(nil))
	}
	mux.HandleFunc("POST /containers/{id}/attach", attach)
	mux.HandleFunc("POST /{ver}/containers/{id}/attach", attach)
	// exec start: echoes the request body, then behaves like `cat`.
	execStart := func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.WriteString(conn, upgradedHead)
		_, _ = conn.Write(append(body, '|'))
		_, _ = io.Copy(conn, buf)
	}
	mux.HandleFunc("POST /exec/{id}/start", execStart)
	mux.HandleFunc("POST /{ver}/exec/{id}/start", execStart)
	mux.HandleFunc("GET /{ver}/containers/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "line1\n")
		w.(http.Flusher).Flush()
		select {
		case <-fe.release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "line2\n")
	})
	mux.HandleFunc("GET /_ping", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.47")
		_, _ = io.WriteString(w, "OK")
	})
	fe.srv = httptest.NewServer(mux)
	t.Cleanup(fe.srv.Close)
	return fe
}

func (fe *fakeEngine) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", fe.srv.Listener.Addr().String())
}

func (fe *fakeEngine) body(t *testing.T, path string) map[string]any {
	t.Helper()
	fe.mu.Lock()
	b := fe.bodies[path]
	fe.mu.Unlock()
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("engine got invalid body for %s: %v (%q)", path, err, b)
	}
	return m
}

var leaseLabels = v1.LeaseLabels("l-test000001", "alice@mac", "s-sess000001", "proj")

type fixture struct {
	fe    *fakeEngine
	p     *Proxy
	srv   *httptest.Server
	local string
}

func newFixture(t *testing.T, mutate func(*Options)) *fixture {
	t.Helper()
	fe := newFakeEngine(t)
	local := t.TempDir()
	o := Options{
		Dial: fe.dial, Labels: leaseLabels,
		PathMaps: []PathMap{{Local: local, Remote: "/srv/dol/l-test000001/proj"}},
	}
	if mutate != nil {
		mutate(&o)
	}
	p, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(func() { srv.Close(); p.Close() })
	return &fixture{fe: fe, p: p, srv: srv, local: local}
}

func (f *fixture) post(t *testing.T, path string, body io.Reader, contentLength int64) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", f.srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = contentLength
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func labelsOf(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	l, ok := m["Labels"].(map[string]any)
	if !ok {
		t.Fatalf("no Labels object in %v", m)
	}
	return l
}

func TestContainerCreateInjectsLabelsAndRewritesBinds(t *testing.T) {
	f := newFixture(t, nil)
	data := filepath.Join(f.local, "data")
	body := fmt.Sprintf(`{
		"Image":"alpine","Labels":{"app":"web",%q:"l-spoofed"},
		"HostConfig":{
			"Memory": 9007199254740993,
			"Binds":[%q,"named:/cache","%s/conf:/etc/conf"],
			"Mounts":[{"Type":"bind","Source":%q,"Target":"/m"},{"Type":"volume","Source":"vol","Target":"/v"}],
			"PortBindings":{"80/tcp":[{"HostIp":"127.0.0.1","HostPort":"8080"}]}
		}}`, v1.LabelLease, data+":/data:ro", f.local, data)
	resp := f.post(t, "/v1.47/containers/create?name=web", strings.NewReader(body), int64(len(body)))
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status %d: %s", resp.StatusCode, b)
	}
	got := f.fe.body(t, "/containers/create")
	l := labelsOf(t, got)
	if l["app"] != "web" {
		t.Errorf("user label lost: %v", l)
	}
	for k, v := range leaseLabels {
		if l[k] != v {
			t.Errorf("label %s = %v, want %s", k, l[k], v)
		}
	}
	hc := got["HostConfig"].(map[string]any)
	if n := hc["Memory"].(json.Number).String(); n != "9007199254740993" {
		t.Errorf("large integer changed: %s", n)
	}
	binds := hc["Binds"].([]any)
	want := []any{"/srv/dol/l-test000001/proj/data:/data:ro", "named:/cache", "/srv/dol/l-test000001/proj/conf:/etc/conf"}
	if fmt.Sprint(binds) != fmt.Sprint(want) {
		t.Errorf("binds = %v, want %v", binds, want)
	}
	mounts := hc["Mounts"].([]any)
	if s := mounts[0].(map[string]any)["Source"]; s != "/srv/dol/l-test000001/proj/data" {
		t.Errorf("bind mount source = %v", s)
	}
	vm := mounts[1].(map[string]any)
	if vm["Source"] != "vol" {
		t.Errorf("volume mount source changed: %v", vm["Source"])
	}
	if vl := labelsOf(t, vm["VolumeOptions"].(map[string]any)); vl[v1.LabelLease] != "l-test000001" {
		t.Errorf("implicit volume labels missing: %v", vl)
	}
	ports := f.p.Ports()
	if len(ports) != 1 || ports[0] != (PortBinding{ContainerID: "c0ffee", ContainerName: "web", ContainerPort: "80/tcp", HostIP: "127.0.0.1", HostPort: "8080"}) {
		t.Errorf("ports = %+v", ports)
	}
}

func TestContainerCreateChunkedBodyAndNoLabels(t *testing.T) {
	f := newFixture(t, nil)
	pr, pw := io.Pipe()
	go func() {
		_, _ = io.WriteString(pw, `{"Image":"alpine",`)
		time.Sleep(10 * time.Millisecond)
		_, _ = io.WriteString(pw, `"Cmd":["true"]}`)
		_ = pw.Close()
	}()
	resp := f.post(t, "/containers/create", pr, -1) // -1: chunked
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", resp.StatusCode)
	}
	l := labelsOf(t, f.fe.body(t, "/containers/create"))
	if l[v1.LabelManaged] != "true" || len(l) != len(leaseLabels) {
		t.Errorf("labels = %v", l)
	}
}

func TestExternalBindRejectedUnlessAllowed(t *testing.T) {
	body := `{"Image":"alpine","HostConfig":{"Binds":["/etc/hosts:/h:ro"]}}`
	f := newFixture(t, nil)
	resp := f.post(t, "/v1.51/containers/create", strings.NewReader(body), int64(len(body)))
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "/etc/hosts") {
		t.Fatalf("want 400 naming the path, got %d %s", resp.StatusCode, b)
	}
	f = newFixture(t, func(o *Options) { o.AllowExternalBinds = true })
	resp = f.post(t, "/v1.51/containers/create", strings.NewReader(body), int64(len(body)))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", resp.StatusCode)
	}
	hc := f.fe.body(t, "/containers/create")["HostConfig"].(map[string]any)
	if b := hc["Binds"].([]any)[0]; b != "/etc/hosts:/h:ro" {
		t.Errorf("external bind changed: %v", b)
	}
	// No path maps (local placement): binds are never touched.
	f = newFixture(t, func(o *Options) { o.PathMaps = nil })
	resp = f.post(t, "/containers/create", strings.NewReader(body), int64(len(body)))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestNetworkAndVolumeCreate(t *testing.T) {
	f := newFixture(t, nil)
	for _, p := range []string{"/networks/create", "/volumes/create"} {
		body := `{"Name":"n1","Labels":{"team":"x"}}`
		resp := f.post(t, "/v1.47"+p, strings.NewReader(body), int64(len(body)))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("%s: status %d", p, resp.StatusCode)
		}
		got := f.fe.body(t, p)
		l := labelsOf(t, got)
		if l["team"] != "x" || l[v1.LabelLease] != "l-test000001" || got["Name"] != "n1" {
			t.Errorf("%s: body %v", p, got)
		}
	}
}

func TestCreateBodyTooLarge(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.MaxCreateBody = 64 })
	body := `{"Image":"alpine","Env":["` + strings.Repeat("x", 100) + `"]}`
	resp := f.post(t, "/containers/create", strings.NewReader(body), int64(len(body)))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestBuildLabelsAndStreamedContext(t *testing.T) {
	f := newFixture(t, nil)
	ctxData := pattern(7, 24<<20)
	want := sha256.Sum256(ctxData)
	resp := f.post(t, `/v1.47/build?t=x&labels=`+`%7B%22a%22%3A%22b%22%7D`, bytes.NewReader(ctxData), -1)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	f.fe.mu.Lock()
	n, sum, q := f.fe.buildN, f.fe.buildSum, f.fe.queries["/build"]
	f.fe.mu.Unlock()
	if n != int64(len(ctxData)) || sum != hex.EncodeToString(want[:]) {
		t.Errorf("context corrupted: %d bytes sha %s", n, sum)
	}
	vals, _ := url.ParseQuery(q)
	var labels map[string]string
	if err := json.Unmarshal([]byte(vals.Get("labels")), &labels); err != nil {
		t.Fatal(err)
	}
	if labels["a"] != "b" || labels[v1.LabelLease] != "l-test000001" || vals.Get("t") != "x" {
		t.Errorf("query = %s", q)
	}
}

// hijack sends an upgrade request over a raw connection and returns the
// connection positioned after the 101 response.
func hijack(t *testing.T, addr, path, body string) (net.Conn, *bufio.Reader) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_, _ = fmt.Fprintf(c, "POST %s HTTP/1.1\r\nHost: api.moby.localhost\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", path, len(body), body)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols || resp.Header.Get("Upgrade") != "tcp" ||
		resp.Header.Get("Content-Type") != "application/vnd.docker.raw-stream" || resp.Status != "101 UPGRADED" {
		t.Fatalf("unexpected upgrade response: %s %v", resp.Status, resp.Header)
	}
	return c, br
}

func TestHijackByteExactBothDirectionsWithHalfClose(t *testing.T) {
	f := newFixture(t, nil)
	c, br := hijack(t, f.srv.Listener.Addr().String(), "/v1.47/containers/abc/attach?stream=1&stdin=1&stdout=1", "")
	stdin := pattern(2, 5<<20)
	go func() {
		_, _ = c.Write(stdin)
		_ = c.(*net.TCPConn).CloseWrite() // stdin EOF, keep reading
	}()
	out, err := io.ReadAll(br)
	if err != nil {
		t.Fatal(err)
	}
	stream := pattern(1, 3<<20)
	if !bytes.Equal(out[:min(len(out), len(stream))], stream) {
		t.Fatalf("engine→client stream corrupted (%d bytes)", len(out))
	}
	sum := sha256.Sum256(stdin)
	if trailer := string(out[len(stream):]); trailer != "\nsha256:"+hex.EncodeToString(sum[:]) {
		t.Fatalf("client→engine stream corrupted or half-close lost: trailer %q", trailer)
	}
}

func TestExecStartForwardsBodyAndEchoes(t *testing.T) {
	f := newFixture(t, nil)
	for _, tty := range []bool{true, false} {
		body := fmt.Sprintf(`{"Detach":false,"Tty":%v}`, tty)
		c, br := hijack(t, f.srv.Listener.Addr().String(), "/v1.47/exec/e1/start", body)
		_, _ = io.WriteString(c, "hello\x00world")
		_ = c.(*net.TCPConn).CloseWrite()
		out, _ := io.ReadAll(br)
		if string(out) != body+"|hello\x00world" {
			t.Errorf("tty=%v: got %q", tty, out)
		}
	}
}

func TestHijackErrorIsRelayed(t *testing.T) {
	f := newFixture(t, nil)
	c, err := net.Dial("tcp", f.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_, _ = io.WriteString(c, "POST /containers/missing/attach HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 0\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 404 || !strings.Contains(string(b), "No such container") {
		t.Fatalf("got %d %s", resp.StatusCode, b)
	}
}

func TestStreamingResponseIsFlushed(t *testing.T) {
	f := newFixture(t, nil)
	resp, err := http.Get(f.srv.URL + "/v1.47/containers/abc/logs?follow=1&stdout=1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	br := bufio.NewReader(resp.Body)
	done := make(chan string, 1)
	go func() { l, _ := br.ReadString('\n'); done <- l }()
	select {
	case l := <-done:
		if l != "line1\n" {
			t.Fatalf("got %q", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first log line not delivered before the stream ended: proxy buffers")
	}
	close(f.fe.release)
	if l, _ := br.ReadString('\n'); l != "line2\n" {
		t.Fatalf("got %q", l)
	}
}

func TestUnixSocketServer(t *testing.T) {
	fe := newFakeEngine(t)
	p, err := New(Options{Dial: fe.dial, Labels: leaseLabels})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "dp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "sub", "l-test000001.sock")
	// A stale socket from a crashed process is replaced.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()

	s, err := Listen(path, p)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v err %v", fi.Mode().Perm(), err)
	}
	if _, err := Listen(path, p); err == nil {
		t.Fatal("second Listen on a live socket must fail")
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
	resp, err := client.Get("http://docker/_ping")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(b) != "OK" || resp.Header.Get("Api-Version") != "1.47" {
		t.Fatalf("ping: %q %v", b, resp.Header)
	}
	if s.DockerHost() != "unix://"+path {
		t.Errorf("DockerHost = %s", s.DockerHost())
	}

	// Close also ends hijacked streams that are still open.
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(c, "POST /exec/e1/start HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: 2\r\n\r\n{}")
	br := bufio.NewReader(c)
	if _, err := http.ReadResponse(br, nil); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadAll(br); err != nil {
		t.Fatalf("hijacked stream not closed by Server.Close: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("socket not removed: %v", err)
	}
	if _, err := Listen(filepath.Join(dir, strings.Repeat("x", 120)+".sock"), p); err == nil {
		t.Error("over-long socket path must be rejected")
	}
}

func TestAPIPathAndClassify(t *testing.T) {
	for in, want := range map[string]string{
		"/v1.47/containers/create": "/containers/create",
		"/v1/containers/create":    "/containers/create",
		"/containers/create":       "/containers/create",
		"/v1.47":                   "/v1.47",
		"/version":                 "/version",
	} {
		if got := apiPath(in); got != want {
			t.Errorf("apiPath(%q) = %q, want %q", in, got, want)
		}
	}
	if classify("GET", "/containers/create") != epOther || classify("POST", "/v1.51/build") != epBuild ||
		classify("POST", "/v1.51/containers/abc/start") != epOther {
		t.Error("classify mismatch")
	}
}

func TestEndpointDialer(t *testing.T) {
	if _, err := EndpointDialer(sshx.Options{}, "tcp://1.2.3.4:2375"); err == nil {
		t.Error("tcp endpoints must be refused")
	}
	if _, err := EndpointDialer(sshx.Options{}, "ssh://-oProxyCommand=x"); err == nil {
		t.Error("option-like ssh host must be refused")
	}
	if _, err := EndpointDialer(sshx.Options{}, "unix:///var/run/docker.sock"); err != nil {
		t.Error(err)
	}
	if _, err := EndpointDialer(sshx.Options{}, "ssh://dol@tower:2222"); err != nil {
		t.Error(err)
	}
}
