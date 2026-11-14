//go:build e2emultimgmt

package multimgmt

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestMM_H06_KillPushAcrossReplicas probes H06: mgmt keeps each host's push
// target in memory on the replica that served the host's registration
// (m.pushURLs), so a kill served by another replica can't push to the host
// and only reaches it through heartbeat revocations.
// Invariant I6: the owning host learns of a kill within 1 s, whichever
// replica served the kill.
func TestMM_H06_KillPushAcrossReplicas(t *testing.T) {
	r := newRun(t, "H06")
	c := newCluster(t, r, 2)
	for i := range 2 {
		c.start(i)
		if _, err := c.waitReady(i, 10*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	// A fake host with a push endpoint on a unix socket.
	var mu sync.Mutex
	got := map[string]time.Time{} // object → first time the host learned of its kill
	via := map[string]string{}
	learn := func(id, how string) {
		mu.Lock()
		defer mu.Unlock()
		if _, ok := got[id]; !ok {
			got[id], via[id] = time.Now(), how
		}
	}
	sock := filepath.Join(os.TempDir(), fmt.Sprintf("twd-mm-%d-push.sock", os.Getpid()))
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(sock)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var b struct{ ObjectID string }
		json.NewDecoder(req.Body).Decode(&b)
		if req.URL.Path == "/v1/push/kill" {
			learn(b.ObjectID, "push")
		}
		w.WriteHeader(200)
	})}
	go srv.Serve(ln)
	defer srv.Close()

	s := c.setupTenant(0, 1000)
	// h1 (re)registers through r0 with its push socket; it heartbeats only to r0.
	c.must(0, "h1", "PUT", "/v1/hosts/h1", map[string]any{"capacityNodes": 10000, "tiers": []string{"process"},
		"pushURL": "unix://" + sock}, nil)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			_, b, _ := c.callAs(0, "h1", "PUT", "/v1/hosts/h1/status", map[string]any{"liveNodes": 0,
				"leases": []map[string]any{{"namespace": s.workload, "epoch": s.epoch}}})
			var hb struct{ RevokeObjects []string }
			json.Unmarshal(b, &hb)
			for _, id := range hb.RevokeObjects {
				learn(id, "heartbeat")
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
	ids := make([]string, 2)
	for k := range 2 {
		var o struct{ ObjectID string }
		c.must(0, "h1", "POST", "/v1/objects", map[string]any{"tenantID": s.tenantID, "namespace": "SupportAgent",
			"objectName": fmt.Sprintf("k%d", k)}, &o)
		ids[k] = o.ObjectID
	}
	time.Sleep(500 * time.Millisecond)
	var lat [2]time.Duration
	for k, id := range ids { // k = the replica serving the kill
		start := time.Now()
		st, b, err := c.call(k, "DELETE", "/v1/objects/"+id+"?reason=h06", nil)
		if err != nil || st/100 != 2 {
			t.Fatalf("kill via r%d: %d %v %s", k, st, err, b)
		}
		for time.Since(start) < 15*time.Second {
			mu.Lock()
			at, ok := got[id]
			mu.Unlock()
			if ok {
				lat[k] = at.Sub(start)
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		mu.Lock()
		t.Logf("kill via r%d (host registered with r0): host learned in %v via %q; kill response %.200s", k, lat[k].Round(time.Millisecond), via[id], b)
		mu.Unlock()
	}
	if lat[0] == 0 || lat[0] > time.Second {
		t.Errorf("control: a kill through the host's own replica took %v", lat[0])
	}
	if lat[1] == 0 || lat[1] > time.Second {
		t.Errorf("I6 violated: a kill served by another replica reached the host after %v (via %q), not within 1 s", lat[1], via[ids[1]])
	}
}
