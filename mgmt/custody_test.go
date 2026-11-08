package mgmt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func testCustody(t *testing.T, c Custody) {
	ctx := context.Background()
	secret := []byte("value-" + "kFAKEsecret")
	if err := c.Put(ctx, "twd/tn/o1/authkey", secret); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(ctx, "twd/tn/o1/authkey")
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("get %q %v", got, err)
	}
	if _, err := c.Get(ctx, "twd/tn/nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	if err := c.Delete(ctx, "twd/tn/o1/authkey"); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "twd/tn/o1/authkey"); err != nil {
		t.Fatal("double delete:", err)
	}
	if _, err := c.Get(ctx, "twd/tn/o1/authkey"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestFileCustody(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "custody")
	c, err := OpenFileCustody(dir)
	if err != nil {
		t.Fatal(err)
	}
	testCustody(t, c)
	st, _ := os.Stat(filepath.Join(dir, "custody.key"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", st.Mode())
	}
	// Ciphertext at rest, bound to the name.
	ctx := context.Background()
	c.Put(ctx, "a", []byte("plaintext-marker"))
	b, _ := os.ReadFile(c.path("a"))
	if bytes.Contains(b, []byte("plaintext-marker")) {
		t.Fatal("secret stored in plaintext")
	}
	os.WriteFile(c.path("b"), b, 0o600) // swap a's file in as b
	if _, err := c.Get(ctx, "b"); err == nil {
		t.Fatal("swapped ciphertext accepted under another name")
	}
	b[len(b)-1] ^= 1
	os.WriteFile(c.path("a"), b, 0o600)
	if _, err := c.Get(ctx, "a"); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
	// Reopen reuses the key.
	c2, err := OpenFileCustody(dir)
	if err != nil {
		t.Fatal(err)
	}
	c2.Put(ctx, "x", []byte("1"))
	if v, err := c.Get(ctx, "x"); err != nil || string(v) != "1" {
		t.Fatal(v, err)
	}
	// A world-readable key is refused.
	os.Chmod(filepath.Join(dir, "custody.key"), 0o644)
	if _, err := OpenFileCustody(dir); err == nil {
		t.Fatal("0644 key accepted")
	}
}

// fakeSetec mimics setec's /api/{put,activate,get,delete}.
func fakeSetec(t *testing.T) *httptest.Server {
	var mu sync.Mutex
	vals := map[string][]byte{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Sec-X-Tailscale-No-Browsers") != "setec" {
			w.WriteHeader(403)
			return
		}
		var req struct {
			Name    string
			Value   []byte
			Version uint64
		}
		json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/put":
			vals[req.Name] = req.Value
			json.NewEncoder(w).Encode(1)
		case "/api/activate":
		case "/api/get":
			v, ok := vals[req.Name]
			if !ok {
				http.Error(w, "not found", 404)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"Value": v, "Version": 1})
		case "/api/delete":
			if _, ok := vals[req.Name]; !ok {
				http.Error(w, "not found", 404)
				return
			}
			delete(vals, req.Name)
		default:
			w.WriteHeader(500)
		}
	}))
}

func TestSetecCustody(t *testing.T) {
	srv := fakeSetec(t)
	defer srv.Close()
	testCustody(t, &SetecCustody{BaseURL: srv.URL})
}
