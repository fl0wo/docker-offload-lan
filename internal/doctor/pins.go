package doctor

// Tool versions pinned by deploy/linux/install.sh (docs/compat.md). A test
// keeps these in sync with the script.
const (
	KindVersion    = "v0.33.0"
	KubectlVersion = "v1.37.2"
	// SSHPort is the port sshd listens on for dol on nodes (ADR-0030).
	SSHPort = 2222
	// NodeUser is the node account clients log in as.
	NodeUser = "dol"
)
