//go:build e2emultimgmt

package multimgmt

import (
	"fmt"
	"testing"
	"time"
)

// TestMM_H11_FileCustodyPerReplica probes H11: with the default
// -custody file, each replica keeps minted auth keys in its own state dir,
// so a credential minted by one replica can't be released by another.
// Invariant I3/I8: any replica can release any minted object's credential.
func TestMM_H11_FileCustodyPerReplica(t *testing.T) {
	const n = 12
	r := newRun(t, "H11")
	c := newCluster(t, r, 2)
	c.extraArgs = []string{"-custody", "file"} // the default; overrides the harness's setec (last flag wins)
	db := c.db()
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
	for k := range n {
		// Alternate the replica that serves the create (and enqueues the mint).
		c.must(k%2, "h1", "POST", "/v1/objects", map[string]any{"tenantID": s.tenantID, "namespace": "SupportAgent",
			"objectName": fmt.Sprintf("o%03d", k)}, nil)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, _ := objectStatuses(db)
		left := 0
		for _, v := range st {
			if v == "pending" {
				left++
			}
		}
		if left == 0 && len(st) == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d still pending", left)
		}
		time.Sleep(200 * time.Millisecond)
	}
	st, _ := objectStatuses(db)
	fail := [2]int{}
	both := 0
	for id := range st {
		okOn := 0
		for i := range 2 {
			code, b, err := c.callAs(i, "h1", "POST", fmt.Sprintf("/v1/objects/%s/credentials?epoch=%d", id, s.epoch), nil)
			if err != nil || code/100 != 2 {
				fail[i]++
				t.Logf("r%d %s: %d %.100s", i, id, code, b)
			} else {
				okOn++
			}
		}
		if okOn == 0 {
			both++
		}
	}
	t.Logf("credential failures: r0 %d/%d, r1 %d/%d, on both %d", fail[0], n, fail[1], n, both)
	if fail[0]+fail[1] > 0 {
		t.Errorf("I3/I8 violated: with -custody file, %d credential releases failed because the key is in another replica's custody", fail[0]+fail[1])
	}
}
