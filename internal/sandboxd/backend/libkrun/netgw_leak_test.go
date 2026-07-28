package libkrun

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// A gateway must return every resource it took: netstack starts a TCP
// processor goroutine per CPU, so a per-sandbox leak here is multiplied by
// the number of launches. Refs: SEC-10
func TestNetGateway_Close_LeaksNoGoroutines(t *testing.T) {
	settle := func() int {
		for i := 0; i < 20; i++ {
			runtime.GC()
			time.Sleep(20 * time.Millisecond)
		}
		return runtime.NumGoroutine()
	}
	before := settle()
	for i := 0; i < 5; i++ {
		gw, err := bindNetGateway(filepath.Join(shortTempDir(t), proxySocketName), &stubAuthorizer{}, nil)
		if err != nil {
			t.Fatalf("bind: %v", err)
		}
		if err := gw.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
	after := settle()
	if after > before+2 {
		t.Errorf("goroutines %d -> %d after 5 gateway launches: netstack not destroyed", before, after)
	}
}
