//go:build e2emultimgmt

// Package multimgmt is the hermetic multi-replica twd-mgmt harness driven by
// e2e/multimgmt/LOOP.md: N real twd-mgmt processes sharing one Postgres,
// a fake Tailscale API and a fake setec. Nothing here touches a real tailnet.
package multimgmt

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

const (
	pgContainer = "twd-mm-pg"
	pgPassword  = "twdmm"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ---- run directory and process records ----

type run struct {
	t    *testing.T
	dir  string
	pids *os.File
	mu   sync.Mutex
}

func newRun(t *testing.T, item string) *run {
	t.Helper()
	root, _ := filepath.Abs("results")
	dir := filepath.Join(root, time.Now().UTC().Format("20060102T150405Z")+"-"+item)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "pids"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	t.Logf("artifacts: %s", dir)
	return &run{t: t, dir: dir, pids: f}
}

func (r *run) record(kind, name string, pid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fmt.Fprintf(r.pids, "%s %s %d\n", kind, name, pid)
}

// ---- Postgres (OrbStack docker) ----

// startPG starts a fresh twd-mm-pg container (or uses TWD_MM_PG_DSN) and
// returns the DSN. The container is removed when the test ends.
func startPG(t *testing.T, r *run) string {
	t.Helper()
	if dsn := os.Getenv("TWD_MM_PG_DSN"); dsn != "" {
		return dsn
	}
	ctx, port := env("PG_CTX", "orbstack"), env("TWD_MM_PG_PORT", "55434")
	docker := func(args ...string) ([]byte, error) {
		return exec.Command("docker", append([]string{"--context", ctx}, args...)...).CombinedOutput()
	}
	docker("rm", "-f", pgContainer)
	out, err := docker("run", "-d", "--rm", "--name", pgContainer, "-e", "POSTGRES_PASSWORD="+pgPassword,
		"-p", "127.0.0.1:"+port+":5432", "postgres:17-alpine",
		"-c", "log_lock_waits=on", "-c", "deadlock_timeout=200ms", "-c", "track_activity_query_size=8192",
		"-c", "max_connections=300")
	if err != nil {
		t.Fatalf("docker run postgres: %v: %s", err, out)
	}
	r.record("container", pgContainer, 0)
	t.Cleanup(func() {
		if logs, err := docker("logs", pgContainer); err == nil {
			os.WriteFile(filepath.Join(r.dir, "postgres.log"), logs, 0o600)
		}
		docker("rm", "-f", pgContainer)
	})
	dsn := "postgres://postgres:" + pgPassword + "@127.0.0.1:" + port + "/postgres?sslmode=disable"
	deadline := time.Now().Add(60 * time.Second)
	for {
		db, err := sql.Open("pgx", dsn)
		if err == nil {
			err = db.Ping()
			db.Close()
		}
		if err == nil {
			return dsn
		}
		if time.Now().After(deadline) {
			t.Fatalf("postgres not ready: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ---- fake Tailscale API ----

// fakeTS is the subset of the Tailscale API that mgmt's mint broker and
// device deletes use. It counts keys per object ("twd <objectID>").
type fakeTS struct {
	srv *httptest.Server
	mu  sync.Mutex
	n   int
	// keysByObject: object ID → key IDs ever created for it.
	keysByObject map[string][]string
	liveKeys     map[string]string // key ID → object ID
	createTimes  []time.Time
	deletedDevs  []string
}

const fakeAPIToken = "fake-api-token"

func newFakeTS(t *testing.T) *fakeTS {
	f := &fakeTS{keysByObject: map[string][]string{}, liveKeys: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeTS) serve(w http.ResponseWriter, r *http.Request) {
	if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != fakeAPIToken {
		w.WriteHeader(401)
		w.Write([]byte(`{"message":"invalid token"}`))
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/keys"):
		var body struct {
			Description string `json:"description"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.n++
		id := fmt.Sprintf("k%06d", f.n)
		obj := strings.TrimPrefix(body.Description, "twd ")
		f.keysByObject[obj] = append(f.keysByObject[obj], id)
		f.liveKeys[id] = obj
		f.createTimes = append(f.createTimes, time.Now())
		json.NewEncoder(w).Encode(map[string]any{"id": id, "key": "fake-authkey-" + id})
	case r.Method == "DELETE" && strings.Contains(r.URL.Path, "/keys/"):
		delete(f.liveKeys, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		w.WriteHeader(200)
	case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/api/v2/device/"):
		f.deletedDevs = append(f.deletedDevs, strings.TrimPrefix(r.URL.Path, "/api/v2/device/"))
		w.WriteHeader(200)
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/devices"):
		json.NewEncoder(w).Encode(map[string]any{"devices": []any{}})
	default:
		w.WriteHeader(404)
		fmt.Fprintf(w, `{"message":"fakeTS: %s %s not implemented"}`, r.Method, r.URL.Path)
	}
}

// ---- fake setec (shared custody) ----

func newFakeSetec(t *testing.T) *httptest.Server {
	var mu sync.Mutex
	vals := map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-X-Tailscale-No-Browsers") != "setec" {
			w.WriteHeader(403)
			return
		}
		var req struct {
			Name  string
			Value []byte
		}
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/put":
			vals[req.Name] = req.Value
			json.NewEncoder(w).Encode(1)
		case "/api/activate":
		case "/api/get":
			v, ok := vals[req.Name]
			if !ok {
				http.Error(w, "not found", 404)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"Value": v, "Version": 1})
		case "/api/delete":
			delete(vals, req.Name)
		default:
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ---- twd-mgmt replicas ----

var buildOnce struct {
	sync.Once
	bin string
	err error
}

func mgmtBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "twd-mm-bin-")
		if err != nil {
			buildOnce.err = err
			return
		}
		buildOnce.bin = filepath.Join(dir, "twd-mgmt")
		out, err := exec.Command("go", "build", "-o", buildOnce.bin, "../../cmd/twd-mgmt").CombinedOutput()
		if err != nil {
			buildOnce.err = fmt.Errorf("%v: %s", err, out)
		}
	})
	if buildOnce.err != nil {
		t.Fatal(buildOnce.err)
	}
	return buildOnce.bin
}

type cluster struct {
	t         *testing.T
	r         *run
	dsn       string
	ts        *fakeTS
	setec     *httptest.Server
	tokenFile string
	token     string
	replicas  []*replica
	extraArgs []string
}

type replica struct {
	i      int
	dir    string
	sock   string
	cmd    *exec.Cmd
	log    *os.File
	exited chan struct{}
}

// newCluster prepares n replicas (not started). Every replica shares the DSN,
// the fake TS API, the fake setec and one host token (pre-created, so
// concurrent starts don't race on creating it).
func newCluster(t *testing.T, r *run, n int) *cluster {
	t.Helper()
	mgmtBinary(t) // build first, so "start all at once" really is simultaneous
	c := &cluster{t: t, r: r, dsn: startPG(t, r), ts: newFakeTS(t), setec: newFakeSetec(t)}
	c.tokenFile = filepath.Join(r.dir, "host.token")
	c.token = strings.Repeat("ab", 32)
	if err := os.WriteFile(c.tokenFile, []byte(c.token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		// unix socket paths are limited to ~104 bytes on macOS: keep them short.
		sock := filepath.Join(os.TempDir(), fmt.Sprintf("twd-mm-%d-%d.sock", os.Getpid(), i))
		c.replicas = append(c.replicas, &replica{i: i, dir: filepath.Join(r.dir, fmt.Sprintf("r%d", i)), sock: sock})
	}
	t.Cleanup(func() {
		for _, rp := range c.replicas {
			c.stop(rp.i, syscall.SIGKILL)
		}
	})
	return c
}

func (c *cluster) start(i int) {
	c.t.Helper()
	rp := c.replicas[i]
	if rp.cmd != nil {
		c.t.Fatalf("replica %d already running", i)
	}
	os.MkdirAll(rp.dir, 0o700)
	lf, err := os.OpenFile(filepath.Join(c.r.dir, fmt.Sprintf("r%d.log", i)), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		c.t.Fatal(err)
	}
	fmt.Fprintf(lf, "=== start %s\n", time.Now().UTC().Format(time.RFC3339Nano))
	args := append([]string{"-local-dev", "-socket", rp.sock, "-host-token-file", c.tokenFile, "-state-dir", rp.dir,
		"-db", c.dsn, "-custody", "setec", "-setec-url", c.setec.URL, "-tsapi-url", c.ts.srv.URL, "-reap-interval", "2s",
		"-no-loopback"}, c.extraArgs...)
	cmd := exec.Command(mgmtBinary(c.t), args...)
	cmd.Stdout, cmd.Stderr = lf, lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		c.t.Fatal(err)
	}
	c.r.record("twd-mgmt", fmt.Sprintf("r%d", i), cmd.Process.Pid)
	rp.cmd, rp.log, rp.exited = cmd, lf, make(chan struct{})
	go func(cmd *exec.Cmd, ch chan struct{}) { cmd.Wait(); close(ch) }(cmd, rp.exited)
}

// stop signals replica i (only the process this harness started) and waits
// up to 10 s for it to exit (SIGKILL after that).
func (c *cluster) stop(i int, sig syscall.Signal) {
	rp := c.replicas[i]
	if rp.cmd == nil {
		return
	}
	rp.cmd.Process.Signal(sig)
	select {
	case <-rp.exited:
	case <-time.After(10 * time.Second):
		rp.cmd.Process.Kill()
		<-rp.exited
	}
	fmt.Fprintf(rp.log, "=== stopped (%v) %s\n", sig, time.Now().UTC().Format(time.RFC3339Nano))
	rp.log.Close()
	rp.cmd = nil
	os.Remove(rp.sock)
}

func (c *cluster) running(i int) bool {
	rp := c.replicas[i]
	if rp.cmd == nil {
		return false
	}
	select {
	case <-rp.exited:
		return false
	default:
		return true
	}
}

// client returns an HTTP client for replica i's unix socket.
func (c *cluster) client(i int) *http.Client {
	sock := c.replicas[i].sock
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
}

// call does one API request on replica i and returns status and body.
func (c *cluster) call(i int, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, "http://twd-mgmt"+path, rd)
	req.Header.Set("Twd-Host-Token", c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.client(i).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

// waitReady waits until replica i answers GET /v1/status with 200, or the
// timeout passes. It returns how long that took.
func (c *cluster) waitReady(i int, timeout time.Duration) (time.Duration, error) {
	start := time.Now()
	var last string
	for time.Since(start) < timeout {
		if !c.running(i) {
			return time.Since(start), fmt.Errorf("replica %d exited (see r%d.log)", i, i)
		}
		st, b, err := c.call(i, "GET", "/v1/status", nil)
		if err == nil && st == 200 {
			return time.Since(start), nil
		}
		last = fmt.Sprintf("status=%d err=%v body=%.200s", st, err, b)
		time.Sleep(100 * time.Millisecond)
	}
	return time.Since(start), fmt.Errorf("replica %d not ready after %v: %s", i, timeout, last)
}

// db opens a direct connection to the cluster's Postgres (for the oracle).
func (c *cluster) db() *sql.DB {
	db, err := sql.Open("pgx", c.dsn)
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(func() { db.Close() })
	return db
}

// advisoryHolders returns the PIDs and application names of sessions that
// hold the migration advisory lock 7464.
func advisoryHolders(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT l.pid, COALESCE(a.state,''), COALESCE(a.application_name,''), l.granted
		FROM pg_locks l LEFT JOIN pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND l.objid = 7464`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var pid int
		var state, app string
		var granted bool
		rows.Scan(&pid, &state, &app, &granted)
		out = append(out, fmt.Sprintf("pid=%d state=%s app=%q granted=%v", pid, state, app, granted))
	}
	return out, rows.Err()
}
