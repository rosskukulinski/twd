//go:build e2emultimgmt

package multimgmt

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMM_S12_SmartPlacement (ADR-029): three replicas on Postgres place
// from the same stored path measurements. Fake hosts report through
// different replicas; concurrent deploys through every replica must agree
// on the host and the explanation (scoreHosts is deterministic and the
// measurements are in the store, not in replica memory). Concurrent
// re-places (`replace`) of a workload held by a live host race on
// MoveLease: exactly one move wins, the lease has one holder (I1), the old
// holder is revoked at its next heartbeat and fenced (I2).
func TestMM_S12_SmartPlacement(t *testing.T) {
	r := newRun(t, "S12")
	c := newCluster(t, r, 3)
	for i := range 3 {
		c.start(i)
		if _, err := c.waitReady(i, 10*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	keyFile := r.dir + "/api.env"
	writeAPIKey(t, keyFile)
	var tn struct{ TenantID string }
	c.must(0, "", "POST", "/v1/tenants", map[string]any{"tailnet": "f.ts.net", "mintMode": "apikey", "apiKeyFile": keyFile}, &tn)
	hosts := []string{"h1", "h2", "h3"}
	for i, h := range hosts {
		c.must(i, h, "PUT", "/v1/hosts/"+h, map[string]any{"capacityNodes": 100, "tiers": []string{"process"}}, nil)
	}
	// Heartbeats keep every host live (h1 through r0, h2 through r1, …).
	stop := make(chan struct{})
	defer close(stop)
	var hbMu sync.Mutex
	leases := map[string][]map[string]any{}
	hb := func(rep int, host string) []string {
		hbMu.Lock()
		ls := append([]map[string]any{}, leases[host]...)
		hbMu.Unlock()
		_, b, _ := c.callAs(rep, host, "PUT", "/v1/hosts/"+host+"/status", map[string]any{"liveNodes": 0, "leases": ls})
		var resp struct{ Revoke []string }
		json.Unmarshal(b, &resp)
		return resp.Revoke
	}
	go func() {
		for {
			for i, h := range hosts {
				hb(i, h)
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Second):
			}
		}
	}()
	stateless := makeBundle(map[string]string{"twd.jsonc": `{"tailnet": {"tags": ["tag:w"], "connects": ["tag:db"]}}`, "dist/index.js": "export default {}"})
	type dep struct {
		DeploymentID, Host string
		Epoch              int64
		Placement          struct{ Reason, Summary string }
	}
	deploy := func(rep int, body map[string]any) (int, dep, string) {
		body["tenantID"], body["bundle"] = tn.TenantID, stateless
		st, b, _ := c.call(rep, "POST", "/v1/deployments", body)
		var d dep
		json.Unmarshal(b, &d)
		var e struct{ Code string }
		json.Unmarshal(b, &e)
		return st, d, e.Code
	}
	// Tenant presence on every host (a host may only report paths for a
	// tenant it runs), then reports through different replicas: h1 relayed,
	// h2 and h3 direct at the same RTT and capacity (the host ID decides).
	for i, h := range hosts {
		if st, d, code := deploy(i, map[string]any{"workload": "seed-" + h, "host": h}); st != 202 || d.Host != h {
			t.Fatalf("seed %s: %d %s %+v", h, st, code, d)
		}
	}
	report := func(rep int, h, path string, rtt float64) {
		c.must(rep, h, "PUT", "/v1/hosts/"+h+"/paths", map[string]any{"tenants": []map[string]any{{"tenantID": tn.TenantID,
			"targets": []map[string]any{{"target": "tag:db", "path": path, "rttMs": rtt, "ageMs": 100, "peer": "pg-1"}}}}}, nil)
	}
	report(2, "h1", "derp", 20)
	report(0, "h2", "direct", 3)
	report(1, "h3", "direct", 3)

	// 1. Concurrent first deploys of 12 workloads through all replicas.
	type res struct {
		st   int
		d    dep
		code string
	}
	var wg sync.WaitGroup
	out := make([]res, 12)
	for k := range out {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, d, code := deploy(k%3, map[string]any{"workload": fmt.Sprintf("app%02d", k)})
			out[k] = res{st, d, code}
		}()
	}
	wg.Wait()
	want := "placed on h2: tag:db direct 3.0 ms (h3: direct 3.0 ms, h1: relayed 20 ms)" // others best first
	for k, o := range out {
		if o.st != 202 || o.d.Host != "h2" || o.d.Placement.Reason != "smart" || o.d.Placement.Summary != want {
			t.Errorf("app%02d via r%d: %d %s host=%s %q, want h2 %q", k, k%3, o.st, o.code, o.d.Host, o.d.Placement.Summary, want)
		}
	}

	// 2. A workload on h1 (pinned), held live; then every replica re-places
	// it at once. One MoveLease wins.
	st, mv, code := deploy(0, map[string]any{"workload": "mover", "host": "h1"})
	if st != 202 || mv.Host != "h1" {
		t.Fatalf("pin mover: %d %s %+v", st, code, mv)
	}
	hbMu.Lock()
	leases["h1"] = []map[string]any{{"namespace": "mover", "epoch": mv.Epoch}}
	hbMu.Unlock()
	if rv := hb(0, "h1"); len(rv) != 0 {
		t.Fatalf("h1 revoked before the move: %v", rv)
	}
	const racers = 6
	moves := make([]res, racers)
	start := make(chan struct{})
	for k := range moves {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			st, d, code := deploy(k%3, map[string]any{"workload": "mover", "replace": true})
			moves[k] = res{st, d, code}
		}()
	}
	close(start)
	wg.Wait()
	epochs := map[int64]bool{}
	okN := 0
	codes := map[string]int{}
	for k, m := range moves {
		switch {
		case m.st == 202:
			okN++
			if m.d.Host != "h2" {
				t.Errorf("I1/agreement: re-place %d via r%d landed on %s (%q)", k, k%3, m.d.Host, m.d.Placement.Summary)
			}
			if epochs[m.d.Epoch] {
				t.Errorf("I1: two deploys got epoch %d", m.d.Epoch)
			}
			epochs[m.d.Epoch] = true
		case m.st == 409 && (m.code == "lease_changed" || m.code == "lease_held"):
			codes[m.code]++
		default:
			t.Errorf("re-place %d via r%d: unexpected %d %s", k, k%3, m.st, m.code)
		}
	}
	db := c.db()
	var holder string
	var epoch int64
	if err := db.QueryRow("SELECT holder, epoch FROM leases WHERE name=$1", tn.TenantID+"/mover").Scan(&holder, &epoch); err != nil {
		t.Fatal(err)
	}
	if holder != "h2" {
		t.Fatalf("I1: mover's lease holder %q after the re-place race, want h2", holder)
	}
	var movedRecs int
	db.QueryRow(`SELECT count(*) FROM audit WHERE kind='deploy' AND raw LIKE '%"movedFrom":"h1"%' AND raw LIKE '%"workload":"mover"%'`).Scan(&movedRecs)
	if movedRecs != 1 {
		t.Errorf("I1: %d deploys moved mover off h1, want exactly 1 (the MoveLease winner)", movedRecs)
	}
	// The old holder is revoked and fenced, through every replica.
	for rep := range 3 {
		if rv := hb(rep, "h1"); len(rv) != 1 || !strings.Contains(rv[0], "mover") {
			t.Errorf("I1: h1 not revoked via r%d after the move: %v", rep, rv)
		}
	}
	hbMu.Lock()
	leases["h1"] = nil
	leases["h2"] = []map[string]any{{"namespace": "mover", "epoch": epoch}}
	hbMu.Unlock()
	if rv := hb(1, "h2"); len(rv) != 0 {
		t.Errorf("new holder h2 revoked: %v", rv)
	}
	// Re-placing again stays (h2 is the best host): no churn.
	if st, d, code := deploy(2, map[string]any{"workload": "mover", "replace": true}); st != 202 || d.Host != "h2" || d.Placement.Reason != "sticky" {
		t.Errorf("re-place on the best host: %d %s %+v", st, code, d)
	}
	t.Logf("S12: 12 concurrent placements agreed (%q); %d/%d concurrent re-places succeeded (refused: %v), 1 move audited, lease h2@%d", want, okN, racers, codes, epoch)
}
