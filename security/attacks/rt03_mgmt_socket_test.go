package attacks

// RT-03 regression test (Critical, Linux process/Node tier): the mgmt admin
// unix socket (mgmt/server.go: ListenUnix + uidListener) used to authorize
// by same-uid peercred only. Every tenant workload runs as the same uid as
// mgmt, so any tenant process that could reach the socket (the Node tier
// on Linux, or a workerd isolate escape) was the "local" admin principal:
// list every tenant (and their API key file paths), kill any object, …
//
// Fixed invariant: same-uid peercred is kept as an additional check, but a
// request on the socket is admitted only with the per-host bearer token
// (Twd-Host-Token) that mgmt minted into a 0600 file under hostd's state
// dir (twd-mgmt -host-token-file; rotated on `twd up`). A bare same-uid
// connection is accepted at the TCP level and then refused by every
// endpoint, admin and host alike, including /v0/healthz. This test serves
// the real local wrapper on the real listener and drives it as a tenant
// process would.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/mgmt"
)

func TestRT03_MgmtSocketRejectsBareSameUid(t *testing.T) {
	dir, err := mkShort()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = removeAll(dir) })
	sock := filepath.Join(dir, "mgmt.sock")
	ln, err := mgmt.ListenUnix(sock)
	if err != nil {
		t.Fatalf("ListenUnix: %v", err)
	}
	defer ln.Close()

	// The API handler, as cmd/twd-mgmt serves it on the socket. The inner
	// handler stands for the admin API and records whether it was reached:
	// any call that gets through with a bad token is the bug.
	token := mgmt.NewHostToken()
	reached := 0
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		w.Write([]byte(`[{"tenantID":"tn_victim","apiKeyFile":"/home/x/.config/key.env"}]`))
	})
	srv := &http.Server{Handler: mgmt.LocalHandler(inner, token)}
	go srv.Serve(ln)
	defer srv.Close()

	hc := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	call := func(hdr map[string]string) (int, string, string) {
		req, _ := http.NewRequest("GET", "http://twd-mgmt/v0/tenants", nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		res, err := hc.Do(req)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		var e struct {
			Code string `json:"code"`
		}
		json.Unmarshal(b, &e)
		return res.StatusCode, e.Code, string(b)
	}

	// The PoC: a bare same-uid connection, as a tenant Node app makes it.
	st, code, body := call(nil)
	if st != 401 || code != "host_token_required" || reached != 0 || strings.Contains(body, "tn_victim") {
		t.Fatalf("RT-03: mgmt admin socket admitted a bare same-uid peer with no credential (%d %s %s); a tenant Node app takes over every tenant", st, code, body)
	}
	// A guessed/forged token.
	st, code, body = call(map[string]string{mgmt.HostTokenHeader: mgmt.NewHostToken()})
	if st != 403 || code != "host_token_invalid" || reached != 0 || strings.Contains(body, "tn_victim") {
		t.Fatalf("RT-03: wrong token admitted: %d %s %s", st, code, body)
	}
	// Twd-Host-ID alone (the pre-fix "I am a host" header) is not a credential.
	st, code, _ = call(map[string]string{"Twd-Host-ID": "h1"})
	if st != 401 || code != "host_token_required" || reached != 0 {
		t.Fatalf("RT-03: Twd-Host-ID without a token admitted: %d %s", st, code)
	}
	// The real token (hostd, or the CLI reading the same 0600 file) works.
	if st, _, body = call(map[string]string{mgmt.HostTokenHeader: token}); st != 200 || reached != 1 || !strings.Contains(body, "tn_victim") {
		t.Fatalf("token holder refused: %d %s", st, body)
	}
	// An unconfigured token fails closed, not open.
	sock2 := filepath.Join(dir, "m2.sock")
	ln2, err := mgmt.ListenUnix(sock2)
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()
	srv2 := &http.Server{Handler: mgmt.LocalHandler(inner, "")}
	go srv2.Serve(ln2)
	defer srv2.Close()
	hc2 := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock2)
	}}}
	req, _ := http.NewRequest("GET", "http://twd-mgmt/v0/tenants", nil)
	res, err := hc2.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 503 || reached != 1 {
		t.Fatalf("RT-03: mgmt without a token configured admitted a request: %d", res.StatusCode)
	}
}
