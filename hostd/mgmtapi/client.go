package mgmtapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to twd-mgmt over a unix socket ("unix:///path/mgmt.sock") or
// HTTP(S) base URL.
type Client struct {
	Base string // "http://mgmt" for unix sockets
	HTTP *http.Client
}

// NewClient returns a Client for addr: "unix:///abs/path.sock", a bare
// absolute socket path, or an http(s) URL.
func NewClient(addr string) (*Client, error) {
	switch {
	case strings.HasPrefix(addr, "unix://") || strings.HasPrefix(addr, "/"):
		path := strings.TrimPrefix(addr, "unix://")
		tr := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", path)
			},
			MaxIdleConnsPerHost: 16,
		}
		return &Client{Base: "http://mgmt", HTTP: &http.Client{Transport: tr, Timeout: 30 * time.Second}}, nil
	case strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://"):
		return &Client{Base: strings.TrimSuffix(addr, "/"), HTTP: &http.Client{Timeout: 30 * time.Second}}, nil
	}
	return nil, fmt.Errorf("mgmtapi: unsupported address %q", addr)
}

// Do sends a JSON request and decodes a JSON response into out (if non-nil).
// Non-2xx responses become *Error.
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("content-type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		e := &Error{Status: resp.StatusCode}
		if json.Unmarshal(b, e) != nil || e.Code == "" {
			e.Code, e.Message = "http_"+fmt.Sprint(resp.StatusCode), strings.TrimSpace(string(b))
		}
		return e
	}
	if out == nil {
		return nil
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = b
		return nil
	}
	return json.Unmarshal(b, out)
}

// AsError extracts a *Error.
func AsError(err error) (*Error, bool) {
	e, ok := err.(*Error)
	return e, ok
}

func (c *Client) Register(ctx context.Context, r RegisterRequest) error {
	return c.Do(ctx, "POST", "/v0/hosts/register", r, nil)
}

func (c *Client) Heartbeat(ctx context.Context, host string, r HeartbeatRequest) (*HeartbeatResponse, error) {
	var out HeartbeatResponse
	err := c.Do(ctx, "POST", "/v0/hosts/"+url.PathEscape(host)+"/heartbeat", r, &out)
	return &out, err
}

func (c *Client) Assignments(ctx context.Context, host string) ([]Assignment, error) {
	var out []Assignment
	err := c.Do(ctx, "GET", "/v0/hosts/"+url.PathEscape(host)+"/assignments", nil, &out)
	return out, err
}

func (c *Client) Tenant(ctx context.Context, id string) (*Tenant, error) {
	var out Tenant
	err := c.Do(ctx, "GET", "/v0/tenants/"+url.PathEscape(id), nil, &out)
	return &out, err
}

// Bundle fetches a deployment bundle (raw tar). bundleURL may be relative to
// the mgmt base.
func (c *Client) Bundle(ctx context.Context, bundleURL string) ([]byte, error) {
	path := bundleURL
	if u, err := url.Parse(bundleURL); err == nil && u.IsAbs() {
		if !strings.HasPrefix(bundleURL, c.Base) {
			return nil, fmt.Errorf("mgmtapi: bundle URL %q is not on mgmt", bundleURL)
		}
		path = strings.TrimPrefix(bundleURL, c.Base)
	}
	var b []byte
	err := c.Do(ctx, "GET", path, nil, &b)
	return b, err
}

func (c *Client) CreateObject(ctx context.Context, r CreateObjectRequest) (*CreateObjectResponse, error) {
	var out CreateObjectResponse
	err := c.Do(ctx, "POST", "/v0/objects", r, &out)
	return &out, err
}

func (c *Client) Credential(ctx context.Context, id string, epoch int64) (Credential, error) {
	var out Credential
	err := c.Do(ctx, "GET", fmt.Sprintf("/v0/objects/%s/credential?epoch=%d", url.PathEscape(id), epoch), nil, &out)
	return out, err
}

func (c *Client) Ready(ctx context.Context, id string, r ReadyRequest) error {
	return c.Do(ctx, "POST", "/v0/objects/"+url.PathEscape(id)+"/ready", r, nil)
}

func (c *Client) Kill(ctx context.Context, id string, r KillRequest) (*KillResponse, error) {
	var out KillResponse
	err := c.Do(ctx, "POST", "/v0/objects/"+url.PathEscape(id)+"/kill", r, &out)
	return &out, err
}

func (c *Client) Audit(ctx context.Context, recs []AuditRecord) error {
	return c.Do(ctx, "POST", "/v0/audit", AuditBatch{Records: recs}, nil)
}

// ChainHash is the audit hash of rec chained to prev:
// hex(sha256(prev || "\n" || canonical JSON of rec without hash/hostSig)).
func ChainHash(prev string, rec AuditRecord) string {
	rec.PrevHash, rec.Hash, rec.HostSig = prev, "", ""
	b, _ := json.Marshal(rec)
	h := sha256.Sum256(append([]byte(prev+"\n"), b...))
	return hex.EncodeToString(h[:])
}

// HostSig is the MVP host signature over a record hash: HMAC-SHA256 with the
// host's audit key (an Ed25519 host key replaces it with the setec custody
// work).
func HostSig(key []byte, hash string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(hash))
	return hex.EncodeToString(m.Sum(nil))
}

// VerifyChain checks prevHash links and hashes in order.
func VerifyChain(recs []AuditRecord) (bool, []string) {
	var problems []string
	prev := ""
	for i, r := range recs {
		if r.PrevHash != prev {
			problems = append(problems, fmt.Sprintf("record %d: prevHash does not link", i))
		}
		if ChainHash(r.PrevHash, r) != r.Hash {
			problems = append(problems, fmt.Sprintf("record %d: hash mismatch", i))
		}
		prev = r.Hash
	}
	return len(problems) == 0, problems
}
