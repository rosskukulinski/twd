package mgmt

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestMM_F8_PushTargetShared (H06): a host's push target is persisted at
// registration, so a replica that didn't serve the registration can push to
// it, and a replica with a stale cached target follows a re-registration.
func TestMM_F8_PushTargetShared(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := OpenStore(ctx, filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cust, _ := OpenFileCustody(filepath.Join(dir, "c"))
	mk := func() *Mgmt {
		m, err := New(Config{Store: st, Custody: cust, Broker: &Broker{}, IDSecret: []byte("0123456789abcdef"),
			ReapInterval: time.Hour, PushTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(m.Close)
		return m
	}
	a, b := mk(), mk()
	var mu sync.Mutex
	hits := map[string]int{}
	serve := func(name string) string {
		sock := filepath.Join(os.TempDir(), "twd-f8-"+name+"-"+filepath.Base(dir)+".sock")
		os.Remove(sock)
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var v map[string]any
			json.NewDecoder(r.Body).Decode(&v)
			mu.Lock()
			hits[name]++
			mu.Unlock()
		})}
		go srv.Serve(ln)
		t.Cleanup(func() { srv.Close(); os.Remove(sock) })
		return "unix://" + sock
	}
	first, second := serve("first"), serve("second")
	if _, _, err := a.RegisterHost(ctx, RegisterRequest{HostID: "h1", CapacityNodes: 1, Tiers: []string{"process"}, PushURL: first}, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := b.push("h1", "/v1/push/kill", map[string]any{"objectID": "x"}); err != nil {
		t.Fatalf("replica B (h1 registered with A) must push from the stored target: %v", err)
	}
	// h1 moves its push listener and re-registers with A; B's cache is stale.
	if _, _, err := a.RegisterHost(ctx, RegisterRequest{HostID: "h1", CapacityNodes: 1, Tiers: []string{"process"}, PushURL: second}, "", ""); err != nil {
		t.Fatal(err)
	}
	os.Remove(first[len("unix://"):]) // the old listener is gone
	if err := b.push("h1", "/v1/push/kill", map[string]any{"objectID": "y"}); err != nil {
		t.Fatalf("replica B must follow the re-registration after a failed push: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["first"] != 1 || hits["second"] != 1 {
		t.Fatalf("hits = %v, want first:1 second:1", hits)
	}
}
