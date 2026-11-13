package v1

import (
	"fmt"
	"regexp"
)

// Kind access modes (brief §7.9).
const (
	AccessTunnel = "tunnel" // API server bound to 127.0.0.1 on the node, reached via ssh -L
	AccessLAN    = "lan"    // API server bound to 0.0.0.0 with LAN IP in certSANs
)

// KindCreateRequest is the body of POST /v1/leases/{id}/kind.
type KindCreateRequest struct {
	Name       string `json:"name"`
	ConfigYAML string `json:"config_yaml,omitempty"`
	AccessMode string `json:"access_mode,omitempty"` // default: node config
}

// PortMapping is a host port published by a kind node container
// (extraPortMappings), offered to the client as a forward.
type PortMapping struct {
	ContainerPort int    `json:"container_port"`
	HostPort      int    `json:"host_port"`
	ListenAddress string `json:"listen_address,omitempty"`
	Protocol      string `json:"protocol,omitempty"`
}

// HostPort is a host:port pair.
type HostPort struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// KindCreateResponse is the 201 body of POST /v1/leases/{id}/kind.
//
// Kubeconfig is sensitive: never log it.
type KindCreateResponse struct {
	Cluster      string        `json:"cluster"`
	Kubeconfig   string        `json:"kubeconfig"`
	APIServer    HostPort      `json:"api_server"`
	AccessMode   string        `json:"access_mode"`
	PortMappings []PortMapping `json:"port_mappings,omitempty"`
}

// KindLoadRequest is the body of POST /v1/leases/{id}/kind/{name}/load.
type KindLoadRequest struct {
	Images []string `json:"images"`
}

// KindLoadResponse reports which images were loaded.
type KindLoadResponse struct {
	Loaded []string `json:"loaded"`
}

var kindShortNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// KindShortNameMaxLen bounds the user-facing name so that
// "dol-" + lease id (12) + "-" + name stays within kind's cluster name limit
// (see docs/compat.md) and leaves room for "-control-plane" / "-worker2"
// container suffixes within the 63-char hostname limit.
const KindShortNameMaxLen = 16

// KindClusterName returns the real kind cluster name for a lease.
func KindClusterName(leaseID, name string) string { return "dol-" + leaseID + "-" + name }

// ValidateKindName validates the user-facing cluster name.
func ValidateKindName(name string) error {
	if len(name) == 0 || len(name) > KindShortNameMaxLen || !kindShortNameRe.MatchString(name) {
		return Errorf(CodeBadRequest,
			fmt.Sprintf("use 1-%d lowercase letters, digits or '-', e.g. \"e2e\"", KindShortNameMaxLen),
			"invalid kind cluster name %q", name)
	}
	return nil
}
