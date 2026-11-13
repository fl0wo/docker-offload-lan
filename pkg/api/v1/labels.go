package v1

import (
	"fmt"
	"sort"
	"strings"
)

// LabelPrefix is the reverse-DNS prefix of every Docker label we set.
// It is derived from the project's GitHub namespace (github.com/fl0wo).
const LabelPrefix = "io.github.fl0wo.dol"

// Label keys applied to every resource that belongs to a lease (ADR-0006).
const (
	LabelLease   = LabelPrefix + ".lease"
	LabelOwner   = LabelPrefix + ".owner"
	LabelSession = LabelPrefix + ".session"
	LabelProject = LabelPrefix + ".project"
	LabelManaged = LabelPrefix + ".managed"
)

// ManagedFilter is the `docker … --filter` argument selecting every resource
// managed by docker-offload-lan.
const ManagedFilter = "label=" + LabelManaged + "=true"

// LeaseFilter returns the `--filter` value selecting resources of one lease.
func LeaseFilter(leaseID string) string { return "label=" + LabelLease + "=" + leaseID }

// LeaseLabels returns the full label set for a lease.
func LeaseLabels(leaseID, owner, session, project string) map[string]string {
	return map[string]string{
		LabelLease:   leaseID,
		LabelOwner:   owner,
		LabelSession: session,
		LabelProject: project,
		LabelManaged: "true",
	}
}

// LabelFlags renders labels as `--label k=v` flags in a stable order, quoting
// values for POSIX shells when needed so the string can be pasted verbatim.
func LabelFlags(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, "--label "+ShellQuote(k+"="+labels[k]))
	}
	return strings.Join(parts, " ")
}

// IsOwnedBy reports whether a resource's labels prove it belongs to leaseID.
// Destructive operations must check this before acting (ADR-0006).
func IsOwnedBy(labels map[string]string, leaseID string) bool {
	return labels[LabelManaged] == "true" && labels[LabelLease] == leaseID && leaseID != ""
}

// ShellQuote quotes s for POSIX shells only when it contains characters that
// need quoting.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !alnum && !strings.ContainsRune("@%+=:,./_-", r) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// ValidateLabelValue rejects values that cannot safely be used as a label
// value or embedded in a filter expression.
func ValidateLabelValue(field, v string) error {
	if len(v) > 256 {
		return fmt.Errorf("%s is too long (%d > 256 characters)", field, len(v))
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s contains a control character", field)
		}
	}
	return nil
}
