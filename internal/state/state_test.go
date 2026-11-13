package state

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

const sharedLease = "l-aaaaaaaaaa"

func newLease(id, session string) v1.ClientLease {
	return v1.ClientLease{
		ID: id, Placement: v1.PlacementLocal, DockerContext: "desktop-linux", State: v1.StateActive,
		Owner: "o@h", Session: session, Project: "p", Workload: v1.WorkloadContainer,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Labels:    v1.LeaseLabels(id, "o@h", session, "p"),
	}
}

// hammer performs n locked increments of the shared lease's EstMemoryBytes
// and n independent lease writes/reads/deletes.
func hammer(st *Store, worker string, n int) error {
	ctx := context.Background()
	for i := range n {
		if _, err := st.UpdateLease(ctx, sharedLease, func(l *v1.ClientLease) error {
			l.EstMemoryBytes++
			l.Reason = worker + "-" + strconv.Itoa(i) // grow/shrink the file to catch torn writes
			return nil
		}); err != nil {
			return err
		}
		id := v1.NewLeaseID()
		if err := st.PutLease(newLease(id, "s-bbbbbbbbbb")); err != nil {
			return err
		}
		if _, err := st.GetLease(id); err != nil {
			return err
		}
		if _, errs := st.ListLeases(); len(errs) > 0 {
			return fmt.Errorf("list saw corrupt files: %v", errs)
		}
		if err := st.DeleteLease(id); err != nil {
			return err
		}
	}
	return nil
}

func TestConcurrentGoroutinesNeverCorrupt(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutLease(newLease(sharedLease, "s-aaaaaaaaaa")); err != nil {
		t.Fatal(err)
	}
	const workers, n = 8, 20
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := range workers {
		wg.Go(func() {
			if err := hammer(st, "g"+strconv.Itoa(w), n); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	l, err := st.GetLease(sharedLease)
	if err != nil {
		t.Fatal(err)
	}
	if l.EstMemoryBytes != workers*n {
		t.Fatalf("lost updates: got %d, want %d", l.EstMemoryBytes, workers*n)
	}
}

// TestHelperProcess is re-executed by TestConcurrentProcessesNeverCorrupt.
func TestHelperProcess(t *testing.T) {
	dir := os.Getenv("DOL_STATE_HELPER_DIR")
	if dir == "" {
		t.Skip("helper process only")
	}
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(os.Getenv("DOL_STATE_HELPER_N"))
	// Two goroutines per process: exercises the in-process mutex and the
	// cross-process flock at the same time.
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for g := range 2 {
		wg.Go(func() {
			if err := hammer(st, fmt.Sprintf("p%d-g%d", os.Getpid(), g), n); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestConcurrentProcessesNeverCorrupt(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns processes")
	}
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutLease(newLease(sharedLease, "s-aaaaaaaaaa")); err != nil {
		t.Fatal(err)
	}
	const procs, n = 4, 25
	var wg sync.WaitGroup
	outs := make([][]byte, procs)
	errs := make([]error, procs)
	for p := range procs {
		wg.Go(func() {
			cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$", "-test.count=1")
			cmd.Env = append(os.Environ(), "DOL_STATE_HELPER_DIR="+dir, "DOL_STATE_HELPER_N="+strconv.Itoa(n))
			outs[p], errs[p] = cmd.CombinedOutput()
		})
	}
	wg.Wait()
	for p := range procs {
		if errs[p] != nil {
			t.Fatalf("helper %d: %v\n%s", p, errs[p], outs[p])
		}
	}
	l, err := st.GetLease(sharedLease)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(procs * 2 * n); l.EstMemoryBytes != want {
		t.Fatalf("lost updates across processes: got %d, want %d", l.EstMemoryBytes, want)
	}
	leases, lerrs := st.ListLeases()
	if len(lerrs) > 0 || len(leases) != 1 {
		t.Fatalf("leftover or corrupt leases: %d %v", len(leases), lerrs)
	}
	tmps, _ := filepath.Glob(filepath.Join(dir, LeasesDir, ".*"))
	if len(tmps) > 0 {
		t.Fatalf("temp files left behind: %v", tmps)
	}
}

func TestSessionLiveness(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	self, err := NewSelfSession("s-aaaaaaaaaa", "o", "p")
	if err != nil {
		t.Fatal(err)
	}
	if !self.Alive() {
		t.Fatal("own process must be alive")
	}
	// A real process that exits: its PID is gone.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	gone := Session{ID: "s-bbbbbbbbbb", PID: cmd.Process.Pid, StartTime: "1.000000"}
	// Same PID as us, different start time: PID reuse.
	reused := self
	reused.ID = "s-cccccccccc"
	reused.StartTime = "1.000000"
	for _, s := range []Session{self, gone, reused} {
		if err := st.PutSession(s); err != nil {
			t.Fatal(err)
		}
	}
	dead, err := st.PruneDeadSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(dead) != 2 {
		t.Fatalf("want 2 dead sessions, got %+v", dead)
	}
	if !st.SessionAlive(self.ID) || st.SessionAlive(gone.ID) || st.SessionAlive(reused.ID) {
		t.Fatal("liveness after prune is wrong")
	}
}

func TestContainsAndLeaseFiles(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if st.Contains(st.Dir()) || st.Contains(filepath.Join(st.Dir(), "..", "x")) || !st.Contains(st.Path(KubeDir, "a")) {
		t.Fatal("Contains is wrong")
	}
	for _, p := range []string{st.Path(KubeDir, sharedLease+"-e2e.yaml"), st.Path(ComposeDir, sharedLease+"-web.yaml"), st.Path(KubeDir, "l-bbbbbbbbbb-x.yaml")} {
		if err := WriteFileAtomic(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := st.RemoveLeaseFiles(sharedLease)
	if err != nil || len(removed) != 2 {
		t.Fatalf("removed %v err %v", removed, err)
	}
	if _, err := os.Stat(st.Path(KubeDir, "l-bbbbbbbbbb-x.yaml")); err != nil {
		t.Fatal("other lease's file must stay")
	}
	if _, err := st.GetLease("l-zzzzzzzzzz"); v1.CodeOf(err) != v1.CodeNotFound {
		t.Fatalf("want not_found, got %v", err)
	}
	if _, err := st.GetLease("../etc"); v1.CodeOf(err) != v1.CodeBadRequest {
		t.Fatalf("want bad_request, got %v", err)
	}
}
