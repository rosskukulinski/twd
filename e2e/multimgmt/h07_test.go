//go:build e2emultimgmt

package multimgmt

import (
	"fmt"
	"slices"
	"syscall"
	"testing"
	"time"
)

// TestMM_H07_DeviceDeleteLostOnCrash probes H07: with -device-delete-delay
// (twd up uses 8 s), a killed object's device delete waits in memory
// (time.AfterFunc, m.pending). A replica that dies inside that window never
// deletes the device, and no other replica knows to.
// Invariant I6: at quiescence no device is left for a killed object.
func TestMM_H07_DeviceDeleteLostOnCrash(t *testing.T) {
	r := newRun(t, "H07")
	c := newCluster(t, r, 2)
	c.extraArgs = []string{"-device-delete-delay", "3s"}
	for i := range 2 {
		c.start(i)
		if _, err := c.waitReady(i, 10*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	s := c.setupTenant(0, 1000)
	stop := make(chan struct{})
	defer close(stop)
	c.heartbeat(s, stop)

	ready := func(name, node string) string {
		var o struct{ ObjectID string }
		c.must(1, "h1", "POST", "/v1/objects", map[string]any{"tenantID": s.tenantID, "namespace": "SupportAgent", "objectName": name}, &o)
		deadline := time.Now().Add(10 * time.Second)
		for {
			st, _, _ := c.callAs(1, "h1", "POST", fmt.Sprintf("/v1/objects/%s/credentials?epoch=%d", o.ObjectID, s.epoch), nil)
			if st/100 == 2 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("no credential for %s", name)
			}
			time.Sleep(100 * time.Millisecond)
		}
		c.must(1, "h1", "PATCH", "/v1/objects/"+o.ObjectID, map[string]any{"status": "ready", "nodeID": "1", "stableNodeID": node,
			"magicDNS": o.ObjectID + ".t.ts.net"}, nil)
		return o.ObjectID
	}
	deleted := func(node string) bool {
		c.ts.mu.Lock()
		defer c.ts.mu.Unlock()
		return slices.Contains(c.ts.deletedDevs, node)
	}

	// Control: kill via r0, r0 stays up → device deleted after ~3 s.
	ctl := ready("control", "nCTL")
	c.must(0, "", "DELETE", "/v1/objects/"+ctl+"?reason=h07", nil, nil)
	time.Sleep(5 * time.Second)
	t.Logf("control: device nCTL deleted = %v", deleted("nCTL"))
	if !deleted("nCTL") {
		t.Fatal("control failed: the device was not deleted even without a crash")
	}

	// Kill via r0, then SIGKILL r0 inside the 3 s window; r1 keeps running.
	victim := ready("victim", "nVIC")
	c.must(0, "", "DELETE", "/v1/objects/"+victim+"?reason=h07", nil, nil)
	time.Sleep(500 * time.Millisecond)
	c.stop(0, syscall.SIGKILL)
	time.Sleep(15 * time.Second) // well past the delay; r1's reaper ran several times
	c.start(0)                   // even a restarted r0 has no record of it
	c.waitReady(0, 10*time.Second)
	time.Sleep(5 * time.Second)
	t.Logf("crash: device nVIC deleted = %v (all deleted: %v)", deleted("nVIC"), c.ts.deletedDevs)
	if !deleted("nVIC") {
		t.Errorf("I6 violated: the device of killed object %s was never deleted after the replica holding its delayed delete crashed", victim)
	}
}
