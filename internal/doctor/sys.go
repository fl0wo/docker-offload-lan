package doctor

import (
	"bufio"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func statPath(p string) (FileMode, error) {
	fi, err := os.Stat(p)
	if os.IsNotExist(err) {
		return FileMode{}, nil
	}
	if err != nil {
		return FileMode{}, err
	}
	fm := FileMode{Mode: fi.Mode(), Exist: true, UID: -1, GID: -1}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		fm.UID, fm.GID = int(st.Uid), int(st.Gid)
	}
	return fm, nil
}

func diskFree(p string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize) //nolint:unconvert // int64 on linux, uint32 on darwin
	return st.Bavail * bs, st.Blocks * bs, nil
}

// listeningTCPPorts reads /proc/net/tcp and /proc/net/tcp6 (Linux only).
func listeningTCPPorts() (map[int]bool, error) {
	ports := map[int]bool{}
	var firstErr error
	n := 0
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		fh, err := os.Open(f)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		n++
		for p := range parseProcNetTCP(fh) {
			ports[p] = true
		}
		_ = fh.Close()
	}
	if n == 0 {
		return nil, firstErr
	}
	return ports, nil
}

// parseProcNetTCP returns the local ports in LISTEN state (st == 0A) from a
// /proc/net/tcp{,6} table.
func parseProcNetTCP(r io.Reader) map[int]bool {
	out := map[int]bool{}
	sc := bufio.NewScanner(r)
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue
		}
		f := strings.Fields(sc.Text())
		if len(f) < 4 || f[3] != "0A" {
			continue
		}
		i := strings.LastIndexByte(f[1], ':')
		if i < 0 {
			continue
		}
		if p, err := strconv.ParseUint(f[1][i+1:], 16, 16); err == nil {
			out[int(p)] = true
		}
	}
	return out
}
