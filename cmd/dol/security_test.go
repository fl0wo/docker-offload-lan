package main

import (
	"bytes"
	"context"
	"os"
	"testing"
)

// SR-04: sync_root holds copies of users' working trees, including .env
// files that are not gitignored. `dol node init` must not make it readable
// by other local users of the node (install.sh uses 0750 dol:dol).
func TestSecurityNodeInitSyncRootNotWorldReadable(t *testing.T) {
	dir := t.TempDir()
	f := initFlagsFor(dir)
	var out bytes.Buffer
	if err := runNodeInit(context.Background(), f, noneChanged, &out, okProbes()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(f.syncRoot)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o007 != 0 {
		t.Fatalf("sync_root %s has mode %v: other users can read synced workspaces", f.syncRoot, fi.Mode().Perm())
	}
	if syncRootMode.Perm()&0o007 != 0 {
		t.Fatalf("chownToDolGroup sets mode %v on sync_root", syncRootMode)
	}
}
