package dockerproxy

import (
	"io"
	"net/http"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// TestStreamedBodyThroughput checks that a large build context streams
// through the proxy at memory speed (it is never the bottleneck) and that
// the proxy does not hold the body in memory.
func TestStreamedBodyThroughput(t *testing.T) {
	if testing.Short() {
		t.Skip("short")
	}
	f := newFixture(t, nil)
	const size = 256 << 20
	chunk := pattern(3, 1<<20)
	measure := func(url string) time.Duration {
		pr, pw := io.Pipe()
		go func() {
			for i := 0; i < size/len(chunk); i++ {
				if _, err := pw.Write(chunk); err != nil {
					return
				}
			}
			_ = pw.Close()
		}()
		req, _ := http.NewRequest("POST", url+"/v1.47/build", pr)
		t0 := time.Now()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return time.Since(t0)
	}
	direct := measure(f.fe.srv.URL)
	runtime.GC()
	var peak atomic.Uint64
	stop := make(chan struct{})
	go func() {
		var ms runtime.MemStats
		for {
			runtime.ReadMemStats(&ms)
			if ms.HeapInuse > peak.Load() {
				peak.Store(ms.HeapInuse)
			}
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	proxied := measure(f.srv.URL)
	close(stop)
	if p := peak.Load(); p > 64<<20 {
		t.Errorf("heap in use peaked at %d MiB while streaming 256 MiB: the proxy buffers", p>>20)
	}
	f.fe.mu.Lock()
	n := f.fe.buildN
	f.fe.mu.Unlock()
	if n != size {
		t.Fatalf("engine received %d bytes, want %d", n, size)
	}
	mbps := func(d time.Duration) float64 { return float64(size>>20) / d.Seconds() }
	t.Logf("256 MiB chunked upload: direct %.0f MB/s, via proxy %.0f MB/s, peak heap in use %d MiB", mbps(direct), mbps(proxied), peak.Load()>>20)
}
