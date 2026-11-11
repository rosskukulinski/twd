// twd-mgmt is twd's management plane (M2): tenants, deployments, the object
// registry with lineage, epoch-fenced namespace leases, the mint broker
// (API-key or WIF), the per-tenant registration throttle, spawn checks,
// kill, the reaper and the hash-chained audit log.
//
// It serves the /v1 API (api/openapi.yaml) on its own tsnet node on the
// operator tailnet, authenticated by WhoIs; the twd CLI on the same machine
// reaches it through the node's loopback proxy (<state-dir>/loopback.json).
// Only with -local-dev (twd dev, hermetic tests) does it also serve a unix
// socket guarded by the per-host token. In WIF mode it is also an OIDC
// issuer, served via Funnel (and/or a local listener).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/mgmt"
	"github.com/rosskukulinski/twd/netd"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "twd-mgmt:", err)
		os.Exit(1)
	}
}

func run() error {
	home, _ := os.UserHomeDir()
	var (
		stateDir     = flag.String("state-dir", filepath.Join(home, ".twd", "mgmt"), "state directory (0700): db, custody, issuer key")
		localDev     = flag.Bool("local-dev", false, "ALSO serve the API on a unix socket guarded by the per-host token (twd dev, hermetic tests). Normal operation is tsnet only")
		socket       = flag.String("socket", filepath.Join(home, ".twd", "run", "mgmt.sock"), "-local-dev: the unix socket path")
		tokenFile    = flag.String("host-token-file", filepath.Join(home, ".twd", "hostd", "mgmt.token"), "-local-dev: per-host bearer token for the unix socket (0600; minted here, read by twd-hostd and the twd CLI; sent as Twd-Host-Token)")
		rotateToken  = flag.Bool("rotate-host-token", false, "mint a fresh host token at start (twd up does this)")
		dbDSN        = flag.String("db", "", "store: SQLite path (default <state-dir>/mgmt.db) or postgres://… DSN")
		dbDSNFile    = flag.String("db-file", "", "read the store DSN from this file (keeps a Postgres password off argv)")
		tsapiURL     = flag.String("tsapi-url", tsapi.DefaultBaseURL, "Tailscale API base URL")
		issuerURL    = flag.String("issuer-url", "", "WIF: public https issuer origin (default with -funnel: https://<tsnet name>)")
		issuerListen = flag.String("issuer-listen", "", "WIF: also serve the issuer on this local address (plain HTTP, e.g. behind a proxy)")
		funnel       = flag.Bool("funnel", false, "WIF: serve the issuer publicly via tsnet ListenFunnel :443 (needs -tsnet-hostname)")
		tsHostname   = flag.String("tsnet-hostname", "", "serve the API on the operator tailnet as this tsnet node (e.g. twd-mgmt); required unless -local-dev")
		noLoopback   = flag.Bool("no-loopback", false, "don't publish the node's loopback proxy for the local twd CLI (<state-dir>/loopback.json)")
		version      = flag.String("version", "", "reported by GET /v1/status")
		tsControl    = flag.String("tsnet-control-url", "", "operator tailnet control URL (required with -tsnet-hostname)")
		tsAuthFile   = flag.String("tsnet-authkey-file", "", "file with the operator-tailnet auth key for the first join (TS_AUTHKEY=…); without it the node state must already be logged in (twd up does the interactive login)")
		tsTags       = flag.String("tsnet-tags", mgmt.MgmtTag, "comma-separated tags the mgmt node advertises")
		tsTLS        = flag.Bool("tsnet-tls", true, "also serve the API on :443 with the node's HTTPS certificate when the tailnet has HTTPS enabled (plain :80 is always served)")
		admins       = flag.String("admins", "", "comma-separated tailnet logins admitted as admins even without the "+mgmt.AdminCap+" capability (override; the normal path is the cap, granted to "+mgmt.MgmtTag+")")
		custodyKind  = flag.String("custody", "file", "custody backend: file (AES-GCM in <state-dir>/custody) or setec")
		setecURL     = flag.String("setec-url", "", "setec server URL (with -custody setec)")
		reapEvery    = flag.Duration("reap-interval", 10*time.Second, "reaper interval")
		delDelay     = flag.Duration("device-delete-delay", 0, "defer kill tier 2 (device delete) this long after the tier-1 push, so the host's tombstone answers new connections with object_killed (keep < hostd -kill-tombstone)")
		verbose      = flag.Bool("v", false, "debug logging")
	)
	flag.Parse()
	lvl := slog.LevelInfo
	if *verbose {
		lvl = slog.LevelDebug
	}
	log := slog.New(netd.NewRedactingHandler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))

	if err := os.MkdirAll(*stateDir, 0o700); err != nil {
		return err
	}
	os.Chmod(*stateDir, 0o700)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := *dbDSN
	if *dbDSNFile != "" {
		b, err := os.ReadFile(*dbDSNFile)
		if err != nil {
			return err
		}
		dsn = strings.TrimSpace(string(b))
	}
	if dsn == "" {
		dsn = filepath.Join(*stateDir, "mgmt.db")
	}
	store, err := mgmt.OpenStore(ctx, dsn)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer store.Close()

	var custody mgmt.Custody
	switch *custodyKind {
	case "file":
		if custody, err = mgmt.OpenFileCustody(filepath.Join(*stateDir, "custody")); err != nil {
			return err
		}
	case "setec":
		if *setecURL == "" {
			return errors.New("-custody setec needs -setec-url")
		}
		custody = &mgmt.SetecCustody{BaseURL: *setecURL}
	default:
		return fmt.Errorf("unknown -custody %q", *custodyKind)
	}
	secret, err := mgmt.LoadOrCreateSecret(filepath.Join(*stateDir, "id.secret"))
	if err != nil {
		return err
	}
	replicaID, err := mgmt.LoadOrCreateReplicaID(filepath.Join(*stateDir, "replica.id"))
	if err != nil {
		return err
	}

	// Operator-tailnet node (optional).
	var ts *tsnet.Server
	self := mgmt.SelfInfo{}
	if *tsHostname != "" {
		if *tsControl == "" {
			return errors.New("-tsnet-hostname needs an explicit -tsnet-control-url")
		}
		ts = &tsnet.Server{Hostname: *tsHostname, Dir: filepath.Join(*stateDir, "tsnet"), ControlURL: *tsControl,
			Logf: func(string, ...any) {}}
		if *tsTags != "" {
			ts.AdvertiseTags = strings.Split(*tsTags, ",")
		}
		if *tsAuthFile != "" {
			k, err := tsapi.LoadEnvFile(*tsAuthFile, "TS_AUTHKEY")
			if err != nil {
				return err
			}
			ts.AuthKey = k
		}
		upCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		st, err := tsnetUp(upCtx, ts, ts.AuthKey == "")
		cancel()
		if err != nil {
			return fmt.Errorf("tsnet up: %w", err)
		}
		defer ts.Close()
		self.DNSName = strings.TrimSuffix(st.Self.DNSName, ".")
		self.NodeID = string(st.Self.ID)
		if len(st.TailscaleIPs) > 0 {
			self.IP = st.TailscaleIPs[0].String()
		}
		if st.CurrentTailnet != nil {
			self.Tailnet = st.CurrentTailnet.MagicDNSSuffix
		}
		self.HTTPS = *tsTLS && len(ts.CertDomains()) > 0
		if *funnel && *issuerURL == "" {
			if doms := ts.CertDomains(); len(doms) > 0 {
				*issuerURL = "https://" + doms[0]
			}
		}
		tags := []string{}
		if st.Self.Tags != nil {
			tags = st.Self.Tags.AsSlice()
		}
		log.Info("tailnet node", "name", self.DNSName, "tailnet", self.Tailnet, "tags", tags, "https", self.HTTPS, "ips", st.TailscaleIPs)
	}

	var issuer *mgmt.Issuer
	if *issuerURL != "" {
		if issuer, err = mgmt.LoadOrCreateIssuer(*issuerURL, filepath.Join(*stateDir, "issuer.key")); err != nil {
			return err
		}
		log.Info("OIDC issuer", "base", issuer.BaseURL)
	}

	mcfg := mgmt.Config{Store: store, Custody: custody, Broker: &mgmt.Broker{BaseURL: *tsapiURL, Issuer: issuer},
		IDSecret: secret, ReplicaID: replicaID, ReapInterval: *reapEvery, DeviceDeleteDelay: *delDelay, Log: log,
		OperatorTailnet: self.Tailnet, OperatorControlURL: *tsControl}
	if ts != nil {
		mcfg.TailnetDial = ts.Dial // kill/assignment pushes to hosts' own nodes
	}
	m, err := mgmt.New(mcfg)
	if err != nil {
		return err
	}
	if err := m.Start(); err != nil {
		return err
	}
	defer m.Close()
	if issuer != nil {
		self.Issuer = issuer.BaseURL
	}
	api := &mgmt.Server{M: m, Self: self, Version: *version}
	if *admins != "" {
		api.Admins = strings.Split(*admins, ",")
	}
	if ts == nil && !*localDev {
		return errors.New("no listeners: normal operation is tsnet only (-tsnet-hostname); -local-dev serves the unix socket for twd dev and tests")
	}
	errc := make(chan error, 4)
	serve := func(name string, ln net.Listener, h http.Handler) *http.Server {
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("%s: %w", name, err)
			}
		}()
		log.Info("serving", "listener", name, "addr", ln.Addr().String())
		return srv
	}
	var servers []*http.Server
	loopbackPath := filepath.Join(*stateDir, "loopback.json")
	os.Remove(loopbackPath)
	if *localDev && *socket != "" {
		// RT-03: same-uid peercred is not authorization. Every request on
		// the socket must carry the per-host token (minted here into a 0600
		// file under hostd's state dir; hostd and the CLI read it).
		token, err := mgmt.LoadOrCreateHostToken(*tokenFile, *rotateToken)
		if err != nil {
			return fmt.Errorf("host token: %w", err)
		}
		ln, err := mgmt.ListenUnix(*socket)
		if err != nil {
			return err
		}
		defer os.Remove(*socket)
		log.Info("host token", "file", *tokenFile, "rotated", *rotateToken)
		api.Listeners = append(api.Listeners, "unix:"+*socket)
		servers = append(servers, serve("unix", ln, mgmt.LocalHandler(api.Handler(), token)))
	}
	if ts != nil {
		lc, err := ts.LocalClient()
		if err != nil {
			return err
		}
		whois := func(ctx context.Context, addr string) (mgmt.WhoIsInfo, error) {
			wi, err := lc.WhoIs(ctx, addr)
			if err != nil {
				return mgmt.WhoIsInfo{}, err
			}
			info := mgmt.WhoIsInfo{NodeID: string(wi.Node.StableID), Name: wi.Node.Name, Tags: wi.Node.Tags}
			if wi.UserProfile != nil {
				info.Login = wi.UserProfile.LoginName
			}
			for c := range wi.CapMap {
				info.Caps = append(info.Caps, string(c))
			}
			return info, nil
		}
		ln, err := ts.Listen("tcp", ":80")
		if err != nil {
			return err
		}
		th := api.TailnetHandler(whois, api.Handler())
		api.Listeners = append(api.Listeners, "tsnet:80")
		servers = append(servers, serve("tsnet", ln, th))
		if !*noLoopback {
			// The operator console: the twd CLI on this machine dials mgmt's
			// own tailnet IP through the node's SOCKS5 loopback proxy, so
			// its requests carry mgmt's identity (WhoIs → operator). The
			// credential lives in a 0600 file next to the state; nothing
			// listens on a unix socket.
			addr, cred, _, err := ts.Loopback()
			if err != nil {
				return fmt.Errorf("tsnet loopback: %w", err)
			}
			lb := mgmt.Loopback{Addr: addr, ProxyCred: cred, MgmtIP: self.IP, DNSName: self.DNSName, HTTPS: self.HTTPS, PID: os.Getpid()}
			b, _ := json.Marshal(lb)
			if err := os.WriteFile(loopbackPath+".tmp", b, 0o600); err != nil {
				return err
			}
			if err := os.Rename(loopbackPath+".tmp", loopbackPath); err != nil {
				return err
			}
			defer os.Remove(loopbackPath)
			api.Listeners = append(api.Listeners, "loopback:"+addr)
			log.Info("operator console", "loopback", addr, "file", loopbackPath)
		}
		if self.HTTPS {
			// The API over the node's own HTTPS certificate: what `twd
			// link` uses from a developer machine. tailnet-only (ListenTLS
			// never serves Funnel traffic).
			tln, err := ts.ListenTLS("tcp", ":443")
			if err != nil {
				return fmt.Errorf("tsnet tls: %w", err)
			}
			api.Listeners = append(api.Listeners, "tsnet:443")
			servers = append(servers, serve("tsnet-tls", tln, th))
			// Fetch the node's certificate now, in the background: a new
			// node name's first certificate takes 30–100 s, and without
			// Funnel (oauth mode) nothing else would request it before the
			// first `twd link`, whose TLS handshake would time out.
			go func() {
				lc, err := ts.LocalClient()
				if err != nil || self.DNSName == "" {
					return
				}
				st := time.Now()
				for attempt := 1; ; attempt++ {
					cctx, ccancel := context.WithTimeout(context.Background(), 3*time.Minute)
					_, _, err := lc.CertPair(cctx, self.DNSName)
					ccancel()
					if err == nil {
						log.Info("HTTPS certificate ready", "name", self.DNSName, "ms", time.Since(st).Milliseconds(), "attempts", attempt)
						return
					}
					// ACME orders fail transiently (seen: CreateOrder 404); a
					// TLS handshake retries on its own later, this only warms.
					log.Warn("HTTPS certificate prefetch failed (twd link falls back to http on :80 meanwhile)", "name", self.DNSName, "attempt", attempt, "err", err)
					if attempt == 4 {
						return
					}
					time.Sleep(time.Duration(attempt) * 20 * time.Second)
				}
			}()
		} else if *tsTLS {
			log.Warn("HTTPS is not enabled on this tailnet: the API is served on :80 only; `twd link` uses plain HTTP (WireGuard-encrypted, WhoIs-authenticated)")
		}
		if *funnel {
			if issuer == nil {
				return errors.New("-funnel needs an issuer URL")
			}
			fl, err := ts.ListenFunnel("tcp", ":443", tsnet.FunnelOnly())
			if err != nil {
				return fmt.Errorf("funnel: %w", err)
			}
			api.Listeners = append(api.Listeners, "funnel:443")
			servers = append(servers, serve("funnel", fl, issuer.Handler()))
		}
	}
	if *issuerListen != "" {
		if issuer == nil {
			return errors.New("-issuer-listen needs -issuer-url")
		}
		ln, err := net.Listen("tcp", *issuerListen)
		if err != nil {
			return err
		}
		servers = append(servers, serve("issuer", ln, issuer.Handler()))
	}
	if len(servers) == 0 {
		return errors.New("no listeners: set -tsnet-hostname (normal operation) or -local-dev")
	}
	select {
	case <-ctx.Done():
	case err := <-errc:
		log.Error("listener failed", "err", err)
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, s := range servers {
		s.Shutdown(sctx)
	}
	return nil
}

// tsnetUp brings the node up. If it needs an interactive login and no auth
// key was given, it fails fast with the login URL instead of waiting for
// someone to click it: `twd up` owns the interactive flow.
func tsnetUp(ctx context.Context, ts *tsnet.Server, failOnLogin bool) (*ipnstate.Status, error) {
	if !failOnLogin {
		return ts.Up(ctx)
	}
	lc, err := ts.LocalClient()
	if err != nil {
		return nil, err
	}
	w, err := lc.WatchIPNBus(ctx, ipn.NotifyInitialState)
	if err != nil {
		return nil, err
	}
	defer w.Close()
	for {
		n, err := w.Next()
		if err != nil {
			return nil, err
		}
		if n.ErrMessage != nil {
			return nil, errors.New(*n.ErrMessage)
		}
		if n.BrowseToURL != nil {
			return nil, fmt.Errorf("the mgmt node needs an interactive login (run `twd up` on this host to log in): %s", *n.BrowseToURL)
		}
		if n.State != nil && *n.State == ipn.Running {
			return ts.Up(ctx)
		}
	}
}
