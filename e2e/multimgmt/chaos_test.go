//go:build e2emultimgmt

package multimgmt

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
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
	// Attempts: "replica:status:ms" per try (callAny retries on transport errors).
	Attempts []string `json:"attempts,omitempty"`
}

type history struct {
	mu      sync.Mutex
	f       *os.File
	recs    []opRec
	outages [][2]int64 // [from, to] unix ms: expected-unavailability windows declared by a scenario
}

// outage declares [from, to] as a window in which failures are expected
// (e.g. the store itself is down), excluded from I8 and reported apart.
func (h *history) outage(from, to time.Time) {
	h.mu.Lock()
	h.outages = append(h.outages, [2]int64{from.UnixMilli(), to.UnixMilli()})
	h.mu.Unlock()
}

func (h *history) inOutage(t int64) bool {
	for _, w := range h.outages {
		if t >= w[0] && t <= w[1] {
			return true
		}
	}
	return false
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
	st, b, i, err, _ := c.callAnyTrace(g, host, method, path, body)
	return st, b, i, err
}

func (c *cluster) callAnyTrace(g *rng, host, method, path string, body any) (int, []byte, int, error, []string) {
	var lastErr error
	var tries []string
	tried := map[int]bool{}
	for range 4 {
		// Like a load balancer with passive health checks: never retry the
		// same backend within a request, and skip backends that timed out
		// recently (ejected) while healthier ones are up.
		var up, fresh []int
		for i := range c.replicas {
			if c.running(i) && !tried[i] {
				up = append(up, i)
				if !c.ejected(i) {
					fresh = append(fresh, i)
				}
			}
		}
		if len(fresh) > 0 {
			up = fresh
		}
		if len(up) == 0 {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		i := up[g.intn(len(up))]
		t0 := time.Now()
		st, b, err := c.callAs(i, host, method, path, body)
		tries = append(tries, fmt.Sprintf("r%d:%d:%d", i, st, time.Since(t0).Milliseconds()))
		tried[i] = true
		if err == nil {
			return st, b, i, nil, tries
		}
		c.eject(i, 10*time.Second)
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	return 0, nil, -1, lastErr, tries
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
		st, b, rep, err, tries := c.callAnyTrace(g, host, method, path, body)
		r := opRec{T: st0.UnixMilli(), Op: op, Arg: arg, Replica: rep, Status: st, Ms: time.Since(st0).Milliseconds(), Attempts: tries}
		if err != nil {
			r.Err = err.Error()
		} else if st/100 != 2 {
			var e struct{ Code, Message string }
			json.Unmarshal(b, &e)
			r.Code = e.Code
			if st >= 500 {
				r.Err = e.Message // what the server said (diagnosis)
			}
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

	// Quota: live objects ≤ the tenant's maxObjects (H16).
	var liveN, maxObj int
	db.QueryRow("SELECT COUNT(*) FROM objects WHERE tenant_id=$1 AND status NOT IN ('killed','failed')", s.tenantID).Scan(&liveN)
	db.QueryRow("SELECT max_objects FROM tenants WHERE id=$1", s.tenantID).Scan(&maxObj)
	add("Quota", maxObj == 0 || liveN <= maxObj, "live objects %d, maxObjects %d", liveN, maxObj)

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
	total, bad, pending, expected := len(h.recs), 0, 0, 0
	codes := map[string]int{}
	for _, rec := range h.recs {
		key := fmt.Sprintf("%s %d %s", rec.Op, rec.Status, rec.Code)
		codes[key]++
		switch {
		case rec.Status == 503 && rec.Code == "identity_pending":
			pending++ // documented, retryable backpressure (Retry-After), not an outage
		case (rec.Status == 0 || rec.Status >= 500) && h.inOutage(rec.T):
			expected++ // inside a declared outage (the store was down)
		case rec.Status == 0 || rec.Status >= 500:
			bad++
		}
	}
	h.mu.Unlock()
	rate := 0.0
	if total > 0 {
		rate = float64(bad) / float64(total)
	}
	add("I8", rate <= 0.01, "%d/%d ops failed with a transport error or 5xx outside declared outages (%.2f%%); identity_pending (retryable) %d; failures inside outages %d", bad, total, 100*rate, pending, expected)
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

	// KillOnce: no object has more than one kill record (H09).
	var multiKill int
	db.QueryRow(`SELECT COUNT(*) FROM (SELECT object_id FROM audit WHERE kind='kill' AND object_id<>'' GROUP BY object_id HAVING COUNT(*) > 1) x`).Scan(&multiKill)
	add("KillOnce", multiKill == 0, "objects with more than one kill record: %d", multiKill)

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
func runScenario(t *testing.T, name string, chaos func(ctx context.Context, c *cluster, g *rng, h *history)) {
	runScenarioWith(t, name, nil, chaos)
}

// runScenarioWith is runScenario with prep run before the replicas start.
func runScenarioWith(t *testing.T, name string, prep func(c *cluster), chaos func(ctx context.Context, c *cluster, g *rng, h *history)) {
	seed := scenarioSeed()
	r := newRun(t, fmt.Sprintf("%s-seed%d", name, seed))
	c := newCluster(t, r, 3)
	c.jsonc = strings.Replace(twdJSONC, `"maxLive": 64`, fmt.Sprintf(`"maxLive": %d`, chaosMaxLive), 1)
	if prep != nil {
		prep(c)
	}
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
	go samplePGLocks(hostCtx, c, filepath.Join(r.dir, "locks.txt"))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); runWorkload(ctx, c, g, s, h, 6) }()
	go func() { defer wg.Done(); chaos(ctx, c, g, h) }()
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
	runScenario(t, "S2", func(ctx context.Context, c *cluster, g *rng, h *history) {
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
	runScenario(t, "S3", func(ctx context.Context, c *cluster, g *rng, h *history) {
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
	runScenario(t, "S4", func(ctx context.Context, c *cluster, g *rng, h *history) {
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

// restartPG restarts the Postgres container in place (data kept).
func restartPG(t *testing.T) {
	out, err := exec.Command("docker", "--context", env("PG_CTX", "orbstack"), "restart", "-t", "2", pgContainer).CombinedOutput()
	if err != nil {
		t.Errorf("docker restart: %v: %s", err, out)
	}
}

// S5: restart Postgres mid-load (every connection drops; in-flight
// transactions abort; the server is gone for a few seconds).
func TestMM_S5_PostgresRestart(t *testing.T) {
	runScenario(t, "S5", func(ctx context.Context, c *cluster, g *rng, h *history) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(15+g.intn(15)) * time.Second):
		}
		// The store itself is down: failures from 1 s before until 10 s after
		// the restart returns are expected; anything outside is a finding.
		start := time.Now()
		restartPG(t)
		h.outage(start.Add(-time.Second), time.Now().Add(10*time.Second))
	})
}

// S6: 200 ms–2 s of Postgres latency on one replica, changing every 5 s, so
// its transactions (and the row locks they hold) get slow.
func TestMM_S6_SlowPostgresOneReplica(t *testing.T) {
	runScenario(t, "S6", func(ctx context.Context, c *cluster, g *rng, h *history) {
		i := g.intn(3)
		defer c.proxy(i).set("pass", 0)
		for ctx.Err() == nil {
			c.proxy(i).set("delay", time.Duration(200+g.intn(1800))*time.Millisecond)
			select {
			case <-ctx.Done():
			case <-time.After(5 * time.Second):
			}
		}
	})
}

// samplePGLocks writes, every 500 ms, each session waiting on a lock with
// the query of the session blocking it (LOOP.md "PG sampler").
func samplePGLocks(ctx context.Context, c *cluster, path string) {
	f, err := os.Create(path)
	if err != nil {
		return
	}
	defer f.Close()
	db := c.db()
	for ctx.Err() == nil {
		rows, err := db.QueryContext(ctx, `SELECT w.pid, now()-w.query_start, left(w.query, 120), b.pid, left(b.query, 120), b.state
			FROM pg_stat_activity w JOIN LATERAL unnest(pg_blocking_pids(w.pid)) bp(pid) ON true
			JOIN pg_stat_activity b ON b.pid = bp.pid WHERE w.wait_event_type = 'Lock'`)
		if err == nil {
			ts := time.Now().UTC().Format("15:04:05.000")
			for rows.Next() {
				var wp, bpid int
				var waited, wq, bq, bstate string
				rows.Scan(&wp, &waited, &wq, &bpid, &bq, &bstate)
				fmt.Fprintf(f, "%s waiter=%d waited=%s q=%q | blocker=%d state=%s q=%q\n", ts, wp, waited, wq, bpid, bstate, bq)
			}
			rows.Close()
		}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Passive health checking for callAny (a real svc VIP needs the same).
var ejections = struct {
	sync.Mutex
	until map[*cluster]map[int]time.Time
}{until: map[*cluster]map[int]time.Time{}}

func (c *cluster) eject(i int, d time.Duration) {
	ejections.Lock()
	defer ejections.Unlock()
	if ejections.until[c] == nil {
		ejections.until[c] = map[int]time.Time{}
	}
	ejections.until[c][i] = time.Now().Add(d)
}

func (c *cluster) ejected(i int) bool {
	ejections.Lock()
	defer ejections.Unlock()
	return time.Now().Before(ejections.until[c][i])
}

// S1: rolling graceful restart (SIGTERM, then start) of every replica in
// turn, repeatedly, under steady load.
func TestMM_S1_RollingRestart(t *testing.T) {
	runScenario(t, "S1", func(ctx context.Context, c *cluster, g *rng, h *history) {
		for ctx.Err() == nil {
			for i := range 3 {
				if ctx.Err() != nil {
					return
				}
				c.stop(i, syscall.SIGTERM)
				c.start(i)
				c.waitReady(i, 15*time.Second)
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Duration(3+g.intn(5)) * time.Second):
				}
			}
		}
	})
}

// S7a: every replica SIGKILLed and restarted at the same moment, twice,
// under load: simultaneous cold starts over a populated store with pending
// work. Requests in the gap fail; the gap is declared an outage.
func TestMM_S7a_SimultaneousColdStart(t *testing.T) {
	runScenario(t, "S7a", func(ctx context.Context, c *cluster, g *rng, h *history) {
		for k := 0; k < 2 && ctx.Err() == nil; k++ {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(15+g.intn(10)) * time.Second):
			}
			start := time.Now()
			var wg sync.WaitGroup
			for i := range 3 {
				wg.Add(1)
				go func() { defer wg.Done(); c.stop(i, syscall.SIGKILL) }()
			}
			wg.Wait()
			for i := range 3 {
				c.start(i)
			}
			for i := range 3 {
				if _, err := c.waitReady(i, 15*time.Second); err != nil {
					t.Errorf("cold start %d: %v", k, err)
				}
			}
			h.outage(start.Add(-time.Second), time.Now().Add(2*time.Second))
		}
	})
}

// S8: one replica's machine clock is 20 s ahead and another's 20 s behind
// (test hook, -local-dev only). Ownership decisions use the store clock
// (F1), so every invariant must hold; /v1/status on the skewed replicas
// must report clock.ok=false (F2).
func TestMM_S8_ClockSkew(t *testing.T) {
	skew := map[int]time.Duration{1: 20 * time.Second, 2: -20 * time.Second}
	runScenarioWith(t, "S8", func(c *cluster) {
		c.env = map[int][]string{}
		for i, d := range skew {
			c.env[i] = []string{"TWD_TEST_WALL_CLOCK_OFFSET=" + d.String()}
		}
	}, func(ctx context.Context, c *cluster, g *rng, h *history) {
		time.Sleep(12 * time.Second) // a couple of reaper ticks
		for i := range 3 {
			_, b, err := c.call(i, "GET", "/v1/status", nil)
			var st struct {
				OK    bool
				Clock struct {
					OK       bool
					OffsetMs int64
				}
			}
			json.Unmarshal(b, &st)
			want := skew[i]
			t.Logf("r%d (skew %v): status ok=%v clock ok=%v offset=%dms err=%v", i, want, st.OK, st.Clock.OK, st.Clock.OffsetMs, err)
			if want == 0 && !st.Clock.OK {
				t.Errorf("r%d has no skew but reports clock not ok (offset %d ms)", i, st.Clock.OffsetMs)
			}
			if want != 0 && (st.Clock.OK || st.Clock.OffsetMs > -want.Milliseconds()+2000 || st.Clock.OffsetMs < -want.Milliseconds()-2000) {
				t.Errorf("r%d skewed %v: status must report clock not ok with offset ≈ %d ms, got ok=%v offset=%d", i, want, -want.Milliseconds(), st.Clock.OK, st.Clock.OffsetMs)
			}
		}
		<-ctx.Done()
	})
}

// S9: a hot tenant: 500 concurrent creates and spawns spread over the
// replicas mid-workload (audit-head, throttle, quota and budget contention).
func TestMM_S9_HotTenant(t *testing.T) {
	runScenario(t, "S9", func(ctx context.Context, c *cluster, g *rng, h *history) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
		tid := ""
		// The tenant is the only one: read it from any replica.
		_, b, _, _ := c.callAny(g, "", "GET", "/v1/tenants", nil)
		var tl struct{ Items []struct{ TenantID string } }
		json.Unmarshal(b, &tl)
		if len(tl.Items) > 0 {
			tid = tl.Items[0].TenantID
		}
		var roots []string
		for k := range 20 {
			_, b, _, _ := c.callAny(g, "h1", "POST", "/v1/objects", map[string]any{"tenantID": tid, "namespace": "SupportAgent", "objectName": fmt.Sprintf("hot-root-%02d", k)})
			var o struct{ ObjectID string }
			if json.Unmarshal(b, &o) == nil && o.ObjectID != "" {
				roots = append(roots, o.ObjectID)
			}
		}
		var wg sync.WaitGroup
		gate := make(chan struct{})
		for k := range 500 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				body := map[string]any{"tenantID": tid, "namespace": "SupportAgent", "objectName": fmt.Sprintf("hot-%03d", k)}
				op := "hot-create"
				if k%2 == 1 && len(roots) > 0 {
					body = map[string]any{"tenantID": tid, "namespace": "LookupAgent", "objectName": fmt.Sprintf("hot-c%03d", k),
						"parent": map[string]any{"objectID": roots[k%len(roots)]}}
					op = "hot-spawn"
				}
				t0 := time.Now()
				st, b, rep, err, tries := c.callAnyTrace(g, "h1", "POST", "/v1/objects", body)
				rec := opRec{T: t0.UnixMilli(), Op: op, Replica: rep, Status: st, Ms: time.Since(t0).Milliseconds(), Attempts: tries}
				if err != nil {
					rec.Err = err.Error()
				} else if st/100 != 2 {
					var e struct{ Code, Message string }
					json.Unmarshal(b, &e)
					rec.Code = e.Code
					if st >= 500 {
						rec.Err = e.Message
					}
				}
				h.add(rec)
			}()
		}
		close(gate)
		wg.Wait()
		<-ctx.Done()
	})
}

// S10: kill storm: every 3 s, overlapping cascade kills of the same live
// trees through every replica at once.
func TestMM_S10_KillStorm(t *testing.T) {
	runScenario(t, "S10", func(ctx context.Context, c *cluster, g *rng, h *history) {
		db := c.db()
		for ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			rows, err := db.Query(`SELECT root_id FROM objects WHERE status NOT IN ('killed','failed') AND id<>root_id
				GROUP BY root_id ORDER BY random() LIMIT 4`)
			if err != nil {
				continue
			}
			var roots []string
			for rows.Next() {
				var r string
				rows.Scan(&r)
				roots = append(roots, r)
			}
			rows.Close()
			var wg sync.WaitGroup
			for _, root := range roots {
				for rep := range 3 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						t0 := time.Now()
						st, b, err := c.callAs(rep, "", "DELETE", "/v1/objects/"+root+"?cascade=true&reason=storm", nil)
						rec := opRec{T: t0.UnixMilli(), Op: "storm-kill", Arg: root, Replica: rep, Status: st, Ms: time.Since(t0).Milliseconds()}
						if err != nil {
							rec.Err = err.Error()
						} else if st/100 != 2 {
							var e struct{ Code, Message string }
							json.Unmarshal(b, &e)
							rec.Code = e.Code
							if st >= 500 {
								rec.Err = e.Message
							}
						}
						h.add(rec)
					}()
				}
			}
			wg.Wait()
		}
	})
}

// Soak: a random mix of every fault, one every 10–25 s, for
// TWD_MM_DURATION (LOOP.md: 2 clean soaks of 10 min with new seeds).
func TestMM_Soak(t *testing.T) {
	runScenario(t, "SOAK", func(ctx context.Context, c *cluster, g *rng, h *history) {
		db := c.db()
		for ctx.Err() == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(10+g.intn(16)) * time.Second):
			}
			i := g.intn(3)
			switch g.intn(6) {
			case 0: // crash and restart
				c.stop(i, syscall.SIGKILL)
				time.Sleep(3 * time.Second)
				c.start(i)
				c.waitReady(i, 15*time.Second)
			case 1: // stall
				c.signal(i, syscall.SIGSTOP)
				time.Sleep(15 * time.Second)
				c.signal(i, syscall.SIGCONT)
			case 2: // partition from the store
				c.proxy(i).set("blackhole", 0)
				time.Sleep(10 * time.Second)
				c.proxy(i).set("pass", 0)
			case 3: // slow store link
				c.proxy(i).set("delay", time.Duration(200+g.intn(1800))*time.Millisecond)
				time.Sleep(10 * time.Second)
				c.proxy(i).set("pass", 0)
			case 4: // store restart (declared outage)
				start := time.Now()
				restartPG(t)
				h.outage(start.Add(-time.Second), time.Now().Add(10*time.Second))
			case 5: // kill storm on a few trees, through every replica
				rows, err := db.Query(`SELECT root_id FROM objects WHERE status NOT IN ('killed','failed') AND id<>root_id GROUP BY root_id ORDER BY random() LIMIT 3`)
				if err != nil {
					continue
				}
				var roots []string
				for rows.Next() {
					var r string
					rows.Scan(&r)
					roots = append(roots, r)
				}
				rows.Close()
				var wg sync.WaitGroup
				for _, root := range roots {
					for rep := range 3 {
						wg.Add(1)
						go func() {
							defer wg.Done()
							c.callAs(rep, "", "DELETE", "/v1/objects/"+root+"?cascade=true&reason=soak", nil)
						}()
					}
				}
				wg.Wait()
			}
		}
	})
}
