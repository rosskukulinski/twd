package tsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
