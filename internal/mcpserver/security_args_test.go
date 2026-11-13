package mcpserver

import (
	"strings"
	"testing"
)

// SR-02: extra_args are appended after `compose … up`; compose's global
// flags are accepted there too, including attached short forms.
func TestSecurityComposeExtraArgsCannotOverrideProject(t *testing.T) {
	h, acq := localComposeHarness(t, "shop")
	for _, a := range []string{"-pvictim", "-fother.yml", "-dpvictim", "--project-name=victim", "--file=x.yml", "-H", "--host=tcp://1.2.3.4:2375", "--project-directory=/"} {
		r := h.call("dol_compose_up", map[string]any{"lease_id": acq.LeaseID, "extra_args": []string{a}})
		if !r.IsError {
			t.Errorf("extra_args %q must be refused", a)
		}
	}
	for _, c := range h.docker.calls {
		if strings.Contains(c, "victim") || strings.Contains(c, "other.yml") {
			t.Fatalf("forbidden flag reached docker: %q", c)
		}
	}
	// Ordinary flags and service names keep working.
	if r := h.call("dol_compose_up", map[string]any{"lease_id": acq.LeaseID, "extra_args": []string{"--build", "--wait", "-d", "web"}}); r.IsError {
		t.Fatal(textOf(r))
	}
}
