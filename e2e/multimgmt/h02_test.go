//go:build e2emultimgmt

package multimgmt

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestMM_H02_DoubleMint probes H02: every replica's Start re-enqueues every
// pending object (mgmt/service.go:156) and mintOne checks, then acts
// (GetObject pending → CreateKey → custody Put → UpdateObject), so replicas
// starting together can mint the same object more than once.
// Invariant I3: at most one key per object; extra keys must be deleted.
func TestMM_H02_DoubleMint(t *testing.T) {
	const nObjects = 40
	r := newRun(t, "H02")
	c := newCluster(t, r, 3)
	db := c.db()

	// 1. One replica; tenant, host, deployment.
	c.start(0)
	if _, err := c.waitReady(0, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	s := c.setupTenant(0, 1000)
	stop := make(chan struct{})
	defer close(stop)
	c.heartbeat(s, stop)

	// 2. Hold minting, create objects: they stay pending.
	c.ts.setHold(true)
	for k := range nObjects {
		c.must(0, "h1", "POST", "/v1/objects", map[string]any{"tenantID": s.tenantID, "namespace": "SupportAgent",
			"objectName": fmt.Sprintf("o%03d", k)}, nil)
	}
	st, _ := objectStatuses(db)
	pending := 0
	for _, v := range st {
		if v == "pending" {
			pending++
		}
	}
	t.Logf("created %d objects, %d pending, fakeTS 503s so far: %d", len(st), pending, c.ts.held)

	// 3. Kill r0 and start all three at once: each Start enqueues every
	// pending object.
	c.stop(0, syscall.SIGKILL)
	for i := range 3 {
		c.start(i)
	}
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.waitReady(i, 10*time.Second); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	// 4. Release minting; wait until nothing is pending.
	c.ts.setHold(false)
	deadline := time.Now().Add(60 * time.Second)
	for {
		st, _ = objectStatuses(db)
		left := 0
		for _, v := range st {
			if v == "pending" {
				left++
			}
		}
		if left == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d objects still pending after 60 s", left)
		}
		time.Sleep(250 * time.Millisecond)
	}
	time.Sleep(2 * time.Second) // let in-flight duplicate mints land

	// 5. Oracle (I3).
	kc, live := c.ts.keyCounts(), c.ts.liveKeyCounts()
	var dup, liveDup []string
	total := 0
	for id := range st {
		total += kc[id]
		if kc[id] > 1 {
			dup = append(dup, fmt.Sprintf("%s:%d", id, kc[id]))
		}
		if live[id] > 1 {
			liveDup = append(liveDup, fmt.Sprintf("%s:%d", id, live[id]))
		}
	}
	sort.Strings(dup)
	sort.Strings(liveDup)
	t.Logf("objects=%d keys created=%d objects with >1 key=%d, with >1 undeleted key=%d", len(st), total, len(dup), len(liveDup))
	if len(dup) > 0 {
		t.Errorf("I3 violated: %d/%d objects were minted more than once: %v", len(dup), len(st), dup)
	}
	// I3 (custody): each minted object's credential must be releasable and
	// must be the object's own, still-live key.
	liveIDs := map[string]bool{}
	c.ts.mu.Lock()
	for k := range c.ts.liveKeys {
		liveIDs[k] = true
	}
	c.ts.mu.Unlock()
	var badCred []string
	for id, status := range st {
		var keyID string
		db.QueryRow("SELECT key_id FROM objects WHERE id=$1", id).Scan(&keyID)
		code, b, err := c.callAs(c.firstRunning(), "h1", "POST", fmt.Sprintf("/v1/objects/%s/credentials?epoch=%d", id, s.epoch), nil)
		var cred struct{ AuthKey string }
		json.Unmarshal(b, &cred)
		got := strings.TrimPrefix(cred.AuthKey, "fake-authkey-")
		switch {
		case err != nil || code/100 != 2:
			badCred = append(badCred, fmt.Sprintf("%s(%s): credential %d %.120s", id, status, code, b))
		case got != keyID:
			badCred = append(badCred, fmt.Sprintf("%s: custody has key %s, object row has %s", id, got, keyID))
		case !liveIDs[got]:
			badCred = append(badCred, fmt.Sprintf("%s: custody key %s was deleted at the API", id, got))
		}
	}
	sort.Strings(badCred)
	t.Logf("credential check: %d/%d objects bad", len(badCred), len(st))
	if len(badCred) > 0 {
		t.Errorf("I3 violated (custody): %d objects cannot get a valid credential: %v", len(badCred), badCred)
	}
	if len(liveDup) > 0 {
		t.Errorf("I3 violated: %d objects have more than one undeleted key: %v", len(liveDup), liveDup)
	}
}
