package dockerx

import (
	"context"
	"strings"
	"testing"
)

const ctxList = `{"Current":false,"Description":"Current DOCKER_HOST based configuration","DockerEndpoint":"unix:///var/run/docker.sock","Error":"","Name":"default"}
{"Current":true,"Description":"managed by docker-offload-lan","DockerEndpoint":"ssh://tower","Error":"","Name":"dol-tower"}
{"Current":false,"Description":"Docker Desktop","DockerEndpoint":"unix:///Users/x/.docker/run/docker.sock","Error":"","Name":"desktop-linux"}
{"Current":false,"Description":"mine","DockerEndpoint":"ssh://elsewhere","Error":"","Name":"dol-mine"}
`

func countPrefix(calls []string, p string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, p) {
			n++
		}
	}
	return n
}

func TestEnsureContext(t *testing.T) {
	ctx := context.Background()
	f := &fakeRunner{answers: map[string]string{"context ls": ctxList}}
	// Bound to a context: context management must still run without --context.
	d := &Docker{Context: "whatever", R: f}

	if w, err := d.EnsureContext(ctx, "dol-new", "newhost"); err != nil || w != "" {
		t.Fatalf("create: %q %v", w, err)
	}
	if countPrefix(f.calls, "context create dol-new --description managed by docker-offload-lan --docker host=ssh://newhost") != 1 {
		t.Fatalf("expected create, calls: %v", f.calls)
	}
	if w, err := d.EnsureContext(ctx, "dol-tower", "tower"); err != nil || w != "" {
		t.Fatalf("noop: %q %v", w, err)
	}
	if w, err := d.EnsureContext(ctx, "dol-tower", "tower2"); err != nil || w != "" {
		t.Fatalf("update: %q %v", w, err)
	}
	if countPrefix(f.calls, "context update dol-tower") != 1 {
		t.Fatalf("expected update, calls: %v", f.calls)
	}
	w, err := d.EnsureContext(ctx, "dol-mine", "tower")
	if err != nil || !strings.Contains(w, "not created by dol") {
		t.Fatalf("foreign context: %q %v", w, err)
	}
	for _, c := range f.calls {
		if strings.Contains(c, "dol-mine") && !strings.HasPrefix(c, "context ls") {
			t.Fatalf("modified a foreign context: %s", c)
		}
		if strings.HasPrefix(c, "context use") || strings.HasPrefix(c, "--context") {
			t.Fatalf("unexpected call %s", c)
		}
	}
}

func TestDetectLocalContext(t *testing.T) {
	ctx := context.Background()
	// Current is dol-tower → skip it, pick desktop-linux (orbstack missing).
	f := &fakeRunner{answers: map[string]string{"context ls": ctxList, "context show": "dol-tower\n"}}
	got, err := (&Docker{R: f}).DetectLocalContext(ctx)
	if err != nil || got != "desktop-linux" {
		t.Fatalf("got %q %v", got, err)
	}
	// Current is a usable local context → keep it.
	f = &fakeRunner{answers: map[string]string{"context ls": ctxList, "context show": "default\n"}}
	if got, _ := (&Docker{R: f}).DetectLocalContext(ctx); got != "default" {
		t.Fatalf("got %q", got)
	}
	// Nothing usable.
	f = &fakeRunner{answers: map[string]string{"context ls": `{"Name":"x","DockerEndpoint":"ssh://y"}`, "context show": "x\n"}}
	if _, err := (&Docker{R: f}).DetectLocalContext(ctx); err == nil {
		t.Fatal("want error")
	}
}

func TestParseLabels(t *testing.T) {
	m := ParseLabels("a=1,io.github.fl0wo.dol.owner=alice@mac,empty=,c=x=y")
	if m["a"] != "1" || m["io.github.fl0wo.dol.owner"] != "alice@mac" || m["empty"] != "" || m["c"] != "x=y" {
		t.Fatalf("%v", m)
	}
}
