package v1

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLeaseIDFormat(t *testing.T) {
	seen := map[string]bool{}
	for range 2000 {
		id := NewLeaseID()
		if !ValidLeaseID(id) {
			t.Fatalf("generated invalid id %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
	for _, bad := range []string{"", "l-", "l-ABCDEFGHIJ", "l-abcdefghi1", "x-abcdefghij", "l-abcdefghijk"} {
		if ValidLeaseID(bad) {
			t.Errorf("ValidLeaseID(%q) = true", bad)
		}
	}
}

func TestLabelFlagsStableAndQuoted(t *testing.T) {
	l := LeaseLabels("l-aaaaaaaaaa", "alice@mac book", "s-1", "proj")
	got := LabelFlags(l)
	want := "--label io.github.fl0wo.dol.lease=l-aaaaaaaaaa --label io.github.fl0wo.dol.managed=true " +
		"--label 'io.github.fl0wo.dol.owner=alice@mac book' --label io.github.fl0wo.dol.project=proj " +
		"--label io.github.fl0wo.dol.session=s-1"
	if got != want {
		t.Fatalf("LabelFlags:\n got %s\nwant %s", got, want)
	}
	if ShellQuote("it's") != `'it'"'"'s'` {
		t.Fatalf("bad quote: %s", ShellQuote("it's"))
	}
}

func TestIsOwnedBy(t *testing.T) {
	l := LeaseLabels("l-aaaaaaaaaa", "o", "s", "p")
	if !IsOwnedBy(l, "l-aaaaaaaaaa") {
		t.Fatal("expected owned")
	}
	if IsOwnedBy(l, "l-bbbbbbbbbb") || IsOwnedBy(l, "") || IsOwnedBy(map[string]string{LabelLease: "l-aaaaaaaaaa"}, "l-aaaaaaaaaa") {
		t.Fatal("ownership must require managed=true and matching lease")
	}
}

func TestErrorEnvelope(t *testing.T) {
	e := Errorf(CodeInsufficientMemory, "request less memory", "6.1 GiB schedulable, 10.0 GiB requested").
		WithDetails(map[string]any{"schedulable_free_bytes": 1})
	b, _ := json.Marshal(ErrorResponse{Error: e})
	if !strings.Contains(string(b), `"code":"insufficient_memory"`) || e.Code.HTTPStatus() != 409 || !e.Code.IsRefusal() {
		t.Fatalf("unexpected: %s", b)
	}
	if CodeLeaseGone.HTTPStatus() != 410 || CodeNotFound.HTTPStatus() != 404 {
		t.Fatal("status mapping")
	}
}

func TestLeaseRequestValidate(t *testing.T) {
	ok := LeaseRequest{Owner: "a", Session: "s", Workload: WorkloadKind, EstMemoryBytes: GiB}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []LeaseRequest{
		{Session: "s", Workload: WorkloadKind, EstMemoryBytes: 1},
		{Owner: "a", Session: "s", Workload: "vm", EstMemoryBytes: 1},
		{Owner: "a", Session: "s", Workload: WorkloadKind},
		{Owner: "a\n", Session: "s", Workload: WorkloadKind, EstMemoryBytes: 1},
	}
	for i, c := range cases {
		if err := c.Validate(); CodeOf(err) != CodeBadRequest {
			t.Errorf("case %d: want bad_request, got %v", i, err)
		}
	}
}

func TestLeaseJSONFlattensRequest(t *testing.T) {
	b, _ := json.Marshal(Lease{ID: "l-aaaaaaaaaa", LeaseRequest: LeaseRequest{Owner: "o"}})
	if !strings.Contains(string(b), `"owner":"o"`) {
		t.Fatalf("embedded request fields must be flattened: %s", b)
	}
}

func TestKindNames(t *testing.T) {
	if err := ValidateKindName("e2e"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "E2E", "-a", "a-", "a_b", strings.Repeat("a", KindShortNameMaxLen+1)} {
		if ValidateKindName(bad) == nil {
			t.Errorf("%q should be invalid", bad)
		}
	}
	if n := KindClusterName("l-aaaaaaaaaa", strings.Repeat("a", KindShortNameMaxLen)); len(n) > 33 {
		t.Fatalf("cluster name too long: %d", len(n))
	}
}
