// Package compose renders a compose project, rewrites bind mounts for a
// remote node and injects lease labels (brief §7.10).
package compose

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// Runner runs the docker CLI (dockerx.ExecRunner satisfies it).
type Runner interface {
	Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error)
}

// Doc is a normalized compose model as produced by `docker compose config`.
type Doc map[string]any

// Render runs `docker compose -p <project> -f <files…> config --format json`
// locally (no daemon needed) in workDir, so .env and relative paths resolve
// exactly as the user's own `docker compose` would.
func Render(ctx context.Context, r Runner, workDir, project string, files []string) (Doc, error) {
	args := []string{"compose", "--project-directory", workDir}
	if project != "" {
		args = append(args, "-p", project)
	}
	for _, f := range files {
		if !filepath.IsAbs(f) {
			f = filepath.Join(workDir, f)
		}
		args = append(args, "-f", f)
	}
	args = append(args, "config", "--format", "json")
	out, err := r.Run(ctx, nil, args...)
	if err != nil {
		return nil, v1.Errorf(v1.CodeBadRequest, "run `docker compose config` in the project to see the problem",
			"rendering compose project failed: %v", err)
	}
	var d Doc
	if err := json.Unmarshal(out, &d); err != nil {
		return nil, fmt.Errorf("parsing compose config output: %w", err)
	}
	return d, nil
}

// Name returns the project name chosen by compose.
func (d Doc) Name() string { s, _ := d["name"].(string); return s }

func m(v any) map[string]any { x, _ := v.(map[string]any); return x }

func sortedKeys(x map[string]any) []string {
	ks := make([]string, 0, len(x))
	for k := range x {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// realpath resolves symlinks where possible (macOS /tmp → /private/tmp).
func realpath(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	// Resolve the longest existing prefix.
	dir, base := filepath.Split(p)
	if dir == "" || dir == p {
		return p
	}
	return filepath.Join(realpath(strings.TrimSuffix(dir, "/")), base)
}

func within(root, p string) (string, bool) {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// MapPath maps the absolute path src under localDir to the same relative
// path under remoteDir (symlinks such as macOS /tmp → /private/tmp are
// resolved on both sides). ok is false when src is not under localDir.
func MapPath(localDir, remoteDir, src string) (mapped string, ok bool) {
	rel, ok := within(realpath(localDir), realpath(src))
	if !ok {
		rel, ok = within(filepath.Clean(localDir), filepath.Clean(src))
	}
	if !ok {
		return "", false
	}
	return filepath.ToSlash(filepath.Join(remoteDir, rel)), true
}

// Rewrite maps every bind-mount source (and file-based secrets/configs) under
// localDir to remoteDir. Sources outside localDir are an error unless
// allowExternal, in which case they are left unchanged (they then refer to
// paths on the node) and reported as warnings.
func (d Doc) Rewrite(localDir, remoteDir string, allowExternal bool) (warnings []string, err error) {
	var outside []string
	mapPath := func(where, src string) string {
		if src == "" || !filepath.IsAbs(src) {
			return src
		}
		mapped, ok := MapPath(localDir, remoteDir, src)
		if !ok {
			outside = append(outside, where+": "+src)
			return src
		}
		return mapped
	}
	services := m(d["services"])
	for _, name := range sortedKeys(services) {
		svc := m(services[name])
		vols, _ := svc["volumes"].([]any)
		for _, v := range vols {
			vm := m(v)
			if vm == nil || vm["type"] != "bind" {
				continue
			}
			src, _ := vm["source"].(string)
			vm["source"] = mapPath("services."+name+".volumes", src)
		}
	}
	for _, top := range []string{"secrets", "configs"} {
		items := m(d[top])
		for _, k := range sortedKeys(items) {
			it := m(items[k])
			if f, ok := it["file"].(string); ok {
				it["file"] = mapPath(top+"."+k, f)
			}
		}
	}
	if len(outside) > 0 {
		if !allowExternal {
			return nil, v1.Errorf(v1.CodeBadRequest,
				"move the files into the project directory, or pass allow_external_mounts=true if those paths exist on the node",
				"bind mounts outside the project directory cannot be synced to the node: %s", strings.Join(outside, "; "))
		}
		for _, o := range outside {
			warnings = append(warnings, "external bind mount kept as a node path: "+o)
		}
	}
	return warnings, nil
}

func addLabels(obj map[string]any, labels map[string]string) {
	switch l := obj["labels"].(type) {
	case map[string]any:
		for k, v := range labels {
			l[k] = v
		}
	case []any: // ["k=v", …] (not produced by config, but be lenient)
		mm := map[string]any{}
		for _, e := range l {
			if s, ok := e.(string); ok {
				k, v, _ := strings.Cut(s, "=")
				mm[k] = v
			}
		}
		for k, v := range labels {
			mm[k] = v
		}
		obj["labels"] = mm
	default:
		mm := map[string]any{}
		for k, v := range labels {
			mm[k] = v
		}
		obj["labels"] = mm
	}
}

func external(obj map[string]any) bool {
	switch e := obj["external"].(type) {
	case bool:
		return e
	case map[string]any:
		return true
	}
	return false
}

// InjectLabels adds lease labels to every service, and to every non-external
// network and volume.
func (d Doc) InjectLabels(labels map[string]string) {
	services := m(d["services"])
	for _, k := range sortedKeys(services) {
		if svc := m(services[k]); svc != nil {
			addLabels(svc, labels)
		}
	}
	for _, top := range []string{"networks", "volumes"} {
		items := m(d[top])
		for _, k := range sortedKeys(items) {
			it := m(items[k])
			if it == nil {
				it = map[string]any{}
				items[k] = it
			}
			if !external(it) {
				addLabels(it, labels)
			}
		}
	}
}

// Port is a published port of a service.
type Port struct {
	Service   string `json:"service"`
	HostIP    string `json:"host_ip"`
	Published int    `json:"published"`
	Target    int    `json:"target"`
	Protocol  string `json:"protocol"`
}

// PublishedPorts lists published ports (for dol_forward suggestions) and
// warns about ports reachable from the whole LAN.
func (d Doc) PublishedPorts() (ports []Port, warnings []string) {
	services := m(d["services"])
	for _, name := range sortedKeys(services) {
		ps, _ := m(services[name])["ports"].([]any)
		for _, p := range ps {
			pm := m(p)
			if pm == nil {
				continue
			}
			pub := toInt(pm["published"])
			if pub == 0 {
				continue
			}
			ip, _ := pm["host_ip"].(string)
			proto, _ := pm["protocol"].(string)
			ports = append(ports, Port{Service: name, HostIP: ip, Published: pub, Target: toInt(pm["target"]), Protocol: proto})
			if ip == "" || ip == "0.0.0.0" || ip == "::" {
				warnings = append(warnings, fmt.Sprintf("service %s publishes port %d on all interfaces: on a remote node it is reachable from your LAN; prefer \"127.0.0.1:%d:%d\" and dol_forward", name, pub, pub, toInt(pm["target"])))
			}
		}
	}
	return ports, warnings
}

func toInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case string:
		// "8080" or a range "8080-8081" (take first)
		x, _, _ = strings.Cut(x, "-")
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

// Marshal renders the document as JSON (valid compose YAML).
func (d Doc) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	err := enc.Encode(map[string]any(d))
	return buf.Bytes(), err
}
