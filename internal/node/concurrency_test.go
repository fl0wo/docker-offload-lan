package node

import (
	"context"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fl0wo/docker-offload-lan/internal/config"
	"github.com/fl0wo/docker-offload-lan/internal/nodeclient"
	v1 "github.com/fl0wo/docker-offload-lan/pkg/api/v1"
)

const MiB = int64(1) << 20

type burstResult struct {
	accepted    []int64 // est_memory of accepted requests
	refusedSize []int64 // est_memory of insufficient_memory refusals
	byCode      map[v1.ErrorCode]int
	maxSeen     int64 // highest committed value observed while the burst ran
	took        time.Duration
	connRetries int64 // connects refused by a full listen backlog, retried
}

func connectRefused(err error) bool {
	if v1.CodeOf(err) != v1.CodeUnreachable {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "connection refused") || strings.Contains(m, "resource temporarily unavailable")
}

// burst fires all requests at once through the real HTTP handler over the
// unix socket, while a watcher samples committed memory from the store.
func burst(t *testing.T, n *testNode, sizes []int64) burstResult {
	t.Helper()
	ctx := context.Background()
	res := burstResult{byCode: map[v1.ErrorCode]int{}}
	var mu sync.Mutex
	start := make(chan struct{})
	var wg sync.WaitGroup

	stop := make(chan struct{})
	var maxSeen atomic.Int64
	var connRetries atomic.Int64
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if c, _, err := n.store.Committed(ctx, ""); err == nil && c > maxSeen.Load() {
				maxSeen.Store(c)
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()

	// Several clients so connections are spread like many dol mcp processes.
	clients := make([]*nodeclient.Client, 8)
	for i := range clients {
		clients[i] = nodeclient.NewUnix("test", n.socket)
		defer clients[i].Close()
	}
	for i, sz := range sizes {
		wg.Add(1)
		go func(i int, sz int64) {
			defer wg.Done()
			<-start
			var err error
			for {
				_, err = clients[i%len(clients)].CreateLease(ctx, v1.LeaseRequest{
					Owner: "load", Session: "s-load", Project: "p", Workload: v1.WorkloadContainer, EstMemoryBytes: sz,
				})
				// macOS caps the unix socket backlog at kern.ipc.somaxconn
				// (128): excess connects are refused before any byte is
				// sent, so retrying them cannot double-admit.
				if !connectRefused(err) {
					break
				}
				connRetries.Add(1)
				time.Sleep(time.Millisecond)
			}
			mu.Lock()
			defer mu.Unlock()
			switch code := v1.CodeOf(err); {
			case err == nil:
				res.accepted = append(res.accepted, sz)
			case code == v1.CodeInsufficientMemory:
				res.refusedSize = append(res.refusedSize, sz)
				res.byCode[code]++
			default:
				res.byCode[code]++
				if !code.IsRefusal() {
					t.Errorf("unexpected error: %v", err)
				}
			}
		}(i, sz)
	}
	t0 := time.Now()
	close(start)
	wg.Wait()
	res.took = time.Since(t0)
	close(stop)
	<-watcherDone
	res.maxSeen = maxSeen.Load()
	res.connRetries = connRetries.Load()
	return res
}

func concurrencyNode(t *testing.T, maxActive int) *testNode {
	return newTestNode(t, func(c *config.NodeConfig) {
		c.Leases.MaxActive = maxActive
		c.Capacity.Fake = &config.FakeCapacity{TotalMemoryGB: 32, AvailableMemoryGB: 32, FreeDiskGB: 500}
	})
}

func sum(xs []int64) int64 {
	var s int64
	for _, x := range xs {
		s += x
	}
	return s
}

func TestConcurrentCreateNeverOvercommits(t *testing.T) {
	const requests = 1000
	schedulable := 28 * G // 32 GiB total - 4 GiB reserve, nothing observed in use

	t.Run("uniform", func(t *testing.T) {
		n := concurrencyNode(t, 100000)
		est := 37 * MiB
		want := int(schedulable / est) // analytic maximum: floor(28 GiB / 37 MiB) = 774
		sizes := make([]int64, requests)
		for i := range sizes {
			sizes[i] = est
		}
		r := burst(t, n, sizes)
		committed, active, _ := n.store.Committed(context.Background(), "")
		t.Logf("%d concurrent requests of 37 MiB in %v: %d accepted (analytic max %d), %d refused; committed %d of %d bytes; max committed seen during burst %d; %d backlog-refused connects retried",
			requests, r.took, len(r.accepted), want, len(r.refusedSize), committed, schedulable, r.maxSeen, r.connRetries)
		if len(r.accepted) != want || active != want {
			t.Fatalf("accepted %d (store %d), want exactly %d", len(r.accepted), active, want)
		}
		if committed > schedulable || r.maxSeen > schedulable || committed != int64(want)*est {
			t.Fatalf("overcommit: committed %d, max seen %d, schedulable %d", committed, r.maxSeen, schedulable)
		}
		if len(r.refusedSize) != requests-want {
			t.Fatalf("refusals %d, codes %v", len(r.refusedSize), r.byCode)
		}
	})

	t.Run("mixed sizes", func(t *testing.T) {
		n := concurrencyNode(t, 100000)
		rng := rand.New(rand.NewPCG(42, 7))
		sizes := make([]int64, 1200)
		for i := range sizes {
			sizes[i] = 16*MiB + rng.Int64N(240*MiB)
		}
		r := burst(t, n, sizes)
		committed, active, _ := n.store.Committed(context.Background(), "")
		free := schedulable - committed
		t.Logf("%d concurrent mixed requests in %v: %d accepted, %d refused; committed %d of %d bytes (final free %d)",
			len(sizes), r.took, len(r.accepted), len(r.refusedSize), committed, schedulable, free)
		if committed > schedulable || r.maxSeen > schedulable {
			t.Fatalf("overcommit: committed %d max seen %d", committed, r.maxSeen)
		}
		if sum(r.accepted) != committed || len(r.accepted) != active {
			t.Fatalf("accepted %d bytes / %d leases but store has %d / %d", sum(r.accepted), len(r.accepted), committed, active)
		}
		// Free memory only shrinks during the burst, so every refused request
		// must also be larger than what is left at the end: no request was
		// refused while it would have fit.
		for _, sz := range r.refusedSize {
			if sz <= free {
				t.Fatalf("request of %d bytes refused although %d bytes are free", sz, free)
			}
		}
		if len(r.refusedSize) == 0 {
			t.Fatal("test is vacuous: nothing was refused")
		}
	})

	t.Run("max_active binds", func(t *testing.T) {
		n := concurrencyNode(t, 50)
		sizes := make([]int64, requests)
		for i := range sizes {
			sizes[i] = MiB
		}
		r := burst(t, n, sizes)
		t.Logf("%d concurrent 1 MiB requests with max_active=50 in %v: %d accepted, refusals %v", requests, r.took, len(r.accepted), r.byCode)
		if len(r.accepted) != 50 || r.byCode[v1.CodeMaxLeases] != requests-50 {
			t.Fatalf("accepted %d, codes %v", len(r.accepted), r.byCode)
		}
	})
}
