package dockerx

import (
	"context"
	"fmt"
	"strings"
)

// ContextDescription marks docker contexts created by dol. Contexts without
// it are never modified (brief §7.7).
const ContextDescription = "managed by docker-offload-lan"

// ContextInfo is a row of `docker context ls --format '{{json .}}'`.
type ContextInfo struct {
	Name           string `json:"Name"`
	Description    string `json:"Description"`
	DockerEndpoint string `json:"DockerEndpoint"`
	Current        bool   `json:"Current"`
	Error          string `json:"Error"`
}

// Managed reports whether dol created this context.
func (c ContextInfo) Managed() bool { return c.Description == ContextDescription }

// Local reports whether the endpoint is a local socket (not ssh/tcp).
func (c ContextInfo) Local() bool {
	e := c.DockerEndpoint
	return strings.HasPrefix(e, "unix://") || strings.HasPrefix(e, "npipe://")
}

// cli returns a Docker that runs without --context: context management
// commands must not depend on (or change) any context.
func (d *Docker) cli() *Docker { return &Docker{R: d.R} }

// Contexts runs `docker context ls`.
func (d *Docker) Contexts(ctx context.Context) ([]ContextInfo, error) {
	out, err := d.cli().run(ctx, "context", "ls", "--format", "{{json .}}")
	if err != nil {
		return nil, err
	}
	return jsonLines[ContextInfo](out)
}

// FindContext returns the named context, or nil if it does not exist.
func (d *Docker) FindContext(ctx context.Context, name string) (*ContextInfo, error) {
	cs, err := d.Contexts(ctx)
	if err != nil {
		return nil, err
	}
	for i := range cs {
		if cs[i].Name == name {
			return &cs[i], nil
		}
	}
	return nil, nil
}

// EnsureContext makes sure docker context name points at ssh://sshHost.
//
//   - missing: `docker context create <name> --description "managed by
//     docker-offload-lan" --docker host=ssh://<sshHost>`;
//   - ours (description marker) with another host: `docker context update`;
//   - not ours: left untouched; a warning is returned and the context is used
//     as is.
//
// It never runs `docker context use`: the user's current context is not
// changed.
func (d *Docker) EnsureContext(ctx context.Context, name, sshHost string) (warning string, err error) {
	if name == "" || strings.HasPrefix(name, "-") || sshHost == "" || strings.HasPrefix(sshHost, "-") {
		return "", fmt.Errorf("invalid docker context %q / ssh host %q", name, sshHost)
	}
	want := "ssh://" + sshHost
	c, err := d.FindContext(ctx, name)
	if err != nil {
		return "", err
	}
	switch {
	case c == nil:
		_, err = d.cli().run(ctx, "context", "create", name, "--description", ContextDescription, "--docker", "host="+want)
		if err != nil {
			return "", fmt.Errorf("creating docker context %s: %w", name, err)
		}
		return "", nil
	case !c.Managed():
		if c.DockerEndpoint == want {
			return "", nil // the user made it themselves, and it is right
		}
		return fmt.Sprintf("docker context %q exists but was not created by dol (endpoint %s, expected %s); it was left unchanged. Rename it or set docker_context for this node in the dol config",
			name, c.DockerEndpoint, want), nil
	case c.DockerEndpoint != want:
		if _, err := d.cli().run(ctx, "context", "update", name, "--description", ContextDescription, "--docker", "host="+want); err != nil {
			return "", fmt.Errorf("updating docker context %s: %w", name, err)
		}
	}
	return "", nil
}

// LocalContextCandidates are tried in order when the current context is not
// usable for local fallback.
var LocalContextCandidates = []string{"orbstack", "desktop-linux", "colima", "default"}

// DetectLocalContext picks the docker context for local placement without
// changing anything: the current context (`docker context show`) unless it is
// a dol context or not a local socket, else the first existing context among
// LocalContextCandidates.
func (d *Docker) DetectLocalContext(ctx context.Context) (string, error) {
	cs, err := d.Contexts(ctx)
	if err != nil {
		return "", fmt.Errorf("listing docker contexts: %w", err)
	}
	byName := map[string]ContextInfo{}
	for _, c := range cs {
		byName[c.Name] = c
	}
	if out, err := d.cli().run(ctx, "context", "show"); err == nil {
		cur := strings.TrimSpace(string(out))
		if c, ok := byName[cur]; ok && usableLocal(c) {
			return cur, nil
		}
	}
	for _, n := range LocalContextCandidates {
		if c, ok := byName[n]; ok && usableLocal(c) {
			return n, nil
		}
	}
	return "", fmt.Errorf("no local docker context found (tried the current one and %s); set local.docker_context in the dol config",
		strings.Join(LocalContextCandidates, ", "))
}

func usableLocal(c ContextInfo) bool {
	return !strings.HasPrefix(c.Name, "dol-") && !c.Managed() && c.Error == "" && c.Local()
}

// ComposeProject runs `docker compose -p project <args...>` on this context.
func (d *Docker) ComposeProject(ctx context.Context, project string, args ...string) ([]byte, error) {
	if project == "" || strings.HasPrefix(project, "-") {
		return nil, fmt.Errorf("invalid compose project %q", project)
	}
	return d.run(ctx, append([]string{"compose", "-p", project}, args...)...)
}
