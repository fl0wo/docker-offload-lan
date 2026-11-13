// Package dockerproxy is a Docker Engine API proxy (ADR-0040, milestone M6
// spike). It forwards every request to one upstream engine (a local socket
// or `ssh <node> docker system dial-stdio`) and transparently:
//
//   - injects lease labels into POST /containers/create, /networks/create,
//     /volumes/create bodies and the `labels` query of POST /build;
//   - rewrites bind-mount sources under the synced project directory
//     (HostConfig.Binds and HostConfig.Mounts) to the node path;
//   - records published ports of created containers.
//
// Everything else is streamed untouched: response bodies are flushed as they
// arrive (logs -f, events, pull/build progress), request bodies (build
// contexts, image loads) are streamed, and upgraded/hijacked connections
// (attach, exec start, BuildKit /session and /grpc) are spliced byte for byte
// with half-close propagated in both directions. Only create bodies are
// buffered, bounded by Options.MaxCreateBody.
package dockerproxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DialFunc opens a new connection to the upstream Docker engine.
type DialFunc func(ctx context.Context) (net.Conn, error)

// Options configure a Proxy.
type Options struct {
	// Dial reaches the upstream engine. Required.
	Dial DialFunc
	// Labels are injected on create calls. Only keys under v1.LabelPrefix
	// are used.
	Labels map[string]string
	// PathMaps rewrite bind-mount sources (remote targets only). Empty
	// disables bind rewriting.
	PathMaps []PathMap
	// AllowExternalBinds passes absolute bind sources outside every PathMap
	// through unchanged (they then name paths on the target). When false and
	// PathMaps is non-empty, such creates are refused with a 400.
	AllowExternalBinds bool
	// OnPorts is called (synchronously, keep it fast) after a container with
	// published ports was created.
	OnPorts func([]PortBinding)
	// MaxCreateBody bounds buffered create bodies; default 8 MiB.
	MaxCreateBody int64
	Logger        *slog.Logger
}

// Proxy is an http.Handler serving the Docker Engine API.
type Proxy struct {
	o  Options
	rw rewriter
	rp *httputil.ReverseProxy
	tr *http.Transport

	mu     sync.Mutex
	conns  map[net.Conn]struct{} // live hijacked connections (both ends)
	closed bool
	ports  []PortBinding
}

// New returns a Proxy.
func New(o Options) (*Proxy, error) {
	if o.Dial == nil {
		return nil, errors.New("dockerproxy: Options.Dial is required")
	}
	if o.MaxCreateBody <= 0 {
		o.MaxCreateBody = 8 << 20
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	p := &Proxy{o: o, conns: map[net.Conn]struct{}{}}
	p.rw = rewriter{labels: ourLabels(o.Labels), maps: o.PathMaps, allowExternal: o.AllowExternalBinds}
	p.tr = &http.Transport{
		DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return o.Dial(ctx) },
		DisableCompression:  true,
		MaxIdleConns:        8,
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     60 * time.Second,
		// Larger than the 4 KiB defaults: build contexts and image loads
		// stream hundreds of MB through here.
		WriteBufferSize: 64 << 10,
		ReadBufferSize:  64 << 10,
	}
	target := &url.URL{Scheme: "http", Host: "docker"}
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
		},
		Transport:      p.tr,
		FlushInterval:  -1, // stream: logs -f, events, progress
		ModifyResponse: p.modifyResponse,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // client went away
			}
			p.o.Logger.Warn("docker proxy upstream error", "path", r.URL.Path, "err", err)
			writeError(w, http.StatusBadGateway, "docker-offload-lan proxy: upstream engine: "+err.Error())
		},
	}
	return p, nil
}

// Ports returns the published ports recorded so far.
func (p *Proxy) Ports() []PortBinding {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]PortBinding(nil), p.ports...)
}

// Close drops idle upstream connections and closes every hijacked stream.
func (p *Proxy) Close() {
	p.mu.Lock()
	p.closed = true
	conns := p.conns
	p.conns = map[net.Conn]struct{}{}
	p.mu.Unlock()
	for c := range conns {
		_ = c.Close()
	}
	p.tr.CloseIdleConnections()
}

func (p *Proxy) track(cs ...net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	for _, c := range cs {
		p.conns[c] = struct{}{}
	}
	return true
}

func (p *Proxy) untrack(cs ...net.Conn) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range cs {
		delete(p.conns, c)
	}
}

type ctxKey int

const portsKey ctxKey = 0

// ServeHTTP implements http.Handler.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch classify(r.Method, r.URL.Path) {
	case epContainerCreate:
		var ports []PortBinding
		ok := p.rewriteRequest(w, r, func(m map[string]any) error {
			var err error
			ports, err = p.rw.rewriteContainer(m)
			return err
		})
		if !ok {
			return
		}
		if len(ports) > 0 {
			name := r.URL.Query().Get("name")
			for i := range ports {
				ports[i].ContainerName = name
			}
			r = r.WithContext(context.WithValue(r.Context(), portsKey, ports))
		}
	case epNetworkCreate, epVolumeCreate:
		if !p.rewriteRequest(w, r, p.rw.mergeLabels) {
			return
		}
	case epBuild:
		q := r.URL.Query()
		if err := p.rw.rewriteBuildQuery(q); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		r.URL.RawQuery = q.Encode()
	}
	if isUpgrade(r.Header) {
		p.serveUpgrade(w, r)
		return
	}
	p.rp.ServeHTTP(w, r)
}

// rewriteRequest buffers (bounded) and rewrites a JSON create body in place.
func (p *Proxy) rewriteRequest(w http.ResponseWriter, r *http.Request, fn func(map[string]any) error) bool {
	b, err := io.ReadAll(io.LimitReader(r.Body, p.o.MaxCreateBody+1))
	_ = r.Body.Close()
	if err != nil {
		writeError(w, http.StatusBadRequest, "reading request body: "+err.Error())
		return false
	}
	if int64(len(b)) > p.o.MaxCreateBody {
		writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("docker-offload-lan proxy: create body larger than %d bytes", p.o.MaxCreateBody))
		return false
	}
	out, err := rewriteBody(b, fn)
	if err != nil {
		var re *rejectError
		if errors.As(err, &re) {
			writeError(w, http.StatusBadRequest, re.msg)
		} else {
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(out))
	r.ContentLength = int64(len(out))
	r.Header.Set("Content-Length", strconv.Itoa(len(out)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Del("Transfer-Encoding")
	r.TransferEncoding = nil
	return true
}

// modifyResponse records the container ID of creates with published ports.
func (p *Proxy) modifyResponse(resp *http.Response) error {
	ports, _ := resp.Request.Context().Value(portsKey).([]PortBinding)
	if len(ports) == 0 || resp.StatusCode != http.StatusCreated {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	var created struct {
		ID string `json:"Id"`
	}
	_ = json.Unmarshal(b, &created)
	for i := range ports {
		ports[i].ContainerID = created.ID
	}
	p.mu.Lock()
	p.ports = append(p.ports, ports...)
	p.mu.Unlock()
	if p.o.OnPorts != nil {
		p.o.OnPorts(ports)
	}
	return nil
}

// isUpgrade reports whether the request asks for a connection upgrade
// (`Connection: Upgrade`), as the Docker client does for attach, exec start
// and BuildKit's /session and /grpc.
func isUpgrade(h http.Header) bool {
	for _, v := range h.Values("Connection") {
		for t := range strings.SplitSeq(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), "upgrade") {
				return true
			}
		}
	}
	return false
}

// serveUpgrade forwards an upgrade request on a dedicated upstream
// connection and, on 101 Switching Protocols, splices the two connections.
// It cannot use httputil.ReverseProxy's upgrade support: that closes both
// sides as soon as one direction ends, which truncates the output of
// `echo x | docker exec -i c cat` (the client half-closes stdin first).
func (p *Proxy) serveUpgrade(w http.ResponseWriter, r *http.Request) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		writeError(w, http.StatusInternalServerError, "docker-offload-lan proxy: connection cannot be hijacked")
		return
	}
	up, err := p.o.Dial(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "docker-offload-lan proxy: upstream engine: "+err.Error())
		return
	}
	out := r.Clone(r.Context())
	out.RequestURI = ""
	out.URL.Scheme, out.URL.Host = "", ""
	if out.Host == "" {
		out.Host = "docker"
	}
	// Request.Write sends the (small) body: exec start config, or nothing.
	if err := out.Write(up); err != nil {
		_ = up.Close()
		writeError(w, http.StatusBadGateway, "docker-offload-lan proxy: writing to upstream engine: "+err.Error())
		return
	}
	upr := bufio.NewReader(up)
	resp, err := http.ReadResponse(upr, out)
	if err != nil {
		_ = up.Close()
		writeError(w, http.StatusBadGateway, "docker-offload-lan proxy: reading upstream response: "+err.Error())
		return
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		// An error (404 no such container, 409 not running) or, for very
		// old daemons, a raw stream: relay it as a normal response.
		defer func() { _ = up.Close() }()
		defer func() { _ = resp.Body.Close() }()
		maps.Copy(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(flushWriter{w}, resp.Body)
		return
	}

	client, cbuf, err := hj.Hijack()
	if err != nil {
		_ = up.Close()
		return
	}
	_ = client.SetDeadline(time.Time{})
	if !p.track(client, up) {
		_ = client.Close()
		_ = up.Close()
		return
	}
	defer p.untrack(client, up)

	// Relay the 101 exactly as received (status text, headers) and any
	// bytes the engine already sent after it.
	var hdr bytes.Buffer
	fmt.Fprintf(&hdr, "HTTP/%d.%d %s\r\n", resp.ProtoMajor, resp.ProtoMinor, resp.Status)
	_ = resp.Header.Write(&hdr)
	hdr.WriteString("\r\n")
	if _, err := cbuf.Write(hdr.Bytes()); err != nil || cbuf.Flush() != nil {
		_ = client.Close()
		_ = up.Close()
		return
	}

	splice(client, cbuf.Reader, up, upr)
}

// splice copies client→upstream and upstream→client. When the client
// finishes sending (stdin EOF) the upstream write side is half-closed and
// output keeps flowing; when the upstream finishes, the client write side is
// half-closed and both connections are closed (the engine closes the stream
// when the process or session ends, so nothing more can be delivered).
func splice(client net.Conn, clientR io.Reader, up net.Conn, upR io.Reader) {
	toUp := make(chan struct{})
	go func() {
		defer close(toUp)
		_, _ = io.Copy(up, clientR)
		closeWrite(up)
	}()
	_, _ = io.Copy(client, upR)
	closeWrite(client)
	// Give an in-flight client→upstream copy a moment to drain, then close.
	select {
	case <-toUp:
	case <-time.After(100 * time.Millisecond):
	}
	_ = up.Close()
	_ = client.Close()
	<-toUp
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

type flushWriter struct{ w http.ResponseWriter }

func (f flushWriter) Write(b []byte) (int, error) {
	n, err := f.w.Write(b)
	if fl, ok := f.w.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// writeError writes a Docker-style error body, which the docker CLI prints
// as "Error response from daemon: <message>".
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
}
