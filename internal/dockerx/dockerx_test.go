package dockerx

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// fakeRunner answers by matching the joined args against prefixes.
type fakeRunner struct {
	mu      sync.Mutex
	answers map[string]string // prefix -> stdout ("ERR:" prefix -> error with stderr)
	calls   []string
}

func (f *fakeRunner) Run(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := strings.Join(args, " ")
	f.calls = append(f.calls, j)
	best := ""
	for p := range f.answers {
		if strings.HasPrefix(j, p) && len(p) > len(best) {
			best = p
		}
	}
	if best == "" {
		return nil, nil
	}
	a := f.answers[best]
	if strings.HasPrefix(a, "ERR:") {
		return nil, &CmdError{Args: args, Err: fmt.Errorf("exit 1"), Stderr: a[4:]}
	}
	return []byte(a), nil
}

const lease = "l-aaaaaaaaaa"

func ownedLabels() string {
	return `{"` + v1.LabelLease + `":"` + lease + `","` + v1.LabelManaged + `":"true"}`
}

func TestRequireOwnedRefusesUnlabeled(t *testing.T) {
	f := &fakeRunner{answers: map[string]string{
		"--context x container inspect --format {{json .Config.Labels}} -- mine":  ownedLabels(),
		"--context x container inspect --format {{json .Config.Labels}} -- other": `{"foo":"bar"}`,
		"--context x container inspect --format {{json .Config.Labels}} -- nolab": `null`,
		"--context x container inspect --format {{json .Config.Labels}} -- gone":  "ERR:Error: No such container: gone",
	}}
	d := &Docker{Context: "x", R: f}
	ctx := context.Background()
	if _, err := d.RequireOwned(ctx, KindContainer, "mine", lease); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"other", "nolab"} {
		if err := d.StopOwned(ctx, ref, lease); v1.CodeOf(err) != v1.CodeNotOwned {
			t.Fatalf("%s: want not_owned, got %v", ref, err)
		}
	}
	if _, err := d.RequireOwned(ctx, KindContainer, "mine", "l-bbbbbbbbbb"); v1.CodeOf(err) != v1.CodeNotOwned {
		t.Fatalf("other lease must be refused, got %v", err)
	}
	if err := d.StopOwned(ctx, "gone", lease); v1.CodeOf(err) != v1.CodeNotFound {
		t.Fatalf("want not_found, got %v", err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "--context x stop") && !strings.HasSuffix(c, "mine") {
			t.Fatalf("stopped an unowned container: %s", c)
		}
	}
}

func TestTeardownOrderAndScope(t *testing.T) {
	f := &fakeRunner{answers: map[string]string{
		"ps -a --no-trunc --format {{json .}} --filter label=" + v1.LabelLease + "=" + lease: `{"ID":"c1","Names":"web","State":"running"}
{"ID":"c2","Names":"db","State":"exited"}`,
		"network ls --no-trunc --format {{json .}} --filter label=" + v1.LabelLease + "=" + lease: `{"ID":"n1","Name":"net"}`,
		"volume ls --format {{json .}} --filter label=" + v1.LabelLease + "=" + lease:             `{"Name":"vol"}`,
		"container inspect": ownedLabels(),
		"network inspect":   ownedLabels(),
		"volume inspect":    ownedLabels(),
	}}
	d := &Docker{R: f}
	inv := d.TeardownLease(context.Background(), lease)
	if len(inv.Errors) > 0 || len(inv.Containers) != 2 || len(inv.Networks) != 1 || len(inv.Volumes) != 1 {
		t.Fatalf("%+v", inv)
	}
	order := []string{}
	for _, c := range f.calls {
		for _, k := range []string{"stop", "rm -f", "network rm", "volume rm"} {
			if strings.HasPrefix(c, k) {
				order = append(order, k)
			}
		}
	}
	want := "stop,rm -f,rm -f,network rm,volume rm"
	if strings.Join(order, ",") != want {
		t.Fatalf("order %v, want %s", order, want)
	}
}

func TestTeardownRejectsBadLeaseID(t *testing.T) {
	d := &Docker{R: &fakeRunner{}}
	inv := d.TeardownLease(context.Background(), "")
	if len(inv.Errors) == 0 {
		t.Fatal("empty lease id must be refused (would match everything)")
	}
}

func TestCountUnlabeled(t *testing.T) {
	f := &fakeRunner{answers: map[string]string{
		"ps -a --format":      "true|a\n|b\n|dol-l-aaaaaaaaaa-e2e-control-plane\n|c\n",
		"volume ls --format":  "true|mine\n|users-data\n",
		"network ls --format": "|bridge\n|host\n|none\n|kind\ntrue|x\n|custom\n",
	}}
	c, err := (&Docker{R: f}).CountUnlabeled(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.Containers != 2 || c.Volumes != 1 || c.Networks != 1 {
		t.Fatalf("%+v", c)
	}
}
