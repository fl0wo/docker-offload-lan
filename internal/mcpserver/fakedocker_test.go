package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/fl0wo/docker-offload-lan/internal/dockerx"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// fakeDocker emulates the subset of the docker CLI that dol uses, per
// context, with in-memory containers. Unknown commands fail loudly.
type fakeDocker struct {
	mu         sync.Mutex
	contexts   []dockerx.ContextInfo
	current    string
	containers map[string]*fakeContainer // key: context + "/" + name
	calls      []string
	memTotal   int64
	// composeConfig is returned for `compose … config --format json`.
	composeConfig string
}

type fakeContainer struct {
	ctx, id, name, state string
	labels               map[string]string
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		contexts: []dockerx.ContextInfo{
			{Name: "default", DockerEndpoint: "unix:///var/run/docker.sock"},
			{Name: "desktop-linux", Description: "Docker Desktop", DockerEndpoint: "unix:///Users/x/.docker/run/docker.sock"},
		},
		current:    "desktop-linux",
		containers: map[string]*fakeContainer{},
		memTotal:   16 * v1.GiB,
	}
}

func (f *fakeDocker) add(ctx, name string, labels map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[ctx+"/"+name] = &fakeContainer{ctx: ctx, id: fmt.Sprintf("%064x", len(f.containers)+1), name: name, state: "running", labels: labels}
}

func (f *fakeDocker) get(ctx, name string) (fakeContainer, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[ctx+"/"+name]
	if !ok {
		return fakeContainer{}, false
	}
	return *c, true
}

func (f *fakeDocker) called(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.ContainsFunc(f.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

func cmdErr(args []string, stderr string) error {
	return &dockerx.CmdError{Args: args, Err: errors.New("exit status 1"), Stderr: stderr}
}

func (f *fakeDocker) find(ctx, ref string) *fakeContainer {
	for _, c := range f.containers {
		if c.ctx == ctx && (c.name == ref || c.id == ref || strings.HasPrefix(c.id, ref) && len(ref) >= 12) {
			return c
		}
	}
	return nil
}

func rawLabels(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}

func (f *fakeDocker) Run(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	ctx := f.current
	a := args
	if len(a) >= 2 && a[0] == "--context" {
		ctx, a = a[1], a[2:]
	}
	j := strings.Join(a, " ")
	switch {
	case j == "context ls --format {{json .}}":
		var b strings.Builder
		for _, c := range f.contexts {
			c.Current = c.Name == f.current
			x, _ := json.Marshal(c)
			b.Write(x)
			b.WriteByte('\n')
		}
		return []byte(b.String()), nil
	case j == "context show":
		return []byte(f.current + "\n"), nil
	case len(a) >= 7 && a[0] == "context" && (a[1] == "create" || a[1] == "update"):
		ci := dockerx.ContextInfo{Name: a[2], Description: a[4], DockerEndpoint: strings.TrimPrefix(a[6], "host=")}
		f.contexts = slices.DeleteFunc(f.contexts, func(c dockerx.ContextInfo) bool { return c.Name == ci.Name })
		f.contexts = append(f.contexts, ci)
		return nil, nil
	case j == "info --format {{json .}}":
		return fmt.Appendf(nil, `{"ServerVersion":"29.0.0","OSType":"linux","Architecture":"aarch64","MemTotal":%d}`, f.memTotal), nil
	case strings.HasPrefix(j, "container inspect --format {{json .Config.Labels}} -- "):
		c := f.find(ctx, a[len(a)-1])
		if c == nil {
			return nil, cmdErr(args, "Error: No such container: "+a[len(a)-1])
		}
		if len(c.labels) == 0 {
			return []byte("null\n"), nil
		}
		b, _ := json.Marshal(c.labels)
		return b, nil
	case strings.HasPrefix(j, "network inspect") || strings.HasPrefix(j, "volume inspect"):
		return nil, cmdErr(args, "Error: No such object: "+a[len(a)-1])
	case strings.HasPrefix(j, "stop --time 10 -- "):
		c := f.find(ctx, a[len(a)-1])
		if c == nil {
			return nil, cmdErr(args, "Error: No such container")
		}
		c.state = "exited"
		return nil, nil
	case strings.HasPrefix(j, "rm -f "):
		c := f.find(ctx, a[len(a)-1])
		if c == nil {
			return nil, cmdErr(args, "Error: No such container")
		}
		delete(f.containers, c.ctx+"/"+c.name)
		return nil, nil
	case strings.HasPrefix(j, "ps -a --no-trunc --format {{json .}}"):
		var filters []string
		for i := 0; i < len(a)-1; i++ {
			if a[i] == "--filter" {
				filters = append(filters, strings.TrimPrefix(a[i+1], "label="))
			}
		}
		var b strings.Builder
		names := make([]string, 0)
		for k := range f.containers {
			names = append(names, k)
		}
		sort.Strings(names)
	next:
		for _, k := range names {
			c := f.containers[k]
			if c.ctx != ctx {
				continue
			}
			for _, fl := range filters {
				kk, vv, _ := strings.Cut(fl, "=")
				if c.labels[kk] != vv {
					continue next
				}
			}
			x, _ := json.Marshal(map[string]string{"ID": c.id, "Names": c.name, "Image": "alpine", "State": c.state, "Labels": rawLabels(c.labels)})
			b.Write(x)
			b.WriteByte('\n')
		}
		return []byte(b.String()), nil
	case strings.HasPrefix(j, "network ls") || strings.HasPrefix(j, "volume ls"):
		return nil, nil
	case strings.HasPrefix(j, "compose -p "):
		return nil, nil
	case strings.HasPrefix(j, "compose --project-directory ") && strings.HasSuffix(j, "config --format json"):
		if f.composeConfig == "" {
			return nil, cmdErr(args, "no configuration file provided: not found")
		}
		return []byte(f.composeConfig), nil
	}
	return nil, cmdErr(args, "fakeDocker: unexpected command: "+j)
}
