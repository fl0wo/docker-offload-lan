//go:build linux

package node

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID reads the peer's uid with SO_PEERCRED (no cgo).
func peerUID(c *net.UnixConn) (int, bool) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, false
	}
	var cred *unix.Ucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) //nolint:gosec // fd fits int
	}); err != nil || cerr != nil || cred == nil {
		return 0, false
	}
	return int(cred.Uid), true
}
