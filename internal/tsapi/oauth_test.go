package tsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/netd"
)

// secretFor builds a client secret at runtime (no key-shaped literal is
// committed).
func secretFor(id string) string { return "tskey-" + "client-" + id + "-" + strings.Repeat("Z", 24) }

func writeCreds(t *testing.T, body string, perm os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "oauth.env")
	if err := os.WriteFile(p, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, perm)
	return p
}

func TestLoadOAuthFile(t *testing.T) {
	sec := secretFor("kAbc123CNTRL")
	ok := writeCreds(t, "# twd\nTS_OAUTH_CLIENT_ID=kAbc123CNTRL\nexport TS_OAUTH_CLIENT_SECRET=\""+sec+"\"\n", 0o600)
	c, err := LoadOAuthFile(ok)
	if err != nil || c.ClientID != "kAbc123CNTRL" || c.Secret != sec {
		t.Fatalf("load: %v %v", c, err)
	}
	// The ID is derived from the secret when absent.
	c, err = LoadOAuthFile(writeCreds(t, "TS_OAUTH_CLIENT_SECRET="+sec+"\n", 0o600))
	if err != nil || c.ClientID != "kAbc123CNTRL" {
		t.Fatalf("derived id: %v %v", c, err)
	}
	for name, tc := range map[string]struct {
		body string
		perm os.FileMode
	}{
		"group-readable": {"TS_OAUTH_CLIENT_SECRET=" + sec, 0o640},
		"world-readable": {"TS_OAUTH_CLIENT_SECRET=" + sec, 0o644},
		"no-secret":      {"TS_OAUTH_CLIENT_ID=kAbc123CNTRL", 0o600},
		"api-key":        {"TS_OAUTH_CLIENT_SECRET=tskey-" + "api-kX-" + strings.Repeat("Z", 24), 0o600},
		"id-mismatch":    {"TS_OAUTH_CLIENT_ID=kOther\nTS_OAUTH_CLIENT_SECRET=" + sec, 0o600},
	} {
		_, err := LoadOAuthFile(writeCreds(t, tc.body, tc.perm))
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if strings.Contains(err.Error(), "ZZZZZZZZ") {
			t.Fatalf("%s: the error carries the secret: %v", name, err)
		}
		if (tc.perm&0o077 != 0) != IsInsecureFile(err) {
			t.Fatalf("%s: IsInsecureFile=%v for %v", name, IsInsecureFile(err), err)
		}
	}
	// Formatting never prints the secret.
	for _, f := range []string{"%v", "%+v", "%#v", "%s"} {
		if out := fmt.Sprintf(f, c); strings.Contains(out, "ZZZZ") {
			t.Fatalf("%s prints the secret: %s", f, out)
		}
	}
	if netd.Redact("secret="+sec) != "secret=tskey-[REDACTED]" {
		t.Fatalf("redaction misses tskey-client-: %s", netd.Redact(sec))
	}
}

// fakeOAuthAPI: /oauth/token issues numbered tokens; /keys accepts only
// the current token (an earlier one answers 401, as after an early
// revocation).
type fakeOAuthAPI struct {
	mu        sync.Mutex
	secret    string
	exchanges int
	valid     map[string]bool
	alwaysBad bool // every API call answers 401
	expiresIn int
	bodies    []string
}

func (f *fakeOAuthAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/api/v2/oauth/token" {
		r.ParseForm()
		if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_secret") != f.secret || r.Form.Get("client_id") == "" {
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"invalid_client","error_description":"bad credentials"}`))
			return
		}
		f.exchanges++
		tok := fmt.Sprintf("tok-%d", f.exchanges)
		f.valid = map[string]bool{tok: true}
		json.NewEncoder(w).Encode(map[string]any{"access_token": tok, "expires_in": f.expiresIn})
		return
	}
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if f.alwaysBad || !f.valid[auth] {
		w.WriteHeader(401)
		return
	}
	var b strings.Builder
	fmt.Fprint(&b, r.Method, " ", r.URL.Path)
	f.bodies = append(f.bodies, b.String())
	json.NewEncoder(w).Encode(map[string]string{"id": "k1", "key": "tskey-" + "auth-k1-" + strings.Repeat("x", 20)})
}

func TestOAuthSourceCacheRefreshRetry(t *testing.T) {
	sec := secretFor("kId1CNTRL")
	path := writeCreds(t, "TS_OAUTH_CLIENT_ID=kId1CNTRL\nTS_OAUTH_CLIENT_SECRET="+sec+"\n", 0o600)
	f := &fakeOAuthAPI{secret: sec, expiresIn: 3600}
	srv := httptest.NewServer(f)
	defer srv.Close()
	now := time.Unix(1_700_000_000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	src := &OAuthSource{BaseURL: srv.URL, Now: clock, Load: func() (OAuthCredentials, error) { return LoadOAuthFile(path) }}
	c := &Client{BaseURL: srv.URL, Tailnet: "-", Tokens: src}
	ctx := context.Background()
	mint := func() {
		t.Helper()
		if _, err := c.CreateKey(ctx, KeyOptions{Tags: []string{"tag:twd"}}); err != nil {
			t.Fatal(err)
		}
	}
	mint()
	mint()
	if f.exchanges != 1 {
		t.Fatalf("cached: %d exchanges", f.exchanges)
	}
	// Near expiry (90% of the hour): a fresh exchange.
	mu.Lock()
	now = now.Add(55 * time.Minute)
	mu.Unlock()
	mint()
	if f.exchanges != 2 {
		t.Fatalf("refresh near expiry: %d exchanges", f.exchanges)
	}
	// Revoked early (401): one retry with a fresh token.
	f.mu.Lock()
	f.valid = map[string]bool{}
	f.mu.Unlock()
	mint()
	if f.exchanges != 3 {
		t.Fatalf("401 retry: %d exchanges", f.exchanges)
	}
	// A persistent 401: exactly one retry, then the error.
	f.mu.Lock()
	f.alwaysBad = true
	f.mu.Unlock()
	_, err := c.CreateKey(ctx, KeyOptions{Tags: []string{"tag:twd"}})
	if !IsUnauthorized(err) || f.exchanges != 4 {
		t.Fatalf("persistent 401: %v after %d exchanges (want 4)", err, f.exchanges)
	}
	// A wrong secret: the exchange fails, the secret is not in the error.
	os.WriteFile(path, []byte("TS_OAUTH_CLIENT_SECRET="+secretFor("kId1CNTRL")+"X\n"), 0o600)
	src.Invalidate()
	_, err = src.Token(ctx)
	if err == nil || strings.Contains(err.Error(), "ZZZZ") || !strings.Contains(err.Error(), "oauth token") {
		t.Fatalf("bad secret: %v", err)
	}
	// The file is re-read at each exchange: a mode loosened later is refused.
	os.WriteFile(path, []byte("TS_OAUTH_CLIENT_SECRET="+sec+"\n"), 0o600)
	os.Chmod(path, 0o604)
	src.Invalidate()
	if _, err := src.Token(ctx); !IsInsecureFile(err) {
		t.Fatalf("loosened mode: %v", err)
	}
}

func TestIsTagsNotPermitted(t *testing.T) {
	for msg, want := range map[string]bool{
		"tailscale api: 400 requested tags [tag:twd-mgmt] are invalid or not permitted": true,
		"tailscale api: 403 insufficient scope":                                         false,
		"tailscale api: 429 rate limited":                                               false,
	} {
		if got := IsTagsNotPermitted(fmt.Errorf("%s", msg)); got != want {
			t.Fatalf("%q: %v", msg, got)
		}
	}
	if ClientIDFromSecret(secretFor("kX9")) != "kX9" || ClientIDFromSecret("nope") != "" {
		t.Fatal("ClientIDFromSecret")
	}
}
