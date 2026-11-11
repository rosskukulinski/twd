//go:build e2emultimgmt

package multimgmt

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ---- seeded scenario runner (LOOP.md "Scenarios") ----

const chaosMaxLive = 5

func scenarioSeed() uint64 {
	if s, err := strconv.ParseUint(os.Getenv("TWD_MM_SEED"), 10, 64); err == nil {
		return s
	}
	return 1
}

func scenarioDuration() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("TWD_MM_DURATION")); err == nil {
		return d
	}
	return 60 * time.Second
}

type opRec struct {
	T       int64  `json:"t"` // unix ms at start
	Op      string `json:"op"`
	Arg     string `json:"arg,omitempty"`
	Replica int    `json:"replica"`
	Status  int    `json:"status"`
	Code    string `json:"code,omitempty"`
	Err     string `json:"err,omitempty"`
	Ms      int64  `json:"ms"`
}

type history struct {
	mu   sync.Mutex
	f    *os.File
	recs []opRec
}

func newHistory(t *testing.T, dir string) *history {
	f, err := os.Create(filepath.Join(dir, "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return &history{f: f}
}

func (h *history) add(r opRec) {
	b, _ := json.Marshal(r)
	h.mu.Lock()
	h.recs = append(h.recs, r)
	h.f.Write(append(b, '\n'))
	h.mu.Unlock()
}

// rng is a goroutine-safe seeded source.
type rng struct {
	mu sync.Mutex
	r  *rand.Rand
}

func newRNG(seed uint64) *rng { return &rng{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))} }
func (g *rng) intn(n int) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.r.IntN(n)
}

// callAny sends a request to a random running replica, as a load-balanced
// svc:twd-mgmt would, and retries another replica on a transport error.
func (c *cluster) callAny(g *rng, host, method, path string, body any) (int, []byte, int, error) {
	var lastErr error
	for range 4 {
		var up []int
		for i := range c.replicas {
			if c.running(i) {
				up = append(up, i)
			}
		}
		if len(up) == 0 {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		i := up[g.intn(len(up))]
		st, b, err := c.callAs(i, host, method, path, body)
		if err == nil {
			return st, b, i, nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	return 0, nil, -1, lastErr
}

type known struct {
	mu    sync.Mutex
	roots []string
	all   []string
}

func (k *known) add(id string, root bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.all = append(k.all, id)
	if root {
		k.roots = append(k.roots, id)
	}
}

func (k *known) pick(g *rng, roots bool) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	l := k.all
	if roots {
		l = k.roots
	}
	if len(l) == 0 {
		return ""
	}
	return l[g.intn(len(l))]
}

// runWorkload runs workers mixed operations until ctx is done.
func runWorkload(ctx context.Context, c *cluster, g *rng, s setup, h *history, workers int) {
	var k known
	var seq struct {
		sync.Mutex
		n int
	}
	next := func(p string) string {
		seq.Lock()
		defer seq.Unlock()
		seq.n++
		return fmt.Sprintf("%s%05d", p, seq.n)
	}
	do := func(op, arg, host, method, path string, body any) (int, []byte) {
		st0 := time.Now()
		st, b, rep, err := c.callAny(g, host, method, path, body)
		r := opRec{T: st0.UnixMilli(), Op: op, Arg: arg, Replica: rep, Status: st, Ms: time.Since(st0).Milliseconds()}
		if err != nil {
			r.Err = err.Error()
		} else if st/100 != 2 {
			var e struct{ Code string }
			json.Unmarshal(b, &e)
			r.Code = e.Code
		}
		h.add(r)
		return st, b
	}
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				switch p := g.intn(100); {
				case p < 30: // create a root
					st, b := do("create", "", "h1", "POST", "/v1/objects", map[string]any{"tenantID": s.tenantID, "namespace": "SupportAgent", "objectName": next("r")})
					var o struct{ ObjectID string }
					if st/100 == 2 && json.Unmarshal(b, &o) == nil {
						k.add(o.ObjectID, true)
					}
				case p < 55: // spawn under a root
					root := k.pick(g, true)
					if root == "" {
						continue
					}
					st, b := do("spawn", root, "h1", "POST", "/v1/objects", map[string]any{"tenantID": s.tenantID, "namespace": "LookupAgent",
						"objectName": next("c"), "parent": map[string]any{"objectID": root}})
					var o struct{ ObjectID string }
					if st/100 == 2 && json.Unmarshal(b, &o) == nil {
						k.add(o.ObjectID, false)
					}
				case p < 80: // join: credential, then ready
					id := k.pick(g, false)
					if id == "" {
						continue
					}
					if st, _ := do("credential", id, "h1", "POST", fmt.Sprintf("/v1/objects/%s/credentials?epoch=%d&wait=2", id, s.epoch), nil); st/100 == 2 {
						do("ready", id, "h1", "PATCH", "/v1/objects/"+id, map[string]any{"status": "ready", "nodeID": "1",
							"stableNodeID": "n-" + id, "magicDNS": id + ".t.ts.net"})
					}
				case p < 90: // kill with cascade
					id := k.pick(g, false)
					if id == "" {
						continue
					}
					do("kill", id, "", "DELETE", "/v1/objects/"+id+"?cascade=true&reason=chaos", nil)
				default:
					do("status", "", "", "GET", "/v1/status", nil)
				}
			}
		}()
	}
	wg.Wait()
}

// hostLoop heartbeats h1 through any replica every 3 s (hostd's period).
func hostLoop(ctx context.Context, c *cluster, g *rng, s setup) {
	for ctx.Err() == nil {
		c.callAny(g, "h1", "PUT", "/v1/hosts/h1/status", map[string]any{"liveNodes": 0,
			"leases": []map[string]any{{"namespace": s.workload, "epoch": s.epoch}}})
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
}

// ---- oracle ----

type invariant struct {
	ID       string `json:"id"`
	OK       bool   `json:"ok"`
	Evidence string `json:"evidence"`
}

func checkInvariants(t *testing.T, c *cluster, g *rng, s setup, h *history, r *run) []invariant {
	db := c.db()
	var out []invariant
	add := func(id string, ok bool, f string, a ...any) {
		out = append(out, invariant{ID: id, OK: ok, Evidence: fmt.Sprintf(f, a...)})
	}
	// I9: convergence: nothing pending within KeyExpiry-ish bounds.
	quiesced := time.Now()
	deadline := quiesced.Add(120 * time.Second)
	var pending int
	for {
		db.QueryRow("SELECT COUNT(*) FROM objects WHERE tenant_id=$1 AND status='pending'", s.tenantID).Scan(&pending)
		if pending == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	drained := time.Since(quiesced).Round(time.Second)
	add("I9", pending == 0, "%d objects still pending 120 s after quiescence (drained in %v; %d keys minted in total)", pending, drained, len(c.ts.keyCounts()))
	if pending > 0 { // diagnostics before the container goes away
		var sb strings.Builder
		var nowMs int64
		db.QueryRow("SELECT (extract(epoch from clock_timestamp())*1000)::bigint").Scan(&nowMs)
		fmt.Fprintf(&sb, "db now ms %d\n", nowMs)
		prow, _ := db.Query(`SELECT id, parent_id, root_id, created_at, mint_claim, mint_claimed_at, key_id, deployment_id
			FROM objects WHERE tenant_id=$1 AND status='pending'`, s.tenantID)
		for prow.Next() {
			var id, parent, root, claim, key, dep string
			var created, claimedAt int64
			prow.Scan(&id, &parent, &root, &created, &claim, &claimedAt, &key, &dep)
			fmt.Fprintf(&sb, "%s parent=%q root=%s age=%ds claim=%q claimAge=%ds key=%q dep=%s keysCreated=%d\n", id, parent, root,
				(nowMs-created)/1000, claim, (nowMs-claimedAt)/1000, key, dep, c.ts.keyCounts()[id])
		}
		prow.Close()
		ar, _ := db.Query(`SELECT kind, object_id, raw FROM audit WHERE object_id IN (SELECT id FROM objects WHERE tenant_id=$1 AND status='pending') ORDER BY ts`, s.tenantID)
		for ar.Next() {
			var kind, oid, raw string
			ar.Scan(&kind, &oid, &raw)
			fmt.Fprintf(&sb, "audit %s %s %.300s\n", oid, kind, raw)
		}
		ar.Close()
		os.WriteFile(filepath.Join(r.dir, "pending.txt"), []byte(sb.String()), 0o600)
	}

	// I3: at most one undeleted key per object; every minted object's credential is releasable.
	liveDup := 0
	for _, n := range c.ts.liveKeyCounts() {
		if n > 1 {
			liveDup++
		}
	}
	rows, _ := db.Query("SELECT id FROM objects WHERE tenant_id=$1 AND status='minted'", s.tenantID)
	var minted []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		minted = append(minted, id)
	}
	rows.Close()
	badCred := 0
	for _, id := range minted {
		if st, _, _, _ := c.callAny(g, "h1", "POST", fmt.Sprintf("/v1/objects/%s/credentials?epoch=%d", id, s.epoch), nil); st/100 != 2 {
			badCred++
		}
	}
	add("I3", liveDup == 0 && badCred == 0, "objects with >1 undeleted key: %d; minted objects whose credential can't be released: %d/%d", liveDup, badCred, len(minted))

	// I5: no root over maxLive.
	var over int
	db.QueryRow(`SELECT COUNT(*) FROM (SELECT root_id FROM objects WHERE tenant_id=$1 AND id<>root_id AND status NOT IN ('killed','failed')
		GROUP BY root_id HAVING COUNT(*) > $2) x`, s.tenantID, chaosMaxLive).Scan(&over)
	add("I5", over == 0, "roots over maxLive %d: %d", chaosMaxLive, over)

	// I6: every killed object that had a device had it deleted.
	rows, _ = db.Query("SELECT stable_node_id FROM objects WHERE tenant_id=$1 AND status='killed' AND stable_node_id<>''", s.tenantID)
	c.ts.mu.Lock()
	deleted := map[string]bool{}
	for _, d := range c.ts.deletedDevs {
		deleted[d] = true
	}
	c.ts.mu.Unlock()
	var left []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		if !deleted[n] {
			left = append(left, n)
		}
	}
	rows.Close()
	add("I6", len(left) == 0, "devices of killed objects never deleted: %d %v", len(left), first(left, 5))

	// I7: the audit chain verifies.
	st, b, _, err := c.callAny(g, "", "GET", "/v1/audit/verification", nil)
	var av struct{ OK bool }
	json.Unmarshal(b, &av)
	add("I7", err == nil && st == 200 && av.OK, "audit verification: %d ok=%v %.200s", st, av.OK, b)

	// I8: availability: transport failures and 5xx over all ops (≥1 replica always up).
	h.mu.Lock()
	total, bad, pending := len(h.recs), 0, 0
	codes := map[string]int{}
	for _, rec := range h.recs {
		key := fmt.Sprintf("%s %d %s", rec.Op, rec.Status, rec.Code)
		codes[key]++
		switch {
		case rec.Status == 503 && rec.Code == "identity_pending":
			pending++ // documented, retryable backpressure (Retry-After), not an outage
		case rec.Status == 0 || rec.Status >= 500:
			bad++
		}
	}
	h.mu.Unlock()
	rate := 0.0
	if total > 0 {
		rate = float64(bad) / float64(total)
	}
	add("I8", rate <= 0.01, "%d/%d ops failed with a transport error or 5xx (%.2f%%); identity_pending (retryable) %d", bad, total, 100*rate, pending)
	keys := make([]string, 0, len(codes))
	for k := range codes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, "%s: %d\n", k, codes[k])
	}
	os.WriteFile(filepath.Join(r.dir, "op-codes.txt"), []byte(sb.String()), 0o600)

	// I11: no deadlocks in Postgres.
	var deadlocks int64
	db.QueryRow("SELECT deadlocks FROM pg_stat_database WHERE datname = current_database()").Scan(&deadlocks)
	add("I11", deadlocks == 0, "deadlocks: %d", deadlocks)

	rep, _ := json.MarshalIndent(out, "", "  ")
	os.WriteFile(filepath.Join(r.dir, "report.json"), rep, 0o600)
	return out
}

func first(l []string, n int) []string {
	if len(l) > n {
		return l[:n]
	}
	return l
}

// runScenario sets up 3 replicas, runs the workload with chaos(ctx, c, g)
// for the scenario duration, restores every replica, and checks invariants.
func runScenario(t *testing.T, name string, chaos func(ctx context.Context, c *cluster, g *rng)) {
	seed := scenarioSeed()
	r := newRun(t, fmt.Sprintf("%s-seed%d", name, seed))
	c := newCluster(t, r, 3)
	c.jsonc = strings.Replace(twdJSONC, `"maxLive": 64`, fmt.Sprintf(`"maxLive": %d`, chaosMaxLive), 1)
	for i := range 3 {
		c.start(i)
		if _, err := c.waitReady(i, 10*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	s := c.setupTenant(0, 600)
	g := newRNG(seed)
	h := newHistory(t, r.dir)
	ctx, cancel := context.WithTimeout(context.Background(), scenarioDuration())
	defer cancel()
	hostCtx, hostCancel := context.WithCancel(context.Background())
	defer hostCancel()
	go hostLoop(hostCtx, c, g, s)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); runWorkload(ctx, c, g, s, h, 6) }()
	go func() { defer wg.Done(); chaos(ctx, c, g) }()
	wg.Wait()
	// Quiescence: every replica back up.
	for i := range 3 {
		if !c.running(i) {
			c.start(i)
		}
		if _, err := c.waitReady(i, 15*time.Second); err != nil {
			t.Errorf("after the scenario: %v", err)
		}
	}
	inv := checkInvariants(t, c, g, s, h, r)
	for _, v := range inv {
		if v.OK {
			t.Logf("%s ok: %s", v.ID, v.Evidence)
		} else {
			t.Errorf("%s VIOLATED: %s", v.ID, v.Evidence)
		}
	}
}

// S2: SIGKILL a random replica every 5–20 s; restart it after 3 s.
func TestMM_S2_RandomKills(t *testing.T) {
	runScenario(t, "S2", func(ctx context.Context, c *cluster, g *rng) {
		for ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(5+g.intn(16)) * time.Second):
			}
			i := g.intn(3)
			c.stop(i, syscall.SIGKILL)
			time.Sleep(3 * time.Second)
			c.start(i)
			c.waitReady(i, 15*time.Second)
		}
	})
}

// signal sends sig to replica i (only the process this harness started)
// without waiting for it to exit (SIGSTOP/SIGCONT).
func (c *cluster) signal(i int, sig syscall.Signal) {
	c.repMu.Lock()
	cmd := c.replicas[i].cmd
	c.repMu.Unlock()
	if cmd != nil {
		cmd.Process.Signal(sig)
	}
}

// S3: SIGSTOP one replica for 50 s (longer than the 45 s namespace lease)
// while it serves load, then SIGCONT: the classic zombie writer.
func TestMM_S3_StallLongerThanLease(t *testing.T) {
	runScenario(t, "S3", func(ctx context.Context, c *cluster, g *rng) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
		i := g.intn(3)
		c.signal(i, syscall.SIGSTOP)
		select {
		case <-ctx.Done():
		case <-time.After(50 * time.Second):
		}
		c.signal(i, syscall.SIGCONT)
	})
}

// S4: blackhole one replica's Postgres connection for 30 s (its traffic is
// held, then released), while the others keep going.
func TestMM_S4_PostgresPartitionOneReplica(t *testing.T) {
	runScenario(t, "S4", func(ctx context.Context, c *cluster, g *rng) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
		i := g.intn(3)
		c.proxy(i).set("blackhole", 0)
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
		}
		c.proxy(i).set("pass", 0)
	})
}
