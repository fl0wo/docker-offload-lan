package node

import (
	"context"
	"net"
	"os/user"
	"strconv"
)

type peerUserKey struct{}

// connContext is http.Server.ConnContext: it records the username of the
// process on the other end of the unix socket. That process is
// `dol node dial-stdio`, started by sshd as the SSH user, so this is the SSH
// user recorded on leases for audit (brief §7.2). Unsupported platforms and
// non-unix connections yield "".
func connContext(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, peerUserKey{}, peerUsername(c))
}

func peerUsername(c net.Conn) string {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return ""
	}
	uid, ok := peerUID(uc)
	if !ok {
		return ""
	}
	if u, err := user.LookupId(strconv.Itoa(uid)); err == nil && u.Username != "" {
		return u.Username
	}
	return "uid:" + strconv.Itoa(uid)
}

// PeerUser returns the SSH user recorded for the request's connection.
func PeerUser(ctx context.Context) string {
	s, _ := ctx.Value(peerUserKey{}).(string)
	return s
}
