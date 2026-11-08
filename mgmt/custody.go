package mgmt

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Custody holds secrets (auth keys in flight; node state / DEKs later,
// ADR-018). Names look like "twd/<tenant>/<object>/authkey".
type Custody interface {
	Put(ctx context.Context, name string, value []byte) error
	Get(ctx context.Context, name string) ([]byte, error) // ErrNotFound if absent
	Delete(ctx context.Context, name string) error        // absent is not an error
}

// FileCustody is the homelab store: one AES-256-GCM sealed file per secret
// under dir, with the key in dir/custody.key (0600). The secret name is
// bound as additional data, so a file can't be swapped for another name.
type FileCustody struct {
	dir  string
	aead cipher.AEAD
}

// OpenFileCustody opens (creating if needed) a custody dir.
func OpenFileCustody(dir string) (*FileCustody, error) {
	if err := os.MkdirAll(filepath.Join(dir, "secrets"), 0o700); err != nil {
		return nil, err
	}
	os.Chmod(dir, 0o700)
	keyPath := filepath.Join(dir, "custody.key")
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		if _, err := f.Write(key); err != nil {
			f.Close()
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if st, err := os.Stat(keyPath); err != nil || st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("custody key %s must be mode 0600", keyPath)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("custody key %s: want 32 bytes", keyPath)
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return &FileCustody{dir: dir, aead: aead}, nil
}

func (c *FileCustody) path(name string) string {
	h := sha256.Sum256([]byte(name))
	return filepath.Join(c.dir, "secrets", hex.EncodeToString(h[:16]))
}

func (c *FileCustody) Put(_ context.Context, name string, value []byte) error {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	sealed := c.aead.Seal(nonce, nonce, value, []byte(name))
	p := c.path(name)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, sealed, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (c *FileCustody) Get(_ context.Context, name string) ([]byte, error) {
	b, err := os.ReadFile(c.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	n := c.aead.NonceSize()
	if len(b) < n {
		return nil, errors.New("custody: truncated secret")
	}
	v, err := c.aead.Open(nil, b[:n], b[n:], []byte(name))
	if err != nil {
		return nil, errors.New("custody: secret failed authentication")
	}
	return v, nil
}

func (c *FileCustody) Delete(_ context.Context, name string) error {
	err := os.Remove(c.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// SetecCustody is a minimal client for a setec server (ADR-018), reached
// over the operator tailnet (HTTP is the tsnet client of tag:twd-mgmt; setec
// authorises by grants on tailscale.com/cap/secrets). Stub: put/get/delete
// of the active version only; no version pinning or caching yet.
type SetecCustody struct {
	BaseURL string // e.g. "https://secrets.<operator-tailnet>.ts.net"
	HTTP    *http.Client
}

func (c *SetecCustody) call(ctx context.Context, method string, req, resp any) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	r, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(c.BaseURL, "/")+"/api/"+method, bytes.NewReader(b))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-X-Tailscale-No-Browsers", "setec") // setec's CSRF guard
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	res, err := hc.Do(r)
	if err != nil {
		return fmt.Errorf("setec %s: %w", method, err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if res.StatusCode/100 != 2 {
		return fmt.Errorf("setec %s: %d", method, res.StatusCode) // never echo the body (may hold a value)
	}
	if resp != nil {
		return json.Unmarshal(body, resp)
	}
	return nil
}

func (c *SetecCustody) Put(ctx context.Context, name string, value []byte) error {
	var version uint64
	if err := c.call(ctx, "put", map[string]any{"Name": name, "Value": value}, &version); err != nil {
		return err
	}
	return c.call(ctx, "activate", map[string]any{"Name": name, "Version": version}, nil)
}

func (c *SetecCustody) Get(ctx context.Context, name string) ([]byte, error) {
	var out struct {
		Value   []byte
		Version uint64
	}
	if err := c.call(ctx, "get", map[string]any{"Name": name}, &out); err != nil {
		return nil, err
	}
	return out.Value, nil
}

func (c *SetecCustody) Delete(ctx context.Context, name string) error {
	err := c.call(ctx, "delete", map[string]any{"Name": name}, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}
