package hostd

// The opt-in Node.js process tier (twd.jsonc "runtime": "node", ADR-022).
//
// One supervised `node` process per deployment, with one tailnet identity:
// the workload node. There are no per-object nodes in this tier.
//
//	caller ─WG─▶ workload node (netd: WhoIs, strip x-twd-*, cf blob) ─▶ hostd ingress
//	       ─▶ nodeIngress: x-twd-caller = v1.<claims>.<HMAC(TWD_CALLER_KEY)> ─▶ app (127.0.0.1:PORT or unix socket)
//	app fetch()/http ─▶ HTTP(S)_PROXY = hostd egress proxy (127.0.0.1, Basic auth per process)
//	       ─▶ netd egress (workload node, ADR-019) ─WG─▶ destination
//
// Both secrets (the caller-signing key and the proxy credential) are fresh
// for every process start and live only in that process's environment.
//
// Enforcement: on Linux with Landlock ABI ≥ 4 the app runs under the procsb
// sandbox, which allows TCP only to its own port and the egress proxy, so
// the proxy is enforced. Elsewhere (macOS, older kernels) it is
// cooperative: a malicious app can open raw sockets (loopback, LAN,
// internet) and ignore the env proxies (see docs/node-tier.md).

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rosskukulinski/twd/hostd/bundle"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/netd"
)

// CallerHeader carries the signed verified caller to Node apps.
const CallerHeader = "x-twd-caller"

// nodeProc is the Node-tier state of an instance.
type nodeProc struct {
	bin     string
	listen  string // "tcp" | "unix"
	addr    string // 127.0.0.1:port, or the socket path
	port    int
	dataDir string
	logPath string
	proxy   net.Listener
	psrv    *http.Server

	mu       sync.Mutex
	key      []byte // HMAC key for x-twd-caller (per process start)
	token    string // egress proxy password (per process start)
	pid      int
	starts   int // total process starts
	restarts int // consecutive quick restarts (backoff)
	lastExit string
	started  time.Time     // last process start
	readyDur time.Duration // first start → health answered
}

func (np *nodeProc) secrets() ([]byte, string) {
	np.mu.Lock()
	defer np.mu.Unlock()
	return np.key, np.token
}

func randB64(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// SignCaller builds the x-twd-caller value for claims under key (tests).
func SignCaller(key []byte, claims any) string { return signCaller(key, claims) }

// signCaller builds the x-twd-caller value for claims under key.
func signCaller(key []byte, claims any) string {
	b, _ := json.Marshal(claims)
	p := base64.RawURLEncoding.EncodeToString(b)
	m := hmac.New(sha256.New, key)
	m.Write([]byte("v1." + p))
	return "v1." + p + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (h *Host) nodeBin() (string, error) {
	if h.cfg.NodeBin != "" {
		return h.cfg.NodeBin, nil
	}
	p, err := exec.LookPath("node")
	if err != nil {
		return "", errors.New("runtime \"node\": no node binary on PATH (set twd-hostd -node)")
	}
	return p, nil
}

func freeLoopbackPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

// startNodeInstance writes the bundle, starts the egress proxy and the node
// process, and waits for the app to answer its health path.
func (h *Host) startNodeInstance(w *workload, depID string, app *bundle.App, n int) (*instance, error) {
	bin, err := h.nodeBin()
	if err != nil {
		return nil, err
	}
	dir := h.instanceDir(n)
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o700); err != nil {
		return nil, err
	}
	for p, b := range app.Node.Files {
		fp := filepath.Join(dir, filepath.FromSlash(p))
		if !strings.HasPrefix(fp, dir+string(filepath.Separator)) {
			return nil, fmt.Errorf("bundle: unsafe path %q", p)
		}
		if err := os.MkdirAll(filepath.Dir(fp), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(fp, b, 0o600); err != nil {
			return nil, err
		}
	}
	np := &nodeProc{bin: bin, listen: app.Node.Listen, logPath: filepath.Join(dir, "app.log"),
		dataDir: filepath.Join(h.tenantDir(w.tenantID), "workloads", w.name, "node")}
	if err := os.MkdirAll(np.dataDir, 0o700); err != nil {
		return nil, err
	}
	if np.listen == "unix" {
		np.addr = filepath.Join(dir, "app.sock")
	} else {
		if np.port, err = freeLoopbackPort(); err != nil {
			return nil, err
		}
		np.addr = "127.0.0.1:" + strconv.Itoa(np.port)
	}
	inst := &instance{h: h, w: w, depID: depID, dir: dir, app: app, tier: bundle.TierProcess, node: np}
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		if np.listen == "unix" {
			return (&net.Dialer{}).DialContext(ctx, "unix", np.addr)
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", np.addr)
	}
	tr := &http.Transport{DialContext: dial, MaxIdleConnsPerHost: 64, IdleConnTimeout: 60 * time.Second}
	inst.direct = &http.Client{Timeout: 10 * time.Second, Transport: tr}
	inst.ingress = h.nodeIngress(inst, tr)
	if np.proxy, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		return nil, err
	}
	np.psrv = &http.Server{Handler: h.nodeEgress(inst), ReadHeaderTimeout: 30 * time.Second,
		ErrorLog: log.New(io.Discard, "", 0)}
	go np.psrv.Serve(np.proxy)
	if err := inst.spawn(); err != nil {
		inst.stop()
		return nil, err
	}
	if err := inst.waitNodeHealthy(30 * time.Second); err != nil {
		inst.stop()
		return nil, err
	}
	return inst, nil
}

// spawnNode starts (or restarts) the node process with fresh secrets.
func (inst *instance) spawnNode() error {
	np := inst.node
	logf, err := os.OpenFile(np.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	key := make([]byte, 32)
	rand.Read(key)
	token := randB64(24)
	proxy := (&url.URL{Scheme: "http", User: url.UserPassword("twd", token), Host: np.proxy.Addr().String()}).String()
	env := map[string]string{
		"PATH":     os.Getenv("PATH"),
		"HOME":     np.dataDir,
		"TMPDIR":   filepath.Join(inst.dir, "tmp"),
		"NODE_ENV": "production",
		// fetch()/http honour HTTP(S)_PROXY with this (Node ≥ 24).
		"NODE_USE_ENV_PROXY": "1",
		"HTTP_PROXY":         proxy, "HTTPS_PROXY": proxy, "http_proxy": proxy, "https_proxy": proxy,
		"TWD_EGRESS_PROXY": proxy,
		"TWD_CALLER_KEY":   base64.RawURLEncoding.EncodeToString(key),
		"TWD_DATA_DIR":     np.dataDir,
		"TWD_WORKLOAD":     inst.w.name,
		"TWD_DEPLOYMENT":   inst.depID,
	}
	if np.listen == "unix" {
		env["TWD_SOCKET"] = np.addr
		env["NO_PROXY"] = np.proxy.Addr().String()
	} else {
		p := strconv.Itoa(np.port)
		env["PORT"], env["HOST"] = p, "127.0.0.1"
		// Only the app's own port bypasses the proxy (self-calls such as the
		// Workflow local world's queue); everything else is attributed.
		env["NO_PROXY"] = "127.0.0.1:" + p + ",localhost:" + p + "," + np.proxy.Addr().String()
		env["no_proxy"] = env["NO_PROXY"]
	}
	if wl := inst.w.currentWL(); wl != nil {
		env["TWD_NODE_NAME"] = wl.MagicDNS
	}
	for k, v := range inst.app.Node.Env {
		if _, reserved := env[k]; !reserved {
			env[k] = v
		}
	}
	args := append(append([]string{}, inst.app.Node.Args...), inst.app.Node.Main)
	cmd, err := inst.nodeCommand(args)
	if err != nil {
		logf.Close()
		return err
	}
	cmd.Dir = inst.dir
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if np.listen == "unix" {
		os.Remove(np.addr)
	}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return err
	}
	ex := make(chan struct{})
	np.mu.Lock()
	np.key, np.token, np.pid, np.started = key, token, cmd.Process.Pid, time.Now()
	np.starts++
	np.mu.Unlock()
	inst.mu.Lock()
	inst.cmd, inst.exited, inst.started = cmd, ex, time.Now()
	inst.mu.Unlock()
	inst.h.log.Info("node process started", "workload", inst.w.name, "pid", cmd.Process.Pid)
	go func() {
		err := cmd.Wait()
		logf.Close()
		np.mu.Lock()
		np.pid = 0
		if err != nil {
			np.lastExit = err.Error()
		} else {
			np.lastExit = "exit 0"
		}
		np.mu.Unlock()
		// Reap anything the app left in its process group.
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		close(ex)
		inst.onNodeExit()
	}()
	return nil
}

// onNodeExit restarts the app with exponential backoff (250 ms doubling to
// 10 s; reset after 30 s up). It never gives up: the workload stays active
// and requests get 503 upstream_unavailable while the app is down.
func (inst *instance) onNodeExit() {
	if inst.stopping.Load() || inst.h.ctx.Err() != nil {
		return
	}
	np := inst.node
	inst.mu.Lock()
	up := time.Since(inst.started)
	inst.mu.Unlock()
	np.mu.Lock()
	if up > 30*time.Second {
		np.restarts = 0
	}
	np.restarts++
	n := np.restarts
	why := np.lastExit
	np.mu.Unlock()
	d := 250 * time.Millisecond << min(n-1, 6)
	if d > 10*time.Second {
		d = 10 * time.Second
	}
	inst.h.log.Warn("node process exited; restarting", "workload", inst.w.name, "exit", why, "attempt", n, "backoff", d, "log", inst.nodeLogTail(1024))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-inst.h.ctx.Done():
		return
	}
	if inst.stopping.Load() {
		return
	}
	if err := inst.spawnNode(); err != nil {
		inst.h.log.Error("node restart failed", "workload", inst.w.name, "err", err)
		go inst.onNodeExit()
	}
}

func (inst *instance) nodeLogTail(n int) string {
	b, _ := os.ReadFile(inst.node.logPath)
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return strings.TrimSpace(string(b))
}

func (inst *instance) waitNodeHealthy(d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		req, _ := http.NewRequest("GET", "http://app"+inst.app.Node.Health, nil)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		resp, err := inst.direct.Do(req.WithContext(ctx))
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			cancel()
			inst.node.mu.Lock()
			inst.node.readyDur = time.Since(inst.node.started)
			inst.node.mu.Unlock()
			return nil
		}
		cancel()
		if time.Now().After(deadline) {
			return fmt.Errorf("node app did not answer %s: %s", inst.app.Node.Health, inst.nodeLogTail(4096))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// nodeIngress forwards a tailnet request (netd has done WhoIs and set the
// cf blob; hostd added routing) to the app with a signed x-twd-caller.
func (h *Host) nodeIngress(inst *instance, tr http.RoundTripper) http.Handler {
	rp := &httputil.ReverseProxy{
		Transport:     tr,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&url.URL{Scheme: "http", Host: "app"})
			pr.Out.Host = pr.In.Host
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return
			}
			netd.WriteError(w, &netd.Error{Code: netd.CodeUpstreamUnavailable, Detail: "node app: " + err.Error()})
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var blob struct {
			Tailscale struct {
				Caller json.RawMessage `json:"caller"`
			} `json:"tailscale"`
			ObjectID string `json:"objectId"`
			TraceID  string `json:"traceId"`
			Twd      struct {
				ObjectID string `json:"objectID"`
			} `json:"twd"`
		}
		if err := json.Unmarshal([]byte(r.Header.Get(netd.HeaderCF)), &blob); err != nil || len(blob.Tailscale.Caller) == 0 {
			writeAPIError(w, &apiError{Code: "no_identity", Detail: "missing cf blob"})
			return
		}
		key, _ := inst.node.secrets()
		if key == nil {
			netd.WriteError(w, &netd.Error{Code: netd.CodeUpstreamUnavailable, Detail: "node app is not running"})
			return
		}
		// Nothing identity-shaped from the client survives (netd already
		// stripped x-twd-*; this is defence in depth), and the platform's own
		// headers are replaced by the one signed header.
		for k := range r.Header {
			if lk := strings.ToLower(k); strings.HasPrefix(lk, "x-twd") || strings.HasPrefix(lk, "twd-") {
				r.Header.Del(k)
			}
		}
		// The caller's capMap was already narrowed by netd (RT-01); the
		// sender's certificate summary rides inside `caller.twd` and its lim
		// is repeated at the top level. The nonce lets @twd/node refuse a
		// replayed header (RT-05).
		claims := map[string]any{
			"v": 1, "caller": blob.Tailscale.Caller, "objectId": blob.Twd.ObjectID, "workload": inst.w.name,
			"traceId": blob.TraceID, "iat": time.Now().UnixMilli(), "nonce": randB64(16),
		}
		var sender struct {
			Twd *struct {
				ObjectID string          `json:"objectId"`
				Lim      json.RawMessage `json:"lim"`
			} `json:"twd"`
		}
		if json.Unmarshal(blob.Tailscale.Caller, &sender) == nil && sender.Twd != nil {
			claims["callerObjectId"] = sender.Twd.ObjectID
			if len(sender.Twd.Lim) > 0 {
				claims["lim"] = sender.Twd.Lim
			}
		}
		r.Header.Set(CallerHeader, signCaller(key, claims))
		rp.ServeHTTP(w, r)
	})
}

// nodeEgress is the app's HTTP(S) proxy: it authenticates the process
// (Basic auth with the per-start token), then hands the request to netd,
// which dials from the workload node under ADR-019. CONNECT is tunnelled
// (env proxies use it even for http:// targets).
func (h *Host) nodeEgress(inst *instance) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, token := inst.node.secrets()
		user, pass, ok := parseProxyAuth(r.Header.Get("Proxy-Authorization"))
		if !ok || token == "" || user != "twd" || subtle.ConstantTimeCompare([]byte(pass), []byte(token)) != 1 {
			w.Header().Set("Proxy-Authenticate", `Basic realm="twd egress"`)
			w.Header().Set(netd.HeaderError, "no_identity")
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(http.StatusProxyAuthRequired)
			json.NewEncoder(w).Encode(map[string]any{"code": "no_identity", "message": "egress proxy credentials missing or stale", "doc_url": netd.DocBase + "no_identity"})
			return
		}
		r.Header.Del("Proxy-Authorization")
		wl := inst.w.currentWL()
		if wl == nil {
			writeAPIError(w, &apiError{Code: "identity_pending", Detail: "workload node not created yet"})
			return
		}
		if r.Method == http.MethodConnect {
			h.nodeConnect(w, r, inst, wl)
			return
		}
		h.serveEgress(w, r, inst, wl)
	})
}

func parseProxyAuth(v string) (user, pass string, ok bool) {
	const prefix = "Basic "
	if len(v) < len(prefix) || !strings.EqualFold(v[:len(prefix)], prefix) {
		return "", "", false
	}
	b, err := base64.StdEncoding.DecodeString(v[len(prefix):])
	if err != nil {
		return "", "", false
	}
	user, pass, ok = strings.Cut(string(b), ":")
	return
}

func (h *Host) nodeConnect(w http.ResponseWriter, r *http.Request, inst *instance, wl *object) {
	t := inst.w.tenant
	dst := r.Host
	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.ReadyTimeout)
	up, err := t.reg.DialEgress(ctx, wl.NetdID, dst)
	cancel()
	if err != nil {
		e, ok := netd.AsError(err)
		if !ok {
			e = &netd.Error{Code: netd.CodeDialFailed, Detail: err.Error(), Destination: dst}
		}
		h.audit.add(mgmtapi.AuditRecord{ObjectID: wl.ID, Kind: "deny", Dst: dst, Code: e.Code, TraceID: newTraceID()})
		netd.WriteError(w, e)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		up.Close()
		netd.WriteError(w, &netd.Error{Code: netd.CodeBadRequest, Detail: "CONNECT needs HTTP/1.1"})
		return
	}
	c, brw, err := hj.Hijack()
	if err != nil {
		up.Close()
		return
	}
	brw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
	if err := brw.Flush(); err != nil {
		c.Close()
		up.Close()
		return
	}
	go splice(c, brw.Reader, up)
}

func splice(c net.Conn, br *bufio.Reader, up net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(up, br) // br drains c's buffered bytes, then reads c
		if cw, ok := up.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			up.Close()
		}
		done <- struct{}{}
	}()
	go func() {
		io.Copy(c, up)
		if cw, ok := c.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			c.Close()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
	c.Close()
	up.Close()
}

// stopNode terminates the process group (SIGTERM, then SIGKILL after 3 s)
// and the egress proxy.
func (inst *instance) stopNode() {
	inst.mu.Lock()
	cmd, ex := inst.cmd, inst.exited
	inst.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-ex:
		case <-time.After(3 * time.Second):
			syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-ex
		}
	}
	if np := inst.node; np.psrv != nil {
		np.psrv.Close()
	}
}

// NodeInfo is the Node-tier view of a running deployment.
type NodeInfo struct {
	PID       int
	Starts    int
	Addr      string // the app's listen address (127.0.0.1:port or socket)
	Listen    string // "tcp" | "unix"
	ProxyAddr string // the egress proxy (credentials are not exposed)
	StartedAt time.Time
	// ReadyMs is the first start's process spawn → health answered.
	ReadyMs  float64
	LogFile  string
	DataDir  string
	LastExit string `json:",omitempty"`
}

func (inst *instance) nodeInfo() *NodeInfo {
	np := inst.node
	if np == nil {
		return nil
	}
	np.mu.Lock()
	defer np.mu.Unlock()
	listen := np.listen
	if listen == "" {
		listen = "tcp"
	}
	return &NodeInfo{PID: np.pid, Starts: np.starts, Addr: np.addr, Listen: listen, ProxyAddr: np.proxy.Addr().String(), StartedAt: np.started,
		ReadyMs: float64(np.readyDur.Microseconds()) / 1000, LogFile: np.logPath, DataDir: np.dataDir, LastExit: np.lastExit}
}
