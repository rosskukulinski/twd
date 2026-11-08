package tsapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClient(t *testing.T) {
	secret := "tskey-" + "auth-kFAKE-" + strings.Repeat("z", 24)
	var gotCreate map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer api-token" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/v2/tailnet/t.ts.net/keys":
			json.NewDecoder(r.Body).Decode(&gotCreate)
			json.NewEncoder(w).Encode(map[string]string{"id": "k1", "key": secret})
		case r.Method == "DELETE" && r.URL.Path == "/api/v2/tailnet/t.ts.net/keys/k1":
		case r.Method == "DELETE" && r.URL.Path == "/api/v2/device/nGONE":
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"not found"}`))
		case r.Method == "DELETE" && r.URL.Path == "/api/v2/device/nX":
		case r.Method == "GET" && r.URL.Path == "/api/v2/tailnet/t.ts.net/devices":
			w.Write([]byte(`{"devices":[{"id":"1","nodeId":"nX","name":"ttest-a.t.ts.net","hostname":"ttest-a","tags":["tag:ttest"]}]}`))
		default:
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Tailnet: "t.ts.net", APIKey: "api-token"}
	ctx := context.Background()
	k, err := c.CreateKey(ctx, KeyOptions{Tags: []string{"tag:ttest"}, Ephemeral: true, Preauthorized: true, ExpirySeconds: 300})
	if err != nil || k.ID != "k1" || k.Secret != secret {
		t.Fatalf("%v %v", k, err)
	}
	if strings.Contains(k.String(), "kFAKE") {
		t.Fatal("Key.String leaks secret")
	}
	cr := gotCreate["capabilities"].(map[string]any)["devices"].(map[string]any)["create"].(map[string]any)
	if cr["ephemeral"] != true || cr["preauthorized"] != true || cr["reusable"] != false || gotCreate["expirySeconds"] != float64(300) {
		t.Fatalf("create body %v", gotCreate)
	}
	if err := c.DeleteKey(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteDevice(ctx, "nGONE"); err != nil {
		t.Fatal("404 should be nil:", err)
	}
	if err := c.DeleteDevice(ctx, "nX"); err != nil {
		t.Fatal(err)
	}
	ds, err := c.ListDevices(ctx)
	if err != nil || len(ds) != 1 || ds[0].NodeID != "nX" {
		t.Fatal(ds, err)
	}
	bad := &Client{BaseURL: srv.URL, Tailnet: "t.ts.net", APIKey: "wrong"}
	if _, err := bad.CreateKey(ctx, KeyOptions{}); err == nil || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("bad auth: %v", err)
	}
}

func TestLoadEnvFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.env")
	os.WriteFile(p, []byte("# c\nOTHER=1\nexport TS_API_KEY=\"abc\"\n"), 0o600)
	if v, err := LoadEnvFile(p, "TS_API_KEY"); err != nil || v != "abc" {
		t.Fatal(v, err)
	}
	if _, err := LoadEnvFile(p, "NOPE"); err == nil {
		t.Fatal("missing var accepted")
	}
}

func TestPolicyETagTokenExchangeRetryAfter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/tailnet/t.ts.net/acl":
			if r.Method != "GET" || r.Header.Get("Accept") != "application/json" {
				w.WriteHeader(400)
				return
			}
			if r.Header.Get("If-None-Match") == `"e1"` {
				w.WriteHeader(304)
				return
			}
			w.Header().Set("ETag", `"e1"`)
			w.Write([]byte(`{"grants":[]}`))
		case "/api/v2/oauth/token-exchange":
			r.ParseForm()
			if r.Form.Get("client_id") != "cid" || r.Form.Get("jwt") != "a.b.c" {
				w.WriteHeader(401)
				w.Write([]byte(`{"message":"bad jwt"}`))
				return
			}
			w.Write([]byte(`{"access_token":"tok-secret","token_type":"Bearer","expires_in":3600}`))
		case "/api/v2/tailnet/t.ts.net/keys":
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(429)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	c := &Client{BaseURL: srv.URL, Tailnet: "t.ts.net", APIKey: "x"}
	b, etag, nm, err := c.GetPolicy(ctx, "")
	if err != nil || etag != `"e1"` || nm || string(b) != `{"grants":[]}` {
		t.Fatal(string(b), etag, nm, err)
	}
	if _, _, nm, err := c.GetPolicy(ctx, `"e1"`); err != nil || !nm {
		t.Fatal("304", nm, err)
	}
	tr, err := ExchangeToken(ctx, nil, srv.URL, "cid", "a.b.c")
	if err != nil || tr.AccessToken != "tok-secret" || tr.ExpiresIn != 3600 || strings.Contains(tr.String(), "tok-secret") {
		t.Fatal(tr, err)
	}
	if _, err := ExchangeToken(ctx, nil, srv.URL, "cid", "bad"); err == nil || strings.Contains(err.Error(), "bad.") {
		t.Fatal(err)
	}
	_, err = c.CreateKey(ctx, KeyOptions{Tags: []string{"tag:x"}})
	var ae *APIError
	if !IsRateLimited(err) || !errors.As(err, &ae) || ae.RetryAfter != 7*time.Second {
		t.Fatalf("429: %v", err)
	}
}
