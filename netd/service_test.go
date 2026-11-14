package netd_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptrace"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/internal/tssvc"
	"github.com/rosskukulinski/twd/netd"
	"github.com/rosskukulinski/twd/netd/nettest"
)

// TestService: an object node hosts a Tailscale Service. Flows to the VIP
// reach the same attributed ingress as the node's own address (the caller's
// real WhoIs identity, not 127.0.0.1); object egress resolves Service names
// from the netmap; withdraw and kill stop the Service.
func TestService(t *testing.T) {
	e := startEnv(t, "a", "b")
	ctx := context.Background()
	clientID := selfID(t, e.client)
	const svc, vip = "svc:hello", "100.99.0.10"
	const echoSvc, echoVIP = "svc:echo-svc", "100.99.0.11"

	alc, err := e.reg.Get("a").Server().LocalClient()
	if err != nil {
		t.Fatal(err)
	}
	alc.OmitAuth = true
	blc, _ := e.reg.Get("b").Server().LocalClient()
	blc.OmitAuth = true
	for _, n := range []*nettest.Node{e.client} {
		if err := nettest.AcceptRoutes(ctx, n.LC); err != nil {
			t.Fatal(err)
		}
	}
	if err := nettest.AcceptRoutes(ctx, blc); err != nil {
		t.Fatal(err)
	}

	if err := e.reg.AdvertiseService(ctx, "a", svc); err != nil {
		t.Fatalf("advertise: %v", err)
	}
	if got := e.reg.Get("a").Info().Services; !slices.Equal(got, []string{svc}) {
		t.Fatalf("Info().Services = %v", got)
	}
	if err := e.ctl.HostService(ctx, alc, svc, vip); err != nil {
		t.Fatal(err)
	}
	if err := nettest.WaitDNSRecord(ctx, e.client.LC, vip); err != nil {
		t.Fatal(err)
	}
	fqdn := "http://" + tssvc.Label(svc) + "." + nettest.MagicDNSDomain + "/"

	t.Run("IngressViaVIPKeepsCallerIdentity", func(t *testing.T) {
		var r objResp
		eventually(t, 20*time.Second, func() error {
			code, b, err := tryGet(e.hc, fqdn)
			if err != nil {
				return err
			}
			if code != 200 {
				return errString(string(b))
			}
			return json.Unmarshal(b, &r)
		})
		if r.ObjectID != "a" {
			t.Fatalf("served by %q, want a", r.ObjectID)
		}
		// And it really came through the VIP (not a node of the same name).
		var raddr string
		req, _ := http.NewRequest("GET", fqdn, nil)
		req = req.WithContext(httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) {
			raddr = i.Conn.RemoteAddr().String()
		}}))
		resp, err := (&http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: e.client.Dial}}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if raddr != vip+":80" {
			t.Fatalf("served from %s, not the VIP %s", raddr, vip)
		}
		if r.Caller == nil || r.Caller.NodeID != clientID {
			t.Fatalf("caller = %+v, want the client node %s (WhoIs on the real source address)", r.Caller, clientID)
		}
	})

	t.Run("EgressToSiblingServiceRefused", func(t *testing.T) {
		if err := nettest.WaitDNSRecord(ctx, blc, vip); err != nil {
			t.Fatal(err)
		}
		// b's only route to svc:hello is a, a sibling on this host: refused
		// like a direct call to a (same-host calls go through the platform).
		eventually(t, 20*time.Second, func() error {
			code, h, b := e.get("http://"+e.objIP["b"]+"/egress?url="+fqdn, nil)
			if code == 403 && h.Get(netd.HeaderError) == "forbidden_destination" {
				return nil
			}
			return errString(string(b))
		})
	})

	t.Run("EgressByServiceName", func(t *testing.T) {
		// The echo node hosts its own Service (tssvc, in-process delivery);
		// object b reaches it by name and the echo sees b's node as caller.
		h := tssvc.NewHost(e.echo.Server, e.echo.LC)
		defer h.Close(ctx)
		cl := tssvc.NewConnListener(tssvc.Addr{Name: echoSvc})
		h.Route(80, cl.Deliver)
		go http.Serve(cl, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wi, err := e.echo.LC.WhoIs(r.Context(), r.RemoteAddr)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			io.WriteString(w, "svc-echo "+string(wi.Node.StableID))
		}))
		if err := h.Advertise(ctx, echoSvc, 80); err != nil {
			t.Fatal(err)
		}
		if err := e.ctl.HostService(ctx, e.echo.LC, echoSvc, echoVIP); err != nil {
			t.Fatal(err)
		}
		if err := nettest.WaitDNSRecord(ctx, blc, echoVIP); err != nil {
			t.Fatal(err)
		}
		bID := e.reg.Get("b").Info().NodeID
		for _, u := range []string{"http://echo-svc." + nettest.MagicDNSDomain + "/", "http://echo-svc/", "http://" + echoVIP + "/"} {
			eventually(t, 20*time.Second, func() error {
				code, _, b := e.get("http://"+e.objIP["b"]+"/egress?url="+u, nil)
				if code != 200 || !strings.Contains(string(b), "svc-echo "+bID) {
					return errString(u + ": " + string(b))
				}
				return nil
			})
		}
		// A Service name with no host in b's netmap is refused, typed.
		code, h2, _ := e.get("http://"+e.objIP["b"]+"/egress?url=http://no-such-svc."+nettest.MagicDNSDomain+"/", nil)
		if code != 403 || h2.Get(netd.HeaderError) != "peer_not_visible" {
			t.Fatalf("unknown service: %d %s", code, h2.Get(netd.HeaderError))
		}
	})

	t.Run("WithdrawStopsService", func(t *testing.T) {
		if err := e.reg.WithdrawService(ctx, "a", svc); err != nil {
			t.Fatal(err)
		}
		if got := e.reg.Get("a").Info().Services; len(got) != 0 {
			t.Fatalf("services after withdraw: %v", got)
		}
		ads, _ := alc.GetPrefs(ctx)
		if slices.Contains(ads.AdvertiseServices, svc) {
			t.Fatalf("prefs still advertise %s", svc)
		}
		short := &http.Client{Timeout: 2 * time.Second, Transport: e.hc.Transport}
		eventually(t, 20*time.Second, func() error {
			if _, _, err := tryGet(short, fqdn); err == nil {
				return errString("still answering")
			}
			return nil
		})
		// The node's own address is unaffected.
		if code, _, _ := e.get("http://"+e.objIP["a"]+"/", nil); code != 200 {
			t.Fatalf("node address after withdraw: %d", code)
		}
		// Advertise again: back.
		if err := e.reg.AdvertiseService(ctx, "a", svc); err != nil {
			t.Fatal(err)
		}
		eventually(t, 20*time.Second, func() error {
			code, b, err := tryGet(e.hc, fqdn)
			if err != nil || code != 200 {
				return errString(string(b))
			}
			return nil
		})
	})

	t.Run("KillWithdraws", func(t *testing.T) {
		rep, err := e.reg.Kill(ctx, "a", netd.KillOptions{Reason: "test", SkipDelete: true})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(rep.Services, []string{svc}) || rep.ServiceErr != "" {
			t.Fatalf("kill report services = %v (err %q)", rep.Services, rep.ServiceErr)
		}
		short := &http.Client{Timeout: 2 * time.Second, Transport: e.hc.Transport}
		if _, _, err := tryGet(short, fqdn); err == nil {
			t.Fatal("service still answered after kill")
		}
		if err := e.reg.AdvertiseService(ctx, "a", svc); err == nil {
			t.Fatal("advertise on a killed object succeeded")
		}
	})
}

type errString string

func (e errString) Error() string { return string(e) }

func tryGet(hc *http.Client, u string) (int, []byte, error) {
	resp, err := hc.Get(u)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, nil
}

func eventually(t *testing.T, d time.Duration, f func() error) {
	t.Helper()
	deadline := time.Now().Add(d)
	var err error
	for time.Now().Before(deadline) {
		if err = f(); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("not within %v: %v", d, err)
}
