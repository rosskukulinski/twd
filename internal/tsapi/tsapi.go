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
	"strconv"
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
	// Tokens, if set, supplies the bearer token instead of APIKey (an
	// OAuth client's access token). A 401 invalidates it and the request
	// is retried once with a fresh token.
	Tokens TokenSource
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
	Status     int
	Message    string
	RetryAfter time.Duration // from Retry-After on 429/503, if present
}

// IsRateLimited reports a 429.
func IsRateLimited(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusTooManyRequests
}

func retryAfter(h http.Header) time.Duration {
	v := h.Get("Retry-After")
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return time.Until(t)
	}
	return 0
}

func (e *APIError) Error() string { return fmt.Sprintf("tailscale api: %d %s", e.Status, e.Message) }

// IsNotFound reports a 404.
func IsNotFound(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	_, err := c.doHdr(ctx, method, path, nil, body, out)
	return err
}

// doHdr is do with extra request headers; it returns the response headers.
// out may be *[]byte to receive the raw body.
func (c *Client) doHdr(ctx context.Context, method, path string, hdr http.Header, body, out any) (http.Header, error) {
	if c.Tailnet == "" || (c.APIKey == "" && c.Tokens == nil) {
		return nil, errors.New("tsapi: Tailnet and APIKey (or Tokens) are required")
	}
	var bodyBytes []byte
	ctype := "application/json"
	if rb, ok := body.(RawBody); ok {
		bodyBytes, ctype = rb.Body, rb.ContentType
	} else if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyBytes = b
	}
	for attempt := 0; ; attempt++ {
		bearer := c.APIKey
		if c.Tokens != nil {
			t, err := c.Tokens.Token(ctx)
			if err != nil {
				return nil, err
			}
			bearer = t
		}
		rh, err := c.send(ctx, method, path, hdr, body != nil, bodyBytes, ctype, bearer, out)
		if c.Tokens != nil && attempt == 0 && IsUnauthorized(err) {
			// The access token expired or was revoked early: exchange once more.
			c.Tokens.Invalidate()
			continue
		}
		return rh, err
	}
}

func (c *Client) send(ctx context.Context, method, path string, hdr http.Header, hasBody bool, bodyBytes []byte, ctype, bearer string, out any) (http.Header, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	var rd io.Reader
	if hasBody {
		rd = bytes.NewReader(bodyBytes)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if hasBody {
		req.Header.Set("Content-Type", ctype)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tailscale api %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotModified {
		return resp.Header, nil
	}
	if resp.StatusCode/100 != 2 {
		var m struct {
			Message string `json:"message"`
		}
		json.Unmarshal(b, &m)
		if m.Message == "" {
			m.Message = http.StatusText(resp.StatusCode)
		}
		return resp.Header, &APIError{Status: resp.StatusCode, Message: m.Message, RetryAfter: retryAfter(resp.Header)}
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = b
		return resp.Header, nil
	}
	if out != nil {
		return resp.Header, json.Unmarshal(b, out)
	}
	return resp.Header, nil
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

// GetPolicy reads the tailnet policy file as JSON (read-only; twd never
// writes policy). With etag set it sends If-None-Match and reports
// notModified on a 304.
func (c *Client) GetPolicy(ctx context.Context, etag string) (body []byte, newETag string, notModified bool, err error) {
	h := http.Header{"Accept": {"application/json"}}
	if etag != "" {
		h.Set("If-None-Match", etag)
	}
	var raw []byte
	rh, err := c.doHdr(ctx, "GET", c.tailnetPath("/acl"), h, nil, &raw)
	if err != nil {
		return nil, "", false, err
	}
	if raw == nil && etag != "" {
		return nil, etag, true, nil
	}
	return raw, rh.Get("ETag"), false, nil
}

// TokenResponse is an OAuth access token. AccessToken must not be logged.
type TokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

func (t TokenResponse) String() string { return "tsapi.TokenResponse{[REDACTED]}" }

// ExchangeToken exchanges a workload-identity JWT for an API access token
// (POST /api/v2/oauth/token-exchange, form client_id + jwt).
func ExchangeToken(ctx context.Context, hc *http.Client, baseURL, clientID, idToken string) (TokenResponse, error) {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	form := url.Values{"client_id": {clientID}, "jwt": {idToken}}
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/api/v2/oauth/token-exchange", strings.NewReader(form.Encode()))
	if err != nil {
		return TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		return TokenResponse{}, fmt.Errorf("token exchange: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var m struct {
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		json.Unmarshal(b, &m)
		msg := m.Message
		if msg == "" {
			msg = m.Error
		}
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return TokenResponse{}, &APIError{Status: resp.StatusCode, Message: "token exchange: " + msg, RetryAfter: retryAfter(resp.Header)}
	}
	var tr TokenResponse
	if err := json.Unmarshal(b, &tr); err != nil {
		return TokenResponse{}, err
	}
	if tr.AccessToken == "" {
		return TokenResponse{}, errors.New("token exchange: no access_token")
	}
	return tr, nil
}

// GetDevice fetches one device by ID / stable node ID.
func (c *Client) GetDevice(ctx context.Context, id string) (Device, error) {
	var d Device
	err := c.do(ctx, "GET", "/api/v2/device/"+url.PathEscape(id), nil, &d)
	return d, err
}

// FederatedOptions describes a workload-identity-federation trust credential
// (POST /keys with keyType "federated"; needs the federated_keys scope).
type FederatedOptions struct {
	Scopes      []string
	Tags        []string
	Issuer      string
	Subject     string
	Audience    string // optional; Tailscale generates api.tailscale.com/<clientID>
	Description string
}

// FederatedCredential is a created trust credential. ID is the client ID
// used at token exchange.
type FederatedCredential struct {
	ID       string `json:"id"`
	Audience string `json:"audience"`
}

// CreateFederated creates a trust credential. It holds no secret.
func (c *Client) CreateFederated(ctx context.Context, o FederatedOptions) (FederatedCredential, error) {
	body := map[string]any{"keyType": "federated", "scopes": o.Scopes, "tags": o.Tags, "issuer": o.Issuer, "subject": o.Subject}
	if o.Audience != "" {
		body["audience"] = o.Audience
	}
	if o.Description != "" {
		body["description"] = o.Description
	}
	var fc FederatedCredential
	if err := c.do(ctx, "POST", c.tailnetPath("/keys"), body, &fc); err != nil {
		return fc, err
	}
	if fc.ID == "" {
		return fc, errors.New("tsapi: federated credential response missing id")
	}
	return fc, nil
}

// RawBody is a request body sent as-is (e.g. a HuJSON policy file).
type RawBody struct {
	Body        []byte
	ContentType string
}

// GetPolicyHuJSON reads the policy file as HuJSON (comments kept) with its ETag.
func (c *Client) GetPolicyHuJSON(ctx context.Context) (body []byte, etag string, err error) {
	var raw []byte
	rh, err := c.doHdr(ctx, "GET", c.tailnetPath("/acl"), http.Header{"Accept": {"application/hujson"}}, nil, &raw)
	if err != nil {
		return nil, "", err
	}
	return raw, rh.Get("ETag"), nil
}

// ValidatePolicy asks the control plane to validate a policy file (HuJSON)
// without applying it. A non-nil error carries the server's message.
func (c *Client) ValidatePolicy(ctx context.Context, hujsonBody []byte) error {
	var raw []byte
	if _, err := c.doHdr(ctx, "POST", c.tailnetPath("/acl/validate"), nil, RawBody{hujsonBody, "application/hujson"}, &raw); err != nil {
		return err
	}
	var m struct {
		Message string `json:"message"`
		Data    []struct {
			User   string   `json:"user"`
			Errors []string `json:"errors"`
		} `json:"data"`
	}
	if len(bytes.TrimSpace(raw)) > 0 && json.Unmarshal(raw, &m) == nil && m.Message != "" {
		msg := m.Message
		for _, d := range m.Data {
			msg += "; " + strings.Join(d.Errors, "; ")
		}
		return &APIError{Status: 400, Message: "policy invalid: " + msg}
	}
	return nil
}

// SetPolicy replaces the policy file, only if it still has ETag ifMatch
// (412 otherwise). It returns the new ETag. Callers must validate first.
func (c *Client) SetPolicy(ctx context.Context, hujsonBody []byte, ifMatch string) (string, error) {
	if ifMatch == "" {
		return "", errors.New("tsapi: SetPolicy requires an If-Match ETag")
	}
	var raw []byte
	rh, err := c.doHdr(ctx, "POST", c.tailnetPath("/acl"), http.Header{"If-Match": {ifMatch}, "Accept": {"application/hujson"}},
		RawBody{hujsonBody, "application/hujson"}, &raw)
	if err != nil {
		return "", err
	}
	return rh.Get("ETag"), nil
}

// KeyInfo is the metadata of an auth key (never the secret).
type KeyInfo struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Invalid     bool   `json:"invalid"`
	Revoked     string `json:"revoked,omitempty"`
	Expires     string `json:"expires,omitempty"`
}

// GetKey reads a key's metadata. A 404 means it no longer exists.
func (c *Client) GetKey(ctx context.Context, id string) (KeyInfo, error) {
	var k KeyInfo
	err := c.do(ctx, "GET", c.tailnetPath("/keys/"+url.PathEscape(id)), nil, &k)
	return k, err
}
