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

// TestMM_F12_SharedIDSecret (H14): replicas share the object-ID secret; the
// first replica's existing id.secret is kept.
func TestMM_F12_SharedIDSecret(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := OpenStore(ctx, filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cust, _ := OpenFileCustody(filepath.Join(dir, "c"))
	first := filepath.Join(t.TempDir(), "id.secret")
	LoadOrCreateSecret(first) // replica 0 has a pre-F12 secret
	var wg sync.WaitGroup
	got := make([][]byte, 4)
	errs := make([]error, 4)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := filepath.Join(t.TempDir(), "id.secret")
			if i == 0 {
				p = first
			}
			got[i], errs[i] = LoadSharedIDSecret(ctx, p, st, cust)
		}()
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("replica %d: %v", i, e)
		}
	}
	for i := 1; i < len(got); i++ {
		if string(got[i]) != string(got[0]) {
			t.Fatalf("replica %d got a different ID secret", i)
		}
	}
	// Run again from a fresh dir: the chosen secret, whichever won.
	again, _ := LoadSharedIDSecret(ctx, filepath.Join(t.TempDir(), "id.secret"), st, cust)
	if string(again) != string(got[0]) {
		t.Fatal("a later replica must load the chosen secret")
	}
}

// TestMM_F12_ExistingFileAdopted: a single pre-F12 replica restarting with
// F12 keeps its secret (object IDs it issued stay derivable).
func TestMM_F12_ExistingFileAdopted(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, _ := OpenStore(ctx, filepath.Join(dir, "m.db"))
	defer st.Close()
	cust, _ := OpenFileCustody(filepath.Join(dir, "c"))
	p := filepath.Join(dir, "id.secret")
	orig, _ := LoadOrCreateSecret(p)
	got, err := LoadSharedIDSecret(ctx, p, st, cust)
	if err != nil || string(got) != string(orig) {
		t.Fatalf("existing id.secret must be adopted: %v", err)
	}
}
