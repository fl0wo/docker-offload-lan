//go:build darwin

package node

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerUID reads the peer's uid with LOCAL_PEERCRED (macOS nodes are a v2
// target; this keeps local development and tests faithful).
func peerUID(c *net.UnixConn) (int, bool) {
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, false
	}
	var cred *unix.Xucred
	var cerr error
	if err := raw.Control(func(fd uintptr) {
		cred, cerr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED) //nolint:gosec // fd fits int
	}); err != nil || cerr != nil || cred == nil {
		return 0, false
	}
	return int(cred.Uid), true
}
