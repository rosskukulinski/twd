// Package tsapi is a minimal Tailscale API v2 client for minting auth keys,
// revoking them, and deleting devices. Keys and API tokens are never logged
// or included in errors.
package tsapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// DefaultBaseURL is Tailscale's API.
const DefaultBaseURL = "https://api.tailscale.com"

// Client talks to one tailnet.
type Client struct {
	BaseURL string // default DefaultBaseURL
	Tailnet string // e.g. "tail7906d5.ts.net"; required (never "-" implicitly)
	APIKey  string
	HTTP    *http.Client
}

func (c *Client) String() string { return "tsapi.Client{" + c.Tailnet + "}" }

// LoadEnvFile reads NAME=value from a dotenv-style file without exporting it
// to the process environment.
func LoadEnvFile(path, name string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == name {
			return strings.Trim(strings.TrimSpace(v), `"'`), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("%s not found in %s", name, path)
}

// KeyOptions are the device-create capabilities of a new auth key.
type KeyOptions struct {
	Tags          []string
	Ephemeral     bool
	Reusable      bool
	Preauthorized bool
	ExpirySeconds int
	Description   string
}

// Key is a minted key. Secret must not be logged.
type Key struct {
	ID     string `json:"id"`
	Secret string `json:"key"`
}

func (k Key) String() string { return "tsapi.Key{" + k.ID + "}" }

// APIError is a non-2xx response.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("tailscale api: %d %s", e.Status, e.Message) }

// IsNotFound reports a 404.
func IsNotFound(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	if c.Tailnet == "" || c.APIKey == "" {
		return errors.New("tsapi: Tailnet and APIKey are required")
	}
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("tailscale api %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var m struct {
			Message string `json:"message"`
		}
		json.Unmarshal(b, &m)
		if m.Message == "" {
			m.Message = http.StatusText(resp.StatusCode)
		}
		return &APIError{Status: resp.StatusCode, Message: m.Message}
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (c *Client) tailnetPath(rest string) string {
	return "/api/v2/tailnet/" + url.PathEscape(c.Tailnet) + rest
}

// CreateKey mints an auth key.
func (c *Client) CreateKey(ctx context.Context, o KeyOptions) (Key, error) {
	type create struct {
		Reusable      bool     `json:"reusable"`
		Ephemeral     bool     `json:"ephemeral"`
		Preauthorized bool     `json:"preauthorized"`
		Tags          []string `json:"tags,omitempty"`
	}
	body := map[string]any{
		"capabilities": map[string]any{"devices": map[string]any{"create": create{o.Reusable, o.Ephemeral, o.Preauthorized, o.Tags}}},
	}
	if o.ExpirySeconds > 0 {
		body["expirySeconds"] = o.ExpirySeconds
	}
	if o.Description != "" {
		body["description"] = o.Description
	}
	var k Key
	if err := c.do(ctx, "POST", c.tailnetPath("/keys"), body, &k); err != nil {
		return Key{}, err
	}
	if k.ID == "" || k.Secret == "" {
		return Key{}, errors.New("tsapi: key response missing id or key")
	}
	return k, nil
}

// DeleteKey revokes a key. A 404 (already consumed/expired/revoked) is not an error.
func (c *Client) DeleteKey(ctx context.Context, id string) error {
	err := c.do(ctx, "DELETE", c.tailnetPath("/keys/"+url.PathEscape(id)), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// Device is the subset of the device API used here.
type Device struct {
	ID       string   `json:"id"`
	NodeID   string   `json:"nodeId"`
	Name     string   `json:"name"`
	Hostname string   `json:"hostname"`
	Tags     []string `json:"tags"`
}

// ListDevices lists the tailnet's devices.
func (c *Client) ListDevices(ctx context.Context) ([]Device, error) {
	var out struct {
		Devices []Device `json:"devices"`
	}
	err := c.do(ctx, "GET", c.tailnetPath("/devices"), nil, &out)
	return out.Devices, err
}

// DeleteDevice deletes a device by ID or stable node ID. A 404 is not an error.
func (c *Client) DeleteDevice(ctx context.Context, id string) error {
	err := c.do(ctx, "DELETE", "/api/v2/device/"+url.PathEscape(id), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}
