package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/internal/policy"
	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/mgmt"
	"golang.org/x/term"
)

const prodControl = "https://controlplane.tailscale.com"

// baseSnippet is what `twd up` needs in the tenant policy: tag:twd owns
// twd's tags (DX.md quickstart).
func baseSnippet() policy.Snippet {
	return policy.Snippet{TagOwners: map[string][]string{"tag:twd": {"autogroup:admin"}}}
}

func tenantIDFor(tailnet string) string {
	l, _, _ := strings.Cut(tailnet, ".")
	l = regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(strings.ToLower(l), "-")
	return "tn_" + strings.Trim(l, "-")
}

func defaultHostID() string {
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(strings.ToLower(h), ".")
	h = strings.Trim(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(h, "-"), "-")
	if h == "" {
		h = "local"
	}
	return "h-" + h
}

type upResult struct {
	StateDir   string             `json:"stateDir"`
	Tailnet    string             `json:"tailnet"`
	TenantID   string             `json:"tenantID"`
	HostID     string             `json:"hostID"`
	Started    bool               `json:"started"`
	PolicyOK   bool               `json:"policyOK"`
	PolicyDiff string             `json:"policySnippet,omitempty"`
	Applied    bool               `json:"policyApplied"`
	Timings    map[string]float64 `json:"timingsMs"`
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

func cmdUp(args []string) error {
	f := newFlags("up")
	tailnet := f.String("tailnet", "", "the tailnet to serve (required: twd never uses this machine's tailscaled)")
	keyFile := f.String("api-key-file", "", "file with TS_API_KEY=… (test/homelab mint mode \"apikey\"; the key is read at use, never copied)")
	yes := f.Bool("yes", false, "non-interactive: don't ask for confirmation")
	apply := f.Bool("policy-apply", false, "apply the policy snippet (validated first, written with If-Match); default: print it")
	control := f.String("control-url", prodControl, "coordination server for the tailnet's nodes")
	tsapiURL := f.String("tsapi-url", "", "Tailscale API base URL (testing)")
	hostID := f.String("host-id", defaultHostID(), "this host's ID")
	prefix := f.String("hostname-prefix", "twd-", "prefix for every node hostname")
	ephemeral := f.Bool("ephemeral", false, "join nodes as ephemeral; `twd down` then deletes every device (tests, demos)")
	workerdFlag := f.String("workerd", "", "workerd binary")
	hb := f.Duration("heartbeat", time.Second, "host heartbeat period")
	tomb := f.Duration("kill-tombstone", 12*time.Second, "keep a killed node answering object_killed this long")
	delDelay := f.Duration("device-delete-delay", 8*time.Second, "delete a killed object's device this long after the kill (< --kill-tombstone)")
	pos, err := f.parse(args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return fail("bad_request", "twd up takes no arguments", "usage: twd "+usageOf("up"))
	}
	t0 := time.Now()
	timings := map[string]float64{}
	if *tailnet == "" {
		return fail("tailnet_required", "twd up needs --tailnet",
			"interactive setup (bootstrapping from this machine's tailscaled login) is not supported yet,",
			"and twd never uses this machine's tailscaled or its identity.").
			withFix("twd up --tailnet <tailnet>.ts.net --api-key-file <file with TS_API_KEY=…> --yes")
	}
	if *keyFile == "" {
		return fail("bad_request", "twd up --tailnet needs --api-key-file (mint mode \"apikey\")",
			"WIF trust credentials (no stored secret) need `twd connect`, which is not built yet").
			withFix("twd up --tailnet " + *tailnet + " --api-key-file <file with TS_API_KEY=…> --yes")
	}
	if *delDelay >= *tomb {
		return fail("bad_request", "--device-delete-delay must be shorter than --kill-tombstone",
			"the tombstone answers object_killed only while the device still exists in peers' netmaps")
	}
	absKey, _ := filepath.Abs(*keyFile)
	apiKey, err := tsapi.LoadEnvFile(absKey, "TS_API_KEY")
	if err != nil {
		return fail("bad_request", "can't read TS_API_KEY from "+absKey, err.Error())
	}
	if st, err := os.Stat(absKey); err == nil && st.Mode().Perm()&0o077 != 0 {
		fmt.Fprintf(os.Stderr, "warning: %s is readable by other users (mode %v); chmod 600 it\n", absKey, st.Mode().Perm())
	}
	s, err := openState(*f.stateDir)
	if err != nil {
		return err
	}
	if s.configured() && s.Cfg.Tailnet != *tailnet {
		return fail("state_conflict", s.Dir+" is bound to tailnet "+s.Cfg.Tailnet).
			withFix("twd down --purge, then remove "+s.Dir, "or use another --state-dir")
	}
	if !*yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return fail("bad_request", "not a terminal: pass --yes to run non-interactively")
		}
		fmt.Printf("Start twd on this machine for tailnet %s (state in %s)? [y/N] ", *tailnet, s.Dir)
		ln, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(ln)); a != "y" && a != "yes" {
			return fail("cancelled", "not started")
		}
	}
	// The API key must work for this tailnet; read the policy (never written
	// unless --policy-apply).
	api := &tsapi.Client{Tailnet: *tailnet, APIKey: apiKey, BaseURL: *tsapiURL}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ps := time.Now()
	pol, _, err := api.GetPolicyHuJSON(ctx)
	if err != nil {
		return fail("bootstrap_failed", "the Tailscale API rejected the key for "+*tailnet,
			"step:  GET /api/v2/tailnet/"+*tailnet+"/acl → "+err.Error()).
			withFix("check the tailnet name and that the key in " + absKey + " is valid and can read the policy")
	}
	timings["api_check"] = ms(time.Since(ps))

	wd, err := findWorkerd(*workerdFlag)
	if err != nil {
		return err
	}
	for _, b := range []string{"twd-mgmt", "twd-hostd"} {
		if _, err := findBin(b); err != nil {
			return err
		}
	}
	if !s.configured() {
		s.Cfg = Config{Tailnet: *tailnet, CreatedAt: time.Now().UTC().Format(time.RFC3339)}
	}
	s.Cfg.APIKeyFile, s.Cfg.ControlURL, s.Cfg.TSAPIURL, s.Cfg.HostID = absKey, *control, *tsapiURL, *hostID
	s.Cfg.HostnamePrefix, s.Cfg.Ephemeral, s.Cfg.Workerd = *prefix, *ephemeral, wd
	s.Cfg.Heartbeat, s.Cfg.KillTombstone, s.Cfg.DeviceDeleteDelay = hb.String(), tomb.String(), delDelay.String()
	if err := s.save(); err != nil {
		return err
	}
	res := upResult{StateDir: s.Dir, Tailnet: *tailnet, HostID: *hostID, Timings: timings}
	if !s.supervisorAlive() {
		if err := startSupervisor(s); err != nil {
			return err
		}
		res.Started = true
	}
	ms0 := time.Now()
	for !s.mgmtHealthy(ctx) {
		if time.Since(ms0) > 30*time.Second || !s.supervisorAlive() {
			return fail("mgmt_unavailable", "twd-mgmt did not become healthy", "log: "+s.log("mgmt")).withFix("twd logs --component mgmt")
		}
		time.Sleep(50 * time.Millisecond)
	}
	timings["mgmt_ready"] = ms(time.Since(t0))

	// Tenant (operator = tenant = you).
	tid := s.Cfg.TenantID
	if tid != "" {
		if err := s.mgmt().Do(ctx, "GET", "/v0/tenants/"+tid, nil, nil); err != nil {
			tid = ""
		}
	}
	if tid == "" {
		var tr struct {
			TenantID string `json:"tenantID"`
		}
		err := s.mgmt().Do(ctx, "POST", "/v0/tenants", map[string]any{"tenantID": tenantIDFor(*tailnet), "tailnet": *tailnet,
			"controlURL": *control, "mintMode": "apikey", "apiKeyFile": absKey}, &tr)
		if err != nil {
			return err
		}
		tid = tr.TenantID
		s.Cfg.TenantID = tid
		if err := s.save(); err != nil {
			return err
		}
	}
	res.TenantID = tid
	timings["tenant_ready"] = ms(time.Since(t0))

	// Host registered and heartbeating.
	hs := time.Now()
	for {
		var hosts []struct {
			ID       string `json:"hostID"`
			LastSeen int64  `json:"lastSeen"`
		}
		if s.mgmt().Do(ctx, "GET", "/v0/hosts", nil, &hosts) == nil {
			live := false
			for _, h := range hosts {
				if h.ID == *hostID && time.Since(time.UnixMilli(h.LastSeen)) < 10*time.Second {
					live = true
				}
			}
			if live && s.hostd().Do(ctx, "GET", "/v0/status", nil, nil) == nil {
				break
			}
		}
		if time.Since(hs) > 60*time.Second || !s.supervisorAlive() {
			return fail("host_unavailable", "twd-hostd did not register with twd-mgmt", "log: "+s.log("hostd")).withFix("twd logs --component hostd")
		}
		time.Sleep(50 * time.Millisecond)
	}
	timings["host_ready"] = ms(time.Since(t0))

	miss, err := policy.Missing(pol, baseSnippet())
	if err != nil {
		return err
	}
	res.PolicyOK = miss.Empty()
	if !res.PolicyOK {
		res.PolicyDiff = miss.HuJSON()
		if *apply {
			ar, err := policy.Apply(ctx, api, baseSnippet())
			if err != nil {
				return fail("policy_apply_failed", "could not apply the policy snippet", err.Error()).withFix("add it in the admin console:", res.PolicyDiff)
			}
			res.Applied, res.PolicyOK = ar.Changed, true
		}
	}
	timings["total"] = ms(time.Since(t0))
	emit(f, res, func() {
		fmt.Printf("twd up · all-in-one on this machine · tailnet: %s (API key, mint mode apikey)\n", *tailnet)
		started := "already running"
		if res.Started {
			started = "started"
		}
		fmt.Printf("✓ mgmt     twd-mgmt %s · leases in SQLite (%s) · custody: file (homelab mode)\n", started, filepath.Join(s.Dir, "mgmt", "mgmt.db"))
		fmt.Printf("✓ host     this machine (%s) · tier: process [macOS: process tier, dev-grade isolation]\n", *hostID)
		fmt.Printf("✓ tenant   %s connected (tenant %s) · control %s\n", *tailnet, tid, *control)
		switch {
		case res.Applied:
			fmt.Printf("✓ policy   applied tag:twd tagOwners (validated, If-Match)\n")
		case res.PolicyOK:
			fmt.Printf("✓ policy   tag:twd tagOwners present\n")
		default:
			fmt.Printf("› policy   not applied (re-run with --policy-apply, or add in the admin console):\n")
			for _, l := range strings.Split(strings.TrimSpace(res.PolicyDiff), "\n") {
				fmt.Printf("    %s\n", l)
			}
		}
		fmt.Printf("ready in %.1fs · `twd init hello --template agent && cd hello && twd deploy`\n", timings["total"]/1000)
	})
	return nil
}

// startSupervisor starts `twd __supervise` detached (its own session), with
// output in <state>/logs/supervisor.log.
func startSupervisor(s *state) error {
	if err := os.MkdirAll(s.logDir(), 0o700); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	lf, err := os.OpenFile(s.log("supervisor"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	cmd := exec.Command(exe, "__supervise", "--state-dir", s.Dir)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = lf, lf, nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	cmd.Process.Release()
	for i := 0; i < 100; i++ {
		if s.supervisorPID() == pid {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !pidAlive(pid) {
		return fail("supervisor_failed", "twd supervisor exited at start", "log: "+s.log("supervisor"))
	}
	return nil
}

// ---- supervisor ----

type child struct {
	name    string
	args    []string
	logPath string

	mu       sync.Mutex
	cmd      *exec.Cmd
	done     chan struct{}
	restarts []time.Time
}

func (c *child) start(slog func(string, ...any)) error {
	lf, err := os.OpenFile(c.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cmd := exec.Command(c.args[0], c.args[1:]...)
	cmd.Stdout, cmd.Stderr = lf, lf
	if err := cmd.Start(); err != nil {
		lf.Close()
		return err
	}
	done := make(chan struct{})
	c.mu.Lock()
	c.cmd, c.done = cmd, done
	c.mu.Unlock()
	slog("started %s pid %d", c.name, cmd.Process.Pid)
	go func() {
		err := cmd.Wait()
		lf.Close()
		slog("%s exited: %v", c.name, err)
		close(done)
	}()
	return nil
}

func (c *child) stop(timeout time.Duration) {
	c.mu.Lock()
	cmd, done := c.cmd, c.done
	c.mu.Unlock()
	if cmd == nil {
		return
	}
	cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(timeout):
		cmd.Process.Kill()
		<-done
	}
}

func cmdSupervise(args []string) error {
	f := newFlags("__supervise")
	if _, err := f.parse(args); err != nil {
		return err
	}
	s, err := openState(*f.stateDir)
	if err != nil {
		return err
	}
	if !s.configured() {
		return fail("not_up", "no config in "+s.Dir)
	}
	logf := func(format string, a ...any) {
		fmt.Fprintf(os.Stderr, "%s supervisor: %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, a...))
	}
	if pidAlive(s.supervisorPID()) && s.supervisorPID() != os.Getpid() {
		return fail("already_running", "a supervisor is already running")
	}
	if err := os.WriteFile(s.pidFile(), []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
		return err
	}
	defer os.Remove(s.pidFile())
	for _, d := range []string{s.runDir(), s.hostdRunDir(), filepath.Join(s.Dir, "mgmt"), filepath.Join(s.Dir, "hostd")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
		os.Chmod(d, 0o700)
	}
	mgmtBin, err := findBin("twd-mgmt")
	if err != nil {
		return err
	}
	hostdBin, err := findBin("twd-hostd")
	if err != nil {
		return err
	}
	margs := []string{mgmtBin, "-state-dir", filepath.Join(s.Dir, "mgmt"), "-socket", s.mgmtSock(),
		"-device-delete-delay", s.Cfg.DeviceDeleteDelay}
	if s.Cfg.TSAPIURL != "" {
		margs = append(margs, "-tsapi-url", s.Cfg.TSAPIURL)
	}
	hargs := []string{hostdBin, "-mgmt", "unix://" + s.mgmtSock(), "-host-id", s.Cfg.HostID,
		"-data-dir", filepath.Join(s.Dir, "hostd"), "-run-dir", s.hostdRunDir(), "-workerd", s.Cfg.Workerd,
		"-heartbeat", s.Cfg.Heartbeat, "-kill-tombstone", s.Cfg.KillTombstone, "-hostname-prefix", s.Cfg.HostnamePrefix}
	if s.Cfg.Ephemeral {
		hargs = append(hargs, "-ephemeral")
	}
	mg := &child{name: "twd-mgmt", args: margs, logPath: s.log("mgmt")}
	hd := &child{name: "twd-hostd", args: hargs, logPath: s.log("hostd")}

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	stopping := make(chan struct{})
	go func() {
		<-sig
		close(stopping)
	}()
	waitMgmt := func() bool {
		for i := 0; i < 600; i++ {
			if s.mgmtHealthy(context.Background()) {
				return true
			}
			select {
			case <-stopping:
				return false
			case <-time.After(50 * time.Millisecond):
			}
		}
		return false
	}
	// supervise runs c, restarting it with backoff (≤10 restarts per 5 min).
	var wg sync.WaitGroup
	supervise := func(c *child, before func() bool) {
		defer wg.Done()
		backoff := 500 * time.Millisecond
		for {
			if before != nil && !before() {
				return
			}
			if err := c.start(logf); err != nil {
				logf("start %s: %v", c.name, err)
			} else {
				st := time.Now()
				select {
				case <-c.done:
				case <-stopping:
					return
				}
				if time.Since(st) > time.Minute {
					backoff = 500 * time.Millisecond
				}
			}
			now := time.Now()
			c.restarts = append(c.restarts, now)
			for len(c.restarts) > 0 && now.Sub(c.restarts[0]) > 5*time.Minute {
				c.restarts = c.restarts[1:]
			}
			if len(c.restarts) > 10 {
				logf("%s keeps exiting; giving up (see %s)", c.name, c.logPath)
				return
			}
			select {
			case <-stopping:
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
		}
	}
	logf("supervising in %s (tailnet %s)", s.Dir, s.Cfg.Tailnet)
	wg.Add(2)
	go supervise(mg, nil)
	go supervise(hd, waitMgmt)
	<-stopping
	logf("stopping: hostd first (nodes close in parallel), then mgmt (flushes pending device deletes)")
	hd.stop(45 * time.Second)
	mg.stop(90 * time.Second)
	wg.Wait()
	logf("stopped")
	return nil
}

// ---- down ----

type downResult struct {
	Killed  []string           `json:"killed"`
	Flushed int                `json:"devicesDeleted"`
	Timings map[string]float64 `json:"timingsMs"`
}

func cmdDown(args []string) error {
	f := newFlags("down")
	purge := f.Bool("purge", false, "kill every live object (audit: \"twd down --purge\") and delete its device before stopping (default with --ephemeral)")
	if _, err := f.parse(args); err != nil {
		return err
	}
	s, err := openState(*f.stateDir)
	if err != nil {
		return err
	}
	t0 := time.Now()
	res := downResult{Killed: []string{}, Timings: map[string]float64{}}
	if !s.supervisorAlive() {
		emit(f, res, func() { fmt.Println("twd is not running in " + s.Dir) })
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if (*purge || s.Cfg.Ephemeral) && s.Cfg.TenantID != "" && s.mgmtHealthy(ctx) {
		var objs []mgmt.Object
		if err := s.mgmt().Do(ctx, "GET", "/v0/objects?tenant="+s.Cfg.TenantID, nil, &objs); err == nil {
			live := map[string]bool{}
			for _, o := range objs {
				if o.Status != "killed" && o.Status != "failed" {
					live[o.ID] = true
				}
			}
			for _, o := range objs {
				if !live[o.ID] || live[o.ParentID] {
					continue // dead, or killed with its root (cascade)
				}
				var kr struct {
					Killed []string `json:"killed"`
				}
				if err := s.mgmt().DoHeaders(ctx, "POST", "/v0/objects/"+o.ID+"/kill", map[string]any{"cascade": true, "reason": "twd down --purge"},
					&kr, map[string]string{"Twd-Actor": actor()}); err == nil {
					res.Killed = append(res.Killed, kr.Killed...)
				}
			}
		}
		var fr struct {
			Flushed int `json:"flushed"`
		}
		s.mgmt().Do(ctx, "POST", "/v0/flush", nil, &fr)
		res.Flushed = fr.Flushed
		res.Timings["purge"] = ms(time.Since(t0))
	}
	pid := s.supervisorPID()
	syscall.Kill(pid, syscall.SIGTERM)
	for pidAlive(pid) {
		if time.Since(t0) > 3*time.Minute {
			syscall.Kill(pid, syscall.SIGKILL)
			return fail("stop_timeout", "the supervisor did not stop in 3 min; killed it", "log: "+s.log("supervisor"))
		}
		time.Sleep(50 * time.Millisecond)
	}
	res.Timings["total"] = ms(time.Since(t0))
	emit(f, res, func() {
		if len(res.Killed) > 0 {
			fmt.Printf("✓ purged   %d objects killed · %d devices deleted\n", len(res.Killed), res.Flushed)
		}
		fmt.Printf("✓ stopped  twd-hostd, twd-mgmt (%.1fs) · state kept in %s\n", res.Timings["total"]/1000, s.Dir)
	})
	return nil
}

func actor() string {
	u := os.Getenv("USER")
	if u == "" {
		u = "local"
	}
	h, _ := os.Hostname()
	h, _, _ = strings.Cut(h, ".")
	return u + "@" + h + " (twd cli)"
}

// ---- status ----

type statusResult struct {
	StateDir   string                 `json:"stateDir"`
	Tailnet    string                 `json:"tailnet"`
	TenantID   string                 `json:"tenantID"`
	Supervisor bool                   `json:"supervisor"`
	Mgmt       bool                   `json:"mgmt"`
	Host       bool                   `json:"host"`
	HostAgeMS  int64                  `json:"hostHeartbeatAgeMs"`
	Pending    int                    `json:"pendingDeviceDeletes"`
	MintQueue  int                    `json:"mintQueue"`
	Rate       float64                `json:"registrationsPerMinute"`
	Workloads  []hostd.WorkloadStatus `json:"workloads"`
	Objects    map[string]int         `json:"objectsByStatus"`
	Healthy    bool                   `json:"healthy"`
}

func cmdStatus(args []string) error {
	f := newFlags("status")
	if _, err := f.parse(args); err != nil {
		return err
	}
	s, err := openState(*f.stateDir)
	if err != nil {
		return err
	}
	if !s.configured() {
		return fail("not_up", "twd is not set up in "+s.Dir).withFix("twd up --tailnet <tailnet> --api-key-file <file>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := statusResult{StateDir: s.Dir, Tailnet: s.Cfg.Tailnet, TenantID: s.Cfg.TenantID, Objects: map[string]int{}}
	r.Supervisor = s.supervisorAlive()
	if r.Supervisor && s.mgmtHealthy(ctx) {
		r.Mgmt = true
		var st struct {
			Pending int `json:"pendingDeviceDeletes"`
		}
		s.mgmt().Do(ctx, "GET", "/v0/status", nil, &st)
		r.Pending = st.Pending
		var tn struct {
			Q    int     `json:"mintQueue"`
			Rate float64 `json:"effectiveRegistrationsPerMinute"`
		}
		s.mgmt().Do(ctx, "GET", "/v0/tenants/"+s.Cfg.TenantID, nil, &tn)
		r.MintQueue, r.Rate = tn.Q, tn.Rate
		var hosts []struct {
			ID       string `json:"hostID"`
			LastSeen int64  `json:"lastSeen"`
		}
		s.mgmt().Do(ctx, "GET", "/v0/hosts", nil, &hosts)
		for _, h := range hosts {
			if h.ID == s.Cfg.HostID {
				r.HostAgeMS = time.Since(time.UnixMilli(h.LastSeen)).Milliseconds()
				r.Host = r.HostAgeMS < 10_000
			}
		}
	}
	var hv hostd.StatusView
	if r.Supervisor && s.hostd().Do(ctx, "GET", "/v0/status", nil, &hv) == nil {
		r.Workloads = hv.Workloads
		for _, o := range hv.Objects {
			r.Objects[o.Status]++
		}
	} else {
		r.Host = false
	}
	r.Healthy = r.Supervisor && r.Mgmt && r.Host
	emit(f, r, func() {
		mark := func(ok bool) string {
			if ok {
				return "✓"
			}
			return "✗"
		}
		fmt.Printf("twd status · %s · tailnet %s\n", s.Dir, s.Cfg.Tailnet)
		fmt.Printf("%s supervisor  pid %d\n", mark(r.Supervisor), s.supervisorPID())
		fmt.Printf("%s mgmt        tenant %s · mint queue %d · %.0f registrations/min · %d pending device deletes\n", mark(r.Mgmt), r.TenantID, r.MintQueue, r.Rate, r.Pending)
		fmt.Printf("%s host        %s · heartbeat %.1fs ago · objects %v\n", mark(r.Host), s.Cfg.HostID, float64(r.HostAgeMS)/1000, r.Objects)
		for _, w := range r.Workloads {
			e := ""
			if w.Error != "" {
				e = " · " + w.Error
			}
			fmt.Printf("%s workload    %s %s (%s, epoch %d, %d requests)%s\n", mark(w.Status == "active"), w.Workload, w.Status, w.DeploymentID, w.Epoch, w.Requests, e)
		}
	})
	if !r.Healthy {
		return fail("unhealthy", "twd is not healthy in "+s.Dir).withFix("twd logs --component supervisor", "twd up")
	}
	return nil
}
