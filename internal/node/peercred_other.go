//go:build !linux && !darwin

package node

import "net"

// peerUID is not supported on this platform; ssh_user stays empty.
func peerUID(*net.UnixConn) (int, bool) { return 0, false }
