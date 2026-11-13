package sync

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fl0wo/docker-offload-lan/internal/sshx"
)

func TestSanitizeProject(t *testing.T) {
	for in, want := range map[string]string{"payments-api": "payments-api", "../x": "x", "my app": "my-app", "": "project", "..": "project"} {
		if got := SanitizeProject(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
	if RemoteDir("/srv/dol", "l-aaaaaaaaaa", "../../etc") != "/srv/dol/l-aaaaaaaaaa/etc" {
		t.Fatal("path escape")
	}
}

func TestArgs(t *testing.T) {
	a := Args(Options{SSH: sshx.Options{ExtraArgs: []string{"-F", "/tmp/my cfg"}}, SSHHost: "tower", LocalDir: "/p/", RemoteDir: "/srv/dol/l-a/p", RespectGitignore: true})
	j := strings.Join(a, "|")
	for _, want := range []string{"--delete", "--exclude=.git", "--filter=:- .gitignore", "'/tmp/my cfg'", "--|/p/|tower:/srv/dol/l-a/p/"} {
		if !strings.Contains(j, want) {
			t.Errorf("missing %q in %s", want, j)
		}
	}
	if strings.Contains(j, "checksum") {
		t.Fatal("--checksum must never be used")
	}
}

// TestLocalRsyncSemantics runs the real rsync binary (openrsync or GNU) on a
// local copy with our exact filter flags to prove gitignore/.git/delete.
func TestLocalRsyncSemantics(t *testing.T) {
	bin, _, err := FindRsync()
	if err != nil {
		t.Skip(err)
	}
	src, dst := t.TempDir(), t.TempDir()
	must := func(p, s string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(s), 0o644)
	}
	must(filepath.Join(src, "keep.txt"), "k")
	must(filepath.Join(src, ".gitignore"), "ignored.log\n")
	must(filepath.Join(src, "ignored.log"), "x")
	must(filepath.Join(src, ".git", "HEAD"), "ref")
	must(filepath.Join(dst, "stale.txt"), "old")
	o := Options{SSHHost: "h", LocalDir: src, RemoteDir: dst, RespectGitignore: true, Rsync: bin}
	args := Args(o)
	// Replace host:dst with plain dst for a local copy, keeping all filters.
	args[len(args)-1] = dst + "/"
	// drop -e <ssh>
	var local []string
	for i := 0; i < len(args); i++ {
		if args[i] == "-e" {
			i++
			continue
		}
		local = append(local, args[i])
	}
	if out, err := exec.Command(bin, local...).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	exists := func(p string) bool { _, err := os.Stat(filepath.Join(dst, p)); return err == nil }
	if !exists("keep.txt") || exists("ignored.log") || exists(".git") || exists("stale.txt") {
		t.Fatalf("unexpected result: keep=%v ignored=%v git=%v stale=%v", exists("keep.txt"), exists("ignored.log"), exists(".git"), exists("stale.txt"))
	}
}
