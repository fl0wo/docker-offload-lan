package node

// Regression tests from docs/security-review-2026-11.md.

import (
	"context"
	"strings"
	"testing"

	"github.com/fl0wo/docker-offload-lan/internal/dockerx"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

// SR-01 (node side): any client can register any valid compose project name
// on its lease. Teardown must not run `docker compose -p <name> down
// --volumes` over containers/volumes that belong to another lease, or to
// nobody (the node owner's own compose projects).
func TestSecurityTeardownDoesNotDownForeignComposeProjects(t *testing.T) {
	n := newTestNode(t)
	ctx := context.Background()
	victim := n.lease(t, 1, func(r *v1.LeaseRequest) { r.Owner = "alice@mac" })
	attacker := n.lease(t, 1, func(r *v1.LeaseRequest) { r.Owner = "mallory@mac" })

	// Lease B's compose project "shop" and the node owner's own "postgres".
	n.docker.add(dockerx.KindContainer, "shop-web-1", merge(victim.Labels, map[string]string{dockerx.ComposeLabelProject: "shop"}))
	n.docker.add(dockerx.KindVolume, "shop_data", merge(victim.Labels, map[string]string{dockerx.ComposeLabelProject: "shop"}))
	n.docker.add(dockerx.KindContainer, "postgres-db-1", map[string]string{dockerx.ComposeLabelProject: "postgres"})
	n.docker.add(dockerx.KindVolume, "postgres_data", map[string]string{dockerx.ComposeLabelProject: "postgres"})
	// The attacker's own project is fine to bring down.
	n.docker.add(dockerx.KindContainer, "mine-app-1", merge(attacker.Labels, map[string]string{dockerx.ComposeLabelProject: "mine"}))

	for _, p := range []string{"shop", "postgres", "mine"} {
		if err := n.client.RegisterCompose(ctx, attacker.ID, p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := n.client.ReleaseLease(ctx, attacker.ID, false); err != nil {
		t.Fatal(err)
	}
	for _, c := range n.docker.callsWith("compose") {
		if strings.Contains(c, "-p shop ") || strings.Contains(c, "-p postgres ") {
			t.Errorf("teardown of %s ran %q over a project it does not own", attacker.ID, c)
		}
	}
	if len(n.docker.callsWith("compose -p mine down")) != 1 {
		t.Errorf("the lease's own project must still be brought down: %v", n.docker.callsWith("compose"))
	}
	if got := strings.Join(n.docker.names(dockerx.KindContainer), ","); got != "postgres-db-1,shop-web-1" {
		t.Errorf("containers after teardown: %s", got)
	}
	if got := strings.Join(n.docker.names(dockerx.KindVolume), ","); got != "postgres_data,shop_data" {
		t.Errorf("volumes after teardown: %s", got)
	}
}

func merge(a, b map[string]string) map[string]string {
	m := map[string]string{}
	for k, v := range a {
		m[k] = v
	}
	for k, v := range b {
		m[k] = v
	}
	return m
}
