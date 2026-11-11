package mgmt

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestMM_F11b_SharedIssuer (H13): every replica serves the same WIF issuer
// signing key; the first replica's existing issuer.key is kept.
func TestMM_F11b_SharedIssuer(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := OpenStore(ctx, filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cust, _ := OpenFileCustody(filepath.Join(dir, "c"))
	jwks := func(is *Issuer) string { b, _ := json.Marshal(is.JWKS()); return string(b) }

	// Concurrent first start of 4 replicas (fresh state dirs): one key.
	const url = "https://twd-mgmt.example.ts.net"
	got := make([]string, 4)
	var wg sync.WaitGroup
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			is, err := LoadSharedIssuer(ctx, url, filepath.Join(t.TempDir(), "issuer.key"), st, cust)
			if err != nil {
				t.Error(err)
				return
			}
			got[i] = jwks(is)
		}()
	}
	wg.Wait()
	for i := 1; i < len(got); i++ {
		if got[i] != got[0] {
			t.Fatalf("replica %d serves a different issuer key", i)
		}
	}
	// A restart (any state dir) loads the same key.
	if is, _ := LoadSharedIssuer(ctx, url, filepath.Join(t.TempDir(), "issuer.key"), st, cust); jwks(is) != got[0] {
		t.Fatal("a restarted replica must load the chosen key")
	}

	// Migration: the first replica for a new issuer URL has an existing
	// issuer.key (trusted by Tailscale already): that key is kept and shared.
	const url2 = "https://other.example.ts.net"
	oldDir := t.TempDir()
	old, err := LoadOrCreateIssuer(url2, filepath.Join(oldDir, "issuer.key"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := LoadSharedIssuer(ctx, url2, filepath.Join(oldDir, "issuer.key"), st, cust)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadSharedIssuer(ctx, url2, filepath.Join(t.TempDir(), "issuer.key"), st, cust)
	if err != nil {
		t.Fatal(err)
	}
	if jwks(first) != jwks(old) || jwks(second) != jwks(old) {
		t.Fatal("the existing issuer.key must become the shared key")
	}
	if _, err := os.Stat(filepath.Join(oldDir, "issuer.key")); err != nil {
		t.Fatal("the existing key file must be left in place")
	}
}
