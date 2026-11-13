package node

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/kindx"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

const kindOpTimeout = 15 * time.Minute

func (s *Server) kindError(op string, err error) *v1.Error {
	hint := "check that the node has enough memory and Docker works; details in `journalctl -u dol-node` on the node"
	var ee *exec.Error
	if errors.As(err, &ee) {
		hint = "kind is not installed on the node: re-run deploy/linux/install.sh (it pins the kind version) or set kind.binary in node.yaml"
	} else if strings.Contains(err.Error(), "not present locally") {
		hint = "build the image on the node first with the docker prefix from dol_acquire; images are never copied from your laptop"
	}
	return v1.Errorf(v1.CodeKindFailed, hint, "%s failed on node %s: %v", op, s.cfg.Name, err)
}

func findCluster(l *v1.Lease, name string) (v1.KindCluster, bool) {
	for _, c := range l.Resources.KindClusters {
		if c.Name == name || c.ClusterName == name {
			return c, true
		}
	}
	return v1.KindCluster{}, false
}

func (s *Server) handleKindCreate(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req v1.KindCreateRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	cluster, err := kindx.ClusterName(id, req.Name)
	if err != nil {
		writeErr(w, err)
		return
	}
	mode := req.AccessMode
	if mode == "" {
		mode = s.cfg.Kind.AccessMode
	}
	if mode != v1.AccessTunnel && mode != v1.AccessLAN {
		writeErr(w, v1.Errorf(v1.CodeBadRequest, "use access_mode tunnel or lan", "invalid access_mode %q", mode))
		return
	}
	var sans []string
	if mode == v1.AccessLAN {
		sans = s.lanAddrs()
	}
	// Validate the user config before allocating anything.
	if _, _, err := kindx.GenerateConfig(req.ConfigYAML, kindx.ConfigOptions{AccessMode: mode, Port: s.cfg.Kind.PortRange[0], CertSANs: sans}); err != nil {
		writeErr(w, err)
		return
	}
	// kind create outlives a disconnecting client; the lease records the
	// cluster before creation so a crash still leaves it tracked for teardown.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), kindOpTimeout)
	defer cancel()
	lk := s.opLock(id)
	lk.Lock()
	defer lk.Unlock()

	s.mu.Lock()
	l, e := s.activeLease(ctx, id)
	if e != nil {
		s.mu.Unlock()
		writeErr(w, e)
		return
	}
	if _, ok := findCluster(l, req.Name); ok {
		s.mu.Unlock()
		writeErr(w, v1.Errorf(v1.CodeConflict, "delete it first with dol_kind_delete or choose another name",
			"kind cluster %q already exists on lease %s", req.Name, id))
		return
	}
	used, err := s.store.UsedPorts(ctx)
	if err != nil {
		s.mu.Unlock()
		writeErr(w, s.internal(err, "reading port allocations"))
		return
	}
	port, err := kindx.AllocatePort(s.cfg.Kind.PortRange[0], s.cfg.Kind.PortRange[1], used, s.portFree)
	if err != nil {
		s.mu.Unlock()
		writeErr(w, v1.Errorf(v1.CodeConflict, "delete unused kind clusters or widen kind.port_range in node.yaml",
			"cannot allocate an API server port on node %s: %v", s.cfg.Name, err))
		return
	}
	kc := v1.KindCluster{Name: req.Name, ClusterName: cluster, APIServerPort: port, AccessMode: mode}
	if err := s.store.AddPort(ctx, port, id, cluster); err != nil {
		s.mu.Unlock()
		writeErr(w, s.internal(err, "persisting port allocation"))
		return
	}
	l.Resources.KindClusters = append(l.Resources.KindClusters, kc)
	if err := s.store.Update(ctx, l); err != nil {
		_ = s.store.FreePort(ctx, cluster)
		s.mu.Unlock()
		writeErr(w, s.internal(err, "registering kind cluster"))
		return
	}
	s.mu.Unlock()

	resp, kerr := s.createCluster(ctx, req, kc, sans)
	if kerr != nil {
		// Roll back: delete whatever kind left behind, untrack, free the port.
		if err := s.kind.Delete(ctx, cluster); err != nil {
			s.log.Warn("cleanup after failed kind create", "cluster", cluster, "err", err)
		} else {
			s.untrackCluster(ctx, id, cluster)
		}
		s.log.Warn("kind create failed", "lease", id, "cluster", cluster, "err", kerr.Message)
		writeErr(w, kerr)
		return
	}
	s.log.Info("kind cluster created", "lease", id, "cluster", cluster, "port", port, "access_mode", mode)
	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) createCluster(ctx context.Context, req v1.KindCreateRequest, kc v1.KindCluster, sans []string) (*v1.KindCreateResponse, *v1.Error) {
	cfg, warns, err := kindx.GenerateConfig(req.ConfigYAML, kindx.ConfigOptions{AccessMode: kc.AccessMode, Port: kc.APIServerPort, CertSANs: sans})
	if err != nil {
		if e, ok := v1.AsError(err); ok {
			return nil, e
		}
		return nil, s.internal(err, "generating kind config")
	}
	for _, w := range warns {
		s.log.Info("kind config adjusted", "cluster", kc.ClusterName, "warning", w)
	}
	dir := filepath.Join(s.cfg.DataDir, "kind")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, s.internal(err, "creating kind work dir")
	}
	cfgPath := filepath.Join(dir, kc.ClusterName+".yaml")
	kubePath := filepath.Join(dir, kc.ClusterName+".kubeconfig")
	defer func() { _ = os.Remove(kubePath) }()
	if err := os.WriteFile(cfgPath, cfg, 0o600); err != nil {
		return nil, s.internal(err, "writing kind config")
	}
	if err := s.kind.Create(ctx, kc.ClusterName, cfgPath, kubePath, s.kindWait); err != nil {
		return nil, s.kindError("kind create cluster "+kc.ClusterName, err)
	}
	kube, err := os.ReadFile(kubePath)
	if err != nil {
		return nil, s.kindError("reading the kubeconfig of "+kc.ClusterName, err)
	}
	pms, _ := kindx.PortMappings(cfg)
	host := "127.0.0.1"
	if kc.AccessMode == v1.AccessLAN && len(sans) > 0 {
		host = sans[0]
	}
	return &v1.KindCreateResponse{
		Cluster: kc.ClusterName, Kubeconfig: string(kube),
		APIServer: v1.HostPort{Host: host, Port: kc.APIServerPort}, AccessMode: kc.AccessMode, PortMappings: pms,
	}, nil
}

func (s *Server) untrackCluster(ctx context.Context, id, cluster string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.store.FreePort(ctx, cluster); err != nil {
		s.log.Warn("freeing kind port", "cluster", cluster, "err", err)
	}
	l, err := s.store.Get(ctx, id)
	if err != nil {
		return
	}
	var kept []v1.KindCluster
	for _, c := range l.Resources.KindClusters {
		if c.ClusterName != cluster {
			kept = append(kept, c)
		}
	}
	l.Resources.KindClusters = nonNilClusters(kept)
	if err := s.store.Update(ctx, l); err != nil {
		s.log.Warn("untracking kind cluster", "cluster", cluster, "err", err)
	}
}

// clusterOf resolves {id}/{name} to a tracked cluster of an active lease.
func (s *Server) clusterOf(ctx context.Context, id, name string) (v1.KindCluster, *v1.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, e := s.activeLease(ctx, id)
	if e != nil {
		return v1.KindCluster{}, e
	}
	c, ok := findCluster(l, name)
	if !ok {
		return c, v1.Errorf(v1.CodeNotFound, "list the lease's clusters with GET /v1/leases/{id} (resources.kind_clusters)",
			"kind cluster %q is not tracked on lease %s", name, id)
	}
	return c, nil
}

func (s *Server) handleKindLoad(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("name")
	var req v1.KindLoadRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, err)
		return
	}
	if len(req.Images) == 0 {
		writeErr(w, v1.Errorf(v1.CodeBadRequest, "pass at least one image name, e.g. [\"myapp:dev\"]", "images is empty"))
		return
	}
	for _, img := range req.Images {
		if img == "" || strings.HasPrefix(img, "-") || strings.ContainsAny(img, " \t\n") {
			writeErr(w, v1.Errorf(v1.CodeBadRequest, "pass image references like repo/name:tag", "invalid image reference %q", img))
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), kindOpTimeout)
	defer cancel()
	lk := s.opLock(id)
	lk.Lock()
	defer lk.Unlock()
	c, e := s.clusterOf(ctx, id, name)
	if e != nil {
		writeErr(w, e)
		return
	}
	if err := s.kind.Load(ctx, c.ClusterName, req.Images); err != nil {
		writeErr(w, s.kindError("kind load docker-image into "+c.ClusterName, err))
		return
	}
	s.log.Info("images loaded into kind cluster", "lease", id, "cluster", c.ClusterName, "images", req.Images)
	writeJSON(w, http.StatusOK, v1.KindLoadResponse{Loaded: req.Images})
}

// KindDeleteResponse is the body of DELETE /v1/leases/{id}/kind/{name}.
type KindDeleteResponse struct {
	Deleted string `json:"deleted"`
}

func (s *Server) handleKindDelete(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("name")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), kindOpTimeout)
	defer cancel()
	lk := s.opLock(id)
	lk.Lock()
	defer lk.Unlock()
	c, e := s.clusterOf(ctx, id, name)
	if e != nil {
		writeErr(w, e)
		return
	}
	if err := s.kind.Delete(ctx, c.ClusterName); err != nil {
		writeErr(w, s.kindError("kind delete cluster "+c.ClusterName, err))
		return
	}
	s.untrackCluster(ctx, id, c.ClusterName)
	s.log.Info("kind cluster deleted", "lease", id, "cluster", c.ClusterName)
	writeJSON(w, http.StatusOK, KindDeleteResponse{Deleted: c.ClusterName})
}
