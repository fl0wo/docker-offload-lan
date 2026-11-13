package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/fl0wo/docker-offload-lan/internal/dockerx"
	"strings"
)

// knownLocalContexts are local Docker providers on macOS, in preference order.
var knownLocalContexts = []string{"orbstack", "desktop-linux", "colima"}

// ManagedContextDescription marks docker contexts created by dol. dol never
// modifies or deletes a context without it.
const ManagedContextDescription = dockerx.ContextDescription

// DockerContext is one entry of `docker context ls --format '{{json .}}'`.
type DockerContext struct {
	Name           string `json:"Name"`
	Description    string `json:"Description"`
	DockerEndpoint string `json:"DockerEndpoint"`
	Current        bool   `json:"Current"`
}

// ListContexts runs `docker context ls`.
func ListContexts(ctx context.Context, r Runner) ([]DockerContext, error) {
	out, se, err := r.Run(ctx, "docker", "context", "ls", "--format", "{{json .}}")
	if err != nil {
		return nil, fmt.Errorf("docker context ls: %s", errText(err, se))
	}
	var cs []DockerContext
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		var c DockerContext
		if err := json.Unmarshal([]byte(l), &c); err != nil {
			return nil, fmt.Errorf("parsing docker context ls output: %w", err)
		}
		cs = append(cs, c)
	}
	return cs, nil
}

// DetectLocalContext resolves the client's local docker context. configured
// is local.docker_context from the client config ("auto" or a name).
//
// auto: the current context unless it is one of dol's (a remote node), else
// the first of orbstack, desktop-linux, colima that exists, else "default".
func DetectLocalContext(ctx context.Context, r Runner, configured string) (string, error) {
	if configured != "" && configured != "auto" {
		return configured, nil
	}
	cs, err := ListContexts(ctx, r)
	if err != nil {
		return "", err
	}
	byName := map[string]DockerContext{}
	for _, c := range cs {
		byName[c.Name] = c
		if c.Current && !isDolContext(c) {
			return c.Name, nil
		}
	}
	for _, n := range knownLocalContexts {
		if _, ok := byName[n]; ok {
			return n, nil
		}
	}
	return "default", nil
}

func isDolContext(c DockerContext) bool {
	return strings.HasPrefix(c.Name, "dol-") || c.Description == ManagedContextDescription
}
