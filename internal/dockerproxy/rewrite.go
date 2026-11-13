package dockerproxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/fl0wo/docker-offload-lan/internal/compose"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// endpoint classifies the API calls the proxy rewrites. Everything else is
// streamed through untouched.
type endpoint int

const (
	epOther endpoint = iota
	epContainerCreate
	epNetworkCreate
	epVolumeCreate
	epBuild
)

var versionPrefix = regexp.MustCompile(`^/v[0-9]+(\.[0-9]+)*/`)

// apiPath strips an API version prefix: /v1.47/containers/create →
// /containers/create.
func apiPath(p string) string {
	if loc := versionPrefix.FindStringIndex(p); loc != nil {
		return p[loc[1]-1:]
	}
	return p
}

func classify(method, path string) endpoint {
	if method != "POST" {
		return epOther
	}
	switch strings.TrimSuffix(apiPath(path), "/") {
	case "/containers/create":
		return epContainerCreate
	case "/networks/create":
		return epNetworkCreate
	case "/volumes/create":
		return epVolumeCreate
	case "/build":
		return epBuild
	}
	return epOther
}

// PathMap maps a client directory to the directory it is synced to on the
// target (brief §7.10: <remote_root>/<lease>/<project>).
type PathMap struct {
	Local  string
	Remote string
}

// rewriter holds the per-lease rewrite policy.
type rewriter struct {
	labels        map[string]string
	maps          []PathMap
	allowExternal bool
}

// rejectError is returned for requests the proxy refuses; it becomes a
// Docker-style {"message": …} 400 response.
type rejectError struct{ msg string }

func (e *rejectError) Error() string { return e.msg }

// mergeLabels adds the lease labels to an object's "Labels" member. Labels
// that are not ours are never changed. Keys under v1.LabelPrefix always get
// the lease's value: a resource created through a lease socket belongs to
// that lease, and the reaper and ownership checks rely on it (ADR-0006).
func (r *rewriter) mergeLabels(obj map[string]any) error {
	var labels map[string]any
	switch l := obj["Labels"].(type) {
	case nil:
		labels = map[string]any{}
	case map[string]any:
		labels = l
	default:
		return &rejectError{fmt.Sprintf("Labels must be an object, got %T", l)}
	}
	for k, v := range r.labels {
		labels[k] = v
	}
	obj["Labels"] = labels
	return nil
}

// mapSource rewrites an absolute bind source. ok=false means the source is
// outside every mapped directory.
func (r *rewriter) mapSource(src string) (string, bool) {
	if len(r.maps) == 0 || !filepath.IsAbs(src) {
		return src, true
	}
	for _, m := range r.maps {
		if out, ok := compose.MapPath(m.Local, m.Remote, src); ok {
			return out, true
		}
	}
	return src, false
}

// rewriteContainer injects labels and rewrites bind sources of a
// POST /containers/create body. It returns the published ports it saw.
func (r *rewriter) rewriteContainer(body map[string]any) ([]PortBinding, error) {
	if err := r.mergeLabels(body); err != nil {
		return nil, err
	}
	hc, _ := body["HostConfig"].(map[string]any)
	if hc == nil {
		return nil, nil
	}
	var outside []string
	if binds, ok := hc["Binds"].([]any); ok {
		for i, b := range binds {
			s, ok := b.(string)
			if !ok {
				continue
			}
			src, rest, hasRest := strings.Cut(s, ":")
			if !hasRest || !strings.HasPrefix(src, "/") {
				continue // named volume or anonymous volume
			}
			mapped, ok := r.mapSource(src)
			if !ok {
				outside = append(outside, src)
				continue
			}
			binds[i] = mapped + ":" + rest
		}
	}
	if mounts, ok := hc["Mounts"].([]any); ok {
		for _, m := range mounts {
			mm, _ := m.(map[string]any)
			if mm == nil {
				continue
			}
			if strings.EqualFold(fmt.Sprint(mm["Type"]), "volume") {
				// Labels for a named volume the engine creates implicitly
				// (`--mount type=volume,src=new`). The engine ignores them
				// when the volume already exists.
				vo, _ := mm["VolumeOptions"].(map[string]any)
				if vo == nil {
					vo = map[string]any{}
				}
				if err := r.mergeLabels(vo); err != nil {
					return nil, err
				}
				mm["VolumeOptions"] = vo
				continue
			}
			if !strings.EqualFold(fmt.Sprint(mm["Type"]), "bind") {
				continue
			}
			src, _ := mm["Source"].(string)
			mapped, ok := r.mapSource(src)
			if !ok {
				outside = append(outside, src)
				continue
			}
			mm["Source"] = mapped
		}
	}
	if len(outside) > 0 && !r.allowExternal {
		return nil, &rejectError{fmt.Sprintf(
			"docker-offload-lan proxy: bind mount source(s) %s are outside the synced project directory and do not exist on the node; "+
				"move them into the project (and call dol_sync) or set proxy.allow_external_binds to use node paths",
			strings.Join(outside, ", "))}
	}
	return portBindings(hc), nil
}

// PortBinding is a published port requested on container create.
type PortBinding struct {
	ContainerID   string `json:"container_id,omitempty"`
	ContainerName string `json:"container_name,omitempty"`
	ContainerPort string `json:"container_port"` // e.g. "80/tcp"
	HostIP        string `json:"host_ip,omitempty"`
	HostPort      string `json:"host_port,omitempty"` // "" = ephemeral, resolved at start
}

func portBindings(hc map[string]any) []PortBinding {
	pb, _ := hc["PortBindings"].(map[string]any)
	keys := make([]string, 0, len(pb))
	for k := range pb {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []PortBinding
	for _, k := range keys {
		list, _ := pb[k].([]any)
		for _, e := range list {
			em, _ := e.(map[string]any)
			ip, _ := em["HostIp"].(string)
			port, _ := em["HostPort"].(string)
			out = append(out, PortBinding{ContainerPort: k, HostIP: ip, HostPort: port})
		}
	}
	return out
}

// rewriteBody decodes a JSON create body, applies fn and re-encodes it.
// Numbers are kept verbatim (json.Number) so large integers survive.
func rewriteBody(b []byte, fn func(map[string]any) error) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var body map[string]any
	if len(bytes.TrimSpace(b)) == 0 {
		body = map[string]any{}
	} else if err := dec.Decode(&body); err != nil {
		return nil, &rejectError{"invalid JSON body: " + err.Error()}
	}
	if body == nil {
		body = map[string]any{}
	}
	if err := fn(body); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// rewriteBuildQuery merges the lease labels into the `labels` query
// parameter of POST /build (a JSON object; the legacy builder applies it to
// the resulting image).
func (r *rewriter) rewriteBuildQuery(q url.Values) error {
	labels := map[string]string{}
	if s := q.Get("labels"); s != "" {
		if err := json.Unmarshal([]byte(s), &labels); err != nil {
			return &rejectError{"invalid labels query parameter: " + err.Error()}
		}
	}
	for k, v := range r.labels {
		labels[k] = v
	}
	b, err := json.Marshal(labels)
	if err != nil {
		return err
	}
	q.Set("labels", string(b))
	return nil
}

// ourLabels returns only labels under v1.LabelPrefix (defensive: the proxy
// must not invent labels outside our namespace).
func ourLabels(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		if strings.HasPrefix(k, v1.LabelPrefix+".") {
			out[k] = v
		}
	}
	return out
}
