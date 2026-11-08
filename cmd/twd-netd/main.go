// twd-netd runs one tenant's object nodes on this host: ingress to a workerd
// socket, the attributed egress proxy, and a local control socket.
//
//	twd-netd -control-url URL -state-dir DIR -run-dir DIR \
//	  [-authkey-file F | -mint-api-key-file F -tailnet T -tags tag:x] \
//	  [-upstream unix-socket] [-objects a,b,c] [-rejoin]
//
// Sockets (all under -run-dir, which is forced to 0700):
//
//	egress.sock  proxy-style HTTP for workerd's netd-egress external
//	ctl.sock     control API: GET /v1/objects, POST /v1/objects?id=,
//	             GET /v1/objects/{id}, POST /v1/objects/{id}/kill?reason=&logout=1
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

	"github.com/rosskukulinski/twd/internal/procsb"
	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/netd"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "twd-netd:", netd.Redact(err.Error()))
		os.Exit(1)
	}
}

func run() error {
	var (
		controlURL    = flag.String("control-url", "", "coordination server URL (required; e.g. https://controlplane.tailscale.com)")
		stateDir      = flag.String("state-dir", "", "per-object node state root (required)")
		runDir        = flag.String("run-dir", "", "runtime dir for egress.sock and ctl.sock (required)")
		upstream      = flag.String("upstream", "", "unix socket of the workerd ingress (cfBlobHeader=x-twd-cf)")
		authKeyFile   = flag.String("authkey-file", "", "file holding a static auth key (raw, or TS_AUTHKEY=...)")
		mintKeyFile   = flag.String("mint-api-key-file", "", "file with TS_API_KEY=...; mint one single-use key per object via the API")
		tailnet       = flag.String("tailnet", "", "tailnet name for -mint-api-key-file / -delete-devices (e.g. tail7906d5.ts.net)")
		deleteDevices = flag.Bool("delete-devices", false, "on kill, delete the device via the API (needs -mint-api-key-file)")
		tags          = flag.String("tags", "", "comma-separated tags for object nodes")
		ephemeral     = flag.Bool("ephemeral", false, "register object nodes as ephemeral")
		objects       = flag.String("objects", "", "comma-separated object IDs to create at start")
		rejoin        = flag.Bool("rejoin", true, "rejoin every object with saved state at start")
		joinPar       = flag.Int("join-parallel", 16, "max concurrent node joins")
		port          = flag.Int("ingress-port", 80, "ingress port on each object node")
		siblings      = flag.Bool("allow-siblings", false, "allow egress to other objects on this host")
		logLevel      = flag.String("log-level", "info", "debug|info|warn|error")
		tsnetLogs     = flag.Bool("tsnet-logs", false, "forward tsnet logs at debug level")
	)
	flag.Parse()
	if *controlURL == "" || *stateDir == "" || *runDir == "" {
		flag.Usage()
		return errors.New("-control-url, -state-dir and -run-dir are required")
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		return err
	}
	log := slog.New(netd.NewRedactingHandler(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})))

	var tagList []string
	if *tags != "" {
		tagList = strings.Split(*tags, ",")
	}
	cfg := netd.Config{
		ControlURL: *controlURL, StateRoot: *stateDir, Tags: tagList, Ephemeral: *ephemeral,
		JoinParallel: *joinPar, IngressPort: *port, AllowSiblings: *siblings, Logger: log, TSNetLogs: *tsnetLogs,
	}
	if *upstream != "" {
		cfg.Upstream = netd.UnixUpstream(*upstream)
	}
	switch {
	case *authKeyFile != "" && *mintKeyFile != "":
		return errors.New("use one of -authkey-file and -mint-api-key-file")
	case *authKeyFile != "":
		k, err := readKeyFile(*authKeyFile)
		if err != nil {
			return err
		}
		cfg.Keys = netd.StaticKey(k)
	case *mintKeyFile != "":
		api, err := apiClient(*mintKeyFile, *tailnet)
		if err != nil {
			return err
		}
		cfg.Keys = tsapi.MintSource(api, log)
		if *deleteDevices {
			cfg.OnDelete = func(ctx context.Context, in netd.ObjectInfo) error {
				if in.NodeID == "" {
					return nil
				}
				return api.DeleteDevice(ctx, in.NodeID)
			}
		}
	}

	if err := os.MkdirAll(*runDir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(*runDir, 0o700); err != nil {
		return err
	}
	reg, err := netd.New(cfg)
	if err != nil {
		return err
	}
	defer func() {
		st := time.Now()
		reg.Close()
		log.Info("closed all nodes", "ms", time.Since(st).Milliseconds())
	}()

	egressSock := filepath.Join(*runDir, "egress.sock")
	eln, err := listenUnix(egressSock)
	if err != nil {
		return err
	}
	go (&http.Server{Handler: reg.EgressHandler(), ReadHeaderTimeout: 30 * time.Second}).Serve(eln)

	ctlSock := filepath.Join(*runDir, "ctl.sock")
	cln, err := listenUnix(ctlSock)
	if err != nil {
		return err
	}
	go (&http.Server{Handler: ctlMux(reg), ReadHeaderTimeout: 10 * time.Second}).Serve(cln)

	if *rejoin {
		ids, err := reg.Rejoin()
		if err != nil {
			return err
		}
		if len(ids) > 0 {
			log.Info("rejoining from saved state", "objects", ids)
		}
	}
	for _, id := range strings.Split(*objects, ",") {
		if id = strings.TrimSpace(id); id != "" {
			if _, err := reg.Create(id, netd.ObjectOptions{}); err != nil {
				return err
			}
		}
	}
	log.Info("twd-netd ready", "egress", egressSock, "ctl", ctlSock, "control", *controlURL)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
	return nil
}

func listenUnix(path string) (net.Listener, error) {
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	return procsb.Guard(ln, nil), os.Chmod(path, 0o600)
}

func readKeyFile(path string) (string, error) {
	if k, err := tsapi.LoadEnvFile(path, "TS_AUTHKEY"); err == nil {
		return k, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	k := strings.TrimSpace(string(b))
	if k == "" || strings.Contains(k, "\n") {
		return "", fmt.Errorf("%s: want a single key or TS_AUTHKEY=...", path)
	}
	return k, nil
}

func apiClient(keyFile, tailnet string) (*tsapi.Client, error) {
	if tailnet == "" || tailnet == "-" {
		return nil, errors.New("-tailnet must name the tailnet explicitly")
	}
	k, err := tsapi.LoadEnvFile(keyFile, "TS_API_KEY")
	if err != nil {
		return nil, err
	}
	return &tsapi.Client{Tailnet: tailnet, APIKey: k}, nil
}

func ctlMux(reg *netd.Registry) http.Handler {
	mux := http.NewServeMux()
	js := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /v1/objects", func(w http.ResponseWriter, r *http.Request) { js(w, 200, reg.Objects()) })
	mux.HandleFunc("POST /v1/objects", func(w http.ResponseWriter, r *http.Request) {
		o, err := reg.Create(r.URL.Query().Get("id"), netd.ObjectOptions{})
		if err != nil {
			js(w, 400, map[string]string{"error": err.Error()})
			return
		}
		if r.URL.Query().Get("wait") == "1" {
			ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
			defer cancel()
			if err := o.Ready(ctx); err != nil {
				js(w, 503, map[string]string{"error": err.Error()})
				return
			}
		}
		js(w, 202, o.Info())
	})
	mux.HandleFunc("GET /v1/objects/{id}", func(w http.ResponseWriter, r *http.Request) {
		o := reg.Get(r.PathValue("id"))
		if o == nil {
			js(w, 404, map[string]string{"error": "no such object"})
			return
		}
		js(w, 200, o.Info())
	})
	mux.HandleFunc("POST /v1/objects/{id}/kill", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		rep, err := reg.Kill(r.Context(), r.PathValue("id"), netd.KillOptions{Reason: q.Get("reason"), Logout: q.Get("logout") == "1", CloseFirst: q.Get("close_first") == "1"})
		if errors.Is(err, netd.ErrNotFound) {
			js(w, 404, map[string]string{"error": err.Error()})
			return
		} else if err != nil {
			js(w, 500, map[string]string{"error": err.Error()})
			return
		}
		js(w, 200, rep)
	})
	return mux
}
