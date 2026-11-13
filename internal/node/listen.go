package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"time"
)

// SocketGroup is the group allowed to use the node socket (brief §7.11).
const SocketGroup = "dol"

// ListenUnix creates the node API socket: it creates the parent directory,
// removes a stale socket left by a crashed agent (refusing if another agent
// still answers on it), and sets mode 0660 with group "dol" when that group
// exists.
func ListenUnix(path string, log *slog.Logger) (net.Listener, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if max := maxSocketPath(); len(path) > max {
		return nil, fmt.Errorf("socket path %s is %d bytes, longer than the %d-byte unix socket limit of %s; choose a shorter `socket` in node.yaml",
			path, len(path), max, runtime.GOOS)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // the socket itself is 0660; the directory must be traversable
		return nil, fmt.Errorf("creating socket directory %s: %w (run as root or change `socket` in node.yaml)", dir, err)
	}
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket; refusing to remove it (move it away or change `socket` in node.yaml)", path)
		}
		c, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			_ = c.Close()
			return nil, fmt.Errorf("another node agent is already serving on %s (stop it first: `sudo systemctl stop dol-node`)", path)
		}
		log.Info("removing stale socket", "socket", path)
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("removing stale socket %s: %w", path, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listening on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o660); err != nil { //nolint:gosec // brief §7.11: socket mode 0660, group dol
		_ = ln.Close()
		return nil, fmt.Errorf("chmod 0660 %s: %w", path, err)
	}
	if g, err := user.LookupGroup(SocketGroup); err == nil {
		gid, _ := strconv.Atoi(g.Gid)
		if err := os.Chown(path, -1, gid); err != nil {
			log.Warn("cannot give the socket to group "+SocketGroup+"; only its owner can use it", "socket", path, "err", err)
		}
	} else {
		log.Warn("group "+SocketGroup+" does not exist; the socket is usable only by its owner (deploy/linux/install.sh creates the group)", "socket", path)
	}
	return ln, nil
}

// maxSocketPath is sizeof(sun_path)-1: 107 on Linux, 103 on BSDs/macOS.
func maxSocketPath() int {
	if runtime.GOOS == "linux" {
		return 107
	}
	return 103
}

// DialStdio bridges in/out to the node agent socket, like
// `docker system dial-stdio`: stdin is copied to the socket (half-closing
// the write side at EOF) and the socket to stdout. It returns when the agent
// closes the connection, or when ctx is cancelled.
func DialStdio(ctx context.Context, socket string, in io.Reader, out io.Writer) error {
	c, err := dialUnixRetry(ctx, socket, 3*time.Second)
	if err != nil {
		return fmt.Errorf("cannot connect to node agent at %s: %w\n"+
			"(hint: is the agent running? `sudo systemctl status dol-node`; is this user in the %q group? `id`)",
			socket, unwrapOp(err), SocketGroup)
	}
	uc := c.(*net.UnixConn)
	defer func() { _ = uc.Close() }()

	stdinDone := make(chan error, 1)
	connDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(uc, in)
		if cerr := uc.CloseWrite(); err == nil {
			err = cerr
		}
		stdinDone <- err
	}()
	go func() {
		_, err := io.Copy(out, uc)
		if cw, ok := out.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		connDone <- err
	}()
	select {
	case err := <-connDone:
		// The agent closed: we are done even if stdin is still open.
		return ignoreClosed(err)
	case err := <-stdinDone:
		if err != nil && !isClosed(err) {
			return fmt.Errorf("copying stdin to node agent: %w", err)
		}
		// Client finished sending; wait for the agent's remaining output.
		select {
		case err := <-connDone:
			return ignoreClosed(err)
		case <-ctx.Done():
			return nil
		}
	case <-ctx.Done():
		return nil
	}
}

// dialUnixRetry connects to socket, retrying for up to patience while the
// connect is refused (full listen backlog, or the agent restarting). A
// missing socket fails immediately.
func dialUnixRetry(ctx context.Context, socket string, patience time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(patience)
	var d net.Dialer
	for {
		dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		c, err := d.DialContext(dctx, "unix", socket)
		cancel()
		if err == nil {
			return c, nil
		}
		retryable := errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EAGAIN)
		if !retryable || time.Now().After(deadline) || ctx.Err() != nil {
			return nil, err
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func unwrapOp(err error) error {
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Err != nil {
		return oe.Err
	}
	return err
}

func isClosed(err error) bool {
	return errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, io.EOF)
}

func ignoreClosed(err error) error {
	if err == nil || isClosed(err) {
		return nil
	}
	return fmt.Errorf("copying node agent output: %w", err)
}
