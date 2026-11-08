// twd-mgmt is twd's management plane (M2): tenants, deployments, the object
// registry with lineage, epoch-fenced namespace leases, the mint broker
// (API-key or WIF), the per-tenant registration throttle, spawn checks,
// kill, the reaper and the hash-chained audit log.
//
// It serves the v0 API (docs/contracts/mgmt-api.md) on a unix socket
// (peer uid must match) and, optionally, on the operator tailnet via tsnet
// with WhoIs auth. In WIF mode it is also an OIDC issuer, served on a local
// listener and/or via Funnel.
package main

import (
	"context"
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
		socket       = flag.String("socket", filepath.Join(home, ".twd", "run", "mgmt.sock"), "unix socket for the v0 API (\"\" to disable)")
		dbDSN        = flag.String("db", "", "store: SQLite path (default <state-dir>/mgmt.db) or postgres://… DSN")
		dbDSNFile    = flag.String("db-file", "", "read the store DSN from this file (keeps a Postgres password off argv)")
		tsapiURL     = flag.String("tsapi-url", tsapi.DefaultBaseURL, "Tailscale API base URL")
		issuerURL    = flag.String("issuer-url", "", "WIF: public https issuer origin (default with -funnel: https://<tsnet name>)")
		issuerListen = flag.String("issuer-listen", "", "WIF: also serve the issuer on this local address (plain HTTP, e.g. behind a proxy)")
		funnel       = flag.Bool("funnel", false, "WIF: serve the issuer publicly via tsnet ListenFunnel :443 (needs -tsnet-hostname)")
		tsHostname   = flag.String("tsnet-hostname", "", "serve the API on the operator tailnet as this tsnet node (e.g. twd-mgmt)")
		tsControl    = flag.String("tsnet-control-url", "", "operator tailnet control URL (required with -tsnet-hostname)")
		tsAuthFile   = flag.String("tsnet-authkey-file", "", "file with the operator-tailnet auth key for the first join (TS_AUTHKEY=…)")
		admins       = flag.String("admins", "", "comma-separated operator-tailnet logins allowed to call CLI endpoints (default: any untagged user)")
		custodyKind  = flag.String("custody", "file", "custody backend: file (AES-GCM in <state-dir>/custody) or setec")
		setecURL     = flag.String("setec-url", "", "setec server URL (with -custody setec)")
		reapEvery    = flag.Duration("reap-interval", 10*time.Second, "reaper interval")
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

	// Operator-tailnet node (optional).
	var ts *tsnet.Server
	if *tsHostname != "" {
		if *tsControl == "" {
			return errors.New("-tsnet-hostname needs an explicit -tsnet-control-url")
		}
		ts = &tsnet.Server{Hostname: *tsHostname, Dir: filepath.Join(*stateDir, "tsnet"), ControlURL: *tsControl,
			Logf: func(string, ...any) {}}
		if *tsAuthFile != "" {
			k, err := tsapi.LoadEnvFile(*tsAuthFile, "TS_AUTHKEY")
			if err != nil {
				return err
			}
			ts.AuthKey = k
		}
		upCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		_, err := ts.Up(upCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("tsnet up: %w", err)
		}
		defer ts.Close()
		if *funnel && *issuerURL == "" {
			if doms := ts.CertDomains(); len(doms) > 0 {
				*issuerURL = "https://" + doms[0]
			}
		}
	}

	var issuer *mgmt.Issuer
	if *issuerURL != "" {
		if issuer, err = mgmt.LoadOrCreateIssuer(*issuerURL, filepath.Join(*stateDir, "issuer.key")); err != nil {
			return err
		}
		log.Info("OIDC issuer", "base", issuer.BaseURL)
	}

	m, err := mgmt.New(mgmt.Config{Store: store, Custody: custody, Broker: &mgmt.Broker{BaseURL: *tsapiURL, Issuer: issuer},
		IDSecret: secret, ReapInterval: *reapEvery, Log: log})
	if err != nil {
		return err
	}
	if err := m.Start(); err != nil {
		return err
	}
	defer m.Close()
	api := &mgmt.Server{M: m}
	if *admins != "" {
		api.Admins = strings.Split(*admins, ",")
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
	if *socket != "" {
		ln, err := mgmt.ListenUnix(*socket)
		if err != nil {
			return err
		}
		defer os.Remove(*socket)
		servers = append(servers, serve("unix", ln, mgmt.LocalHandler(api.Handler())))
	}
	if ts != nil {
		lc, err := ts.LocalClient()
		if err != nil {
			return err
		}
		whois := func(ctx context.Context, addr string) (string, []string, string, error) {
			wi, err := lc.WhoIs(ctx, addr)
			if err != nil {
				return "", nil, "", err
			}
			login := ""
			if wi.UserProfile != nil {
				login = wi.UserProfile.LoginName
			}
			return string(wi.Node.StableID), wi.Node.Tags, login, nil
		}
		ln, err := ts.Listen("tcp", ":80")
		if err != nil {
			return err
		}
		servers = append(servers, serve("tsnet", ln, api.TailnetHandler(whois, api.Handler())))
		if *funnel {
			if issuer == nil {
				return errors.New("-funnel needs an issuer URL")
			}
			fl, err := ts.ListenFunnel("tcp", ":443", tsnet.FunnelOnly())
			if err != nil {
				return fmt.Errorf("funnel: %w", err)
			}
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
		return errors.New("no listeners: set -socket and/or -tsnet-hostname")
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
