package mgmtapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	v1 "github.com/rosskukulinski/twd/hostd/mgmtapi/v1"
)

// Client talks to twd-mgmt's /v1 API over HTTP(S) (normal operation: the
// tsnet transport, authenticated by WhoIs on the connection) or over the
// local-dev unix socket ("unix:///path/mgmt.sock" + host token). Do and
// DoHeaders are the generic path-level calls (the CLI); the typed methods
// go through the generated client (V1).
type Client struct {
	Base string // "http://mgmt" for unix sockets
	HTTP *http.Client
	// HostID, if set, is sent as the Twd-Host-ID header (homelab/test
	// transports; production mgmt authenticates hosts by WhoIs instead).
	HostID string
	// TokenFile, if set, is the per-host token file mgmt minted (RT-03); its
	// content is sent as Twd-Host-Token on every request. It is re-read when
	// it changes on disk, so a rotation needs no restart.
	TokenFile string
	// Token, if set, is sent as Twd-Host-Token (tests; TokenFile wins).
	Token string

	tokMu   sync.Mutex
	tokVal  string
	tokMod  time.Time
	tokRead time.Time

	v1Once sync.Once
	v1c    *v1.ClientWithResponses
}

// HostTokenHeader is mgmt's per-host bearer token header (mgmt.HostTokenHeader).
const HostTokenHeader = "Twd-Host-Token"

// hostToken returns the current token ("" if none is configured).
func (c *Client) hostToken() string {
	if c.TokenFile == "" {
		return c.Token
	}
	c.tokMu.Lock()
	defer c.tokMu.Unlock()
	now := time.Now()
	if now.Sub(c.tokRead) < time.Second && c.tokVal != "" {
		return c.tokVal
	}
	c.tokRead = now
	st, err := os.Stat(c.TokenFile)
	if err != nil {
		return c.tokVal // keep the last one we saw (the file may be mid-rotation)
	}
	if st.ModTime().Equal(c.tokMod) && c.tokVal != "" {
		return c.tokVal
	}
	if b, err := os.ReadFile(c.TokenFile); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			c.tokVal, c.tokMod = v, st.ModTime()
		}
	}
	return c.tokVal
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
	return c.DoHeaders(ctx, method, path, in, out, nil)
}

// DoHeaders is Do with extra request headers.
func (c *Client) DoHeaders(ctx context.Context, method, path string, in, out any, hdr map[string]string) error {
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
		if method == http.MethodPatch {
			req.Header.Set("content-type", "application/merge-patch+json")
		}
	}
	c.setHeaders(req)
	for k, v := range hdr {
		req.Header.Set(k, v)
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

// V1 returns the generated /v1 client sharing this client's transport and
// headers (Twd-Host-ID, Twd-Host-Token); every typed method below goes
// through it, so URLs and parameters come from api/openapi.yaml.
func (c *Client) V1() *v1.ClientWithResponses {
	c.v1Once.Do(func() {
		hc := c.HTTP
		if hc == nil {
			hc = &http.Client{Timeout: 30 * time.Second}
		}
		c.v1c, _ = v1.NewClientWithResponses(c.Base, v1.WithHTTPClient(hc), v1.WithRequestEditorFn(func(ctx context.Context, req *http.Request) error {
			c.setHeaders(req)
			return nil
		}))
	})
	return c.v1c
}

// setHeaders adds the host identity and the local-dev token.
func (c *Client) setHeaders(req *http.Request) {
	if c.HostID != "" {
		req.Header.Set("Twd-Host-ID", c.HostID)
	}
	if tok := c.hostToken(); tok != "" {
		req.Header.Set(HostTokenHeader, tok)
	}
}

// result turns a generated response into out (any 2xx) or a typed *Error.
func result(resp *http.Response, body []byte, out any) error {
	if resp == nil {
		return errors.New("mgmtapi: no response")
	}
	if resp.StatusCode/100 != 2 {
		e := &Error{Status: resp.StatusCode}
		if json.Unmarshal(body, e) != nil || e.Code == "" {
			e.Code, e.Message = "http_"+fmt.Sprint(resp.StatusCode), strings.TrimSpace(string(body))
		}
		return e
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, out)
}

func jsonBody(v any) (io.Reader, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return bytes.NewReader(b), nil
}

// Register is PUT /v1/hosts/{id} (idempotent: 201 the first time, 200 after).
func (c *Client) Register(ctx context.Context, r RegisterRequest) error {
	// The spec's HostRegistration: push is {url} (local-dev unix socket)
	// or {port} (the host's own tsnet node); nothing else is accepted.
	reg := map[string]any{"capacityNodes": r.CapacityNodes, "tiers": r.Tiers}
	if r.AuditPubKey != "" {
		reg["auditPubKey"] = r.AuditPubKey
	}
	if r.Pool != "" {
		reg["pool"] = r.Pool
	}
	switch {
	case r.PushPort > 0:
		reg["push"] = map[string]any{"port": r.PushPort}
	case r.PushURL != "":
		reg["push"] = map[string]any{"url": r.PushURL}
	}
	body, err := jsonBody(reg)
	if err != nil {
		return err
	}
	resp, err := c.V1().RegisterHostWithBodyWithResponse(ctx, r.HostID, "application/json", body)
	if err != nil {
		return err
	}
	return result(resp.HTTPResponse, resp.Body, nil)
}

// Heartbeat is PUT /v1/hosts/{id}/status.
func (c *Client) Heartbeat(ctx context.Context, host string, r HeartbeatRequest) (*HeartbeatResponse, error) {
	body, err := jsonBody(r)
	if err != nil {
		return nil, err
	}
	resp, err := c.V1().ReportHostStatusWithBodyWithResponse(ctx, host, "application/json", body)
	if err != nil {
		return nil, err
	}
	var out HeartbeatResponse
	if err := result(resp.HTTPResponse, resp.Body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReportPaths is PUT /v1/hosts/{id}/paths (smart placement).
func (c *Client) ReportPaths(ctx context.Context, host string, reports []PathReport) error {
	body, err := jsonBody(map[string]any{"tenants": reports})
	if err != nil {
		return err
	}
	resp, err := c.V1().ReportHostPathsWithBodyWithResponse(ctx, host, "application/json", body)
	if err != nil {
		return err
	}
	return result(resp.HTTPResponse, resp.Body, nil)
}

// Assignments is GET /v1/hosts/{id}/assignments, following every page.
func (c *Client) Assignments(ctx context.Context, host string) ([]Assignment, error) {
	limit := 500
	var out []Assignment
	var cursor *string
	for {
		resp, err := c.V1().ListHostAssignmentsWithResponse(ctx, host, &v1.ListHostAssignmentsParams{Limit: &limit, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		var pg Page[Assignment]
		if err := result(resp.HTTPResponse, resp.Body, &pg); err != nil {
			return nil, err
		}
		out = append(out, pg.Items...)
		if pg.NextCursor == nil || *pg.NextCursor == "" {
			return out, nil
		}
		cursor = pg.NextCursor
	}
}

// Tenant is GET /v1/tenants/{id}.
func (c *Client) Tenant(ctx context.Context, id string) (*Tenant, error) {
	resp, err := c.V1().GetTenantWithResponse(ctx, id, &v1.GetTenantParams{})
	if err != nil {
		return nil, err
	}
	var out Tenant
	if err := result(resp.HTTPResponse, resp.Body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Tenants lists every tenant (follows every page; local admin socket only;
// used to keep every tenant's API key file out of every workerd sandbox).
func (c *Client) Tenants(ctx context.Context) ([]Tenant, error) {
	limit := 500
	var out []Tenant
	var cursor *string
	for {
		resp, err := c.V1().ListTenantsWithResponse(ctx, &v1.ListTenantsParams{Limit: &limit, Cursor: cursor})
		if err != nil {
			return nil, err
		}
		var pg Page[Tenant]
		if err := result(resp.HTTPResponse, resp.Body, &pg); err != nil {
			return nil, err
		}
		out = append(out, pg.Items...)
		if pg.NextCursor == nil || *pg.NextCursor == "" {
			return out, nil
		}
		cursor = pg.NextCursor
	}
}

// Bundle fetches a deployment bundle (raw tar). bundleURL is the
// assignment's bundleURL (/v1/deployments/{id}/bundle), possibly absolute.
func (c *Client) Bundle(ctx context.Context, bundleURL string) ([]byte, error) {
	path := bundleURL
	if u, err := url.Parse(bundleURL); err == nil && u.IsAbs() {
		if !strings.HasPrefix(bundleURL, c.Base) {
			return nil, fmt.Errorf("mgmtapi: bundle URL %q is not on mgmt", bundleURL)
		}
		path = strings.TrimPrefix(bundleURL, c.Base)
	}
	const prefix, suffix = "/v1/deployments/", "/bundle"
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		return nil, fmt.Errorf("mgmtapi: bundle URL %q is not a /v1 bundle path", bundleURL)
	}
	id, err := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix))
	if err != nil {
		return nil, err
	}
	resp, err := c.V1().GetDeploymentBundleWithResponse(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := result(resp.HTTPResponse, resp.Body, nil); err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// CreateObject is POST /v1/objects: 202 for a new object, 200 for an
// existing one (both success); a killed identity is a typed 410 "killed".
func (c *Client) CreateObject(ctx context.Context, r CreateObjectRequest) (*CreateObjectResponse, error) {
	body, err := jsonBody(r)
	if err != nil {
		return nil, err
	}
	resp, err := c.V1().CreateObjectWithBodyWithResponse(ctx, "application/json", body)
	if err != nil {
		return nil, err
	}
	var out CreateObjectResponse
	if err := result(resp.HTTPResponse, resp.Body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Credential is POST /v1/objects/{id}/credentials?epoch=N (201): a
// single-use key, released only to the lease holder at epoch.
func (c *Client) Credential(ctx context.Context, id string, epoch int64) (Credential, error) {
	return c.credential(ctx, id, epoch, nil)
}

func (c *Client) credential(ctx context.Context, id string, epoch int64, reason *v1.CreateObjectCredentialParamsReason) (Credential, error) {
	wait := 10
	resp, err := c.V1().CreateObjectCredentialWithResponse(ctx, id, &v1.CreateObjectCredentialParams{Epoch: epoch, Wait: &wait, Reason: reason})
	if err != nil {
		return Credential{}, err
	}
	var out Credential
	err = result(resp.HTTPResponse, resp.Body, &out)
	return out, err
}

// Remint is POST /v1/objects/{id}/credentials?epoch=N&reason=rejoin_rejected:
// the saved node state was rejected by control; mgmt deletes the old
// device, clears the certificate and mints again under the same hostname.
// The new single-use key is returned.
func (c *Client) Remint(ctx context.Context, id string, r RemintRequest) (Credential, error) {
	reason := v1.CreateObjectCredentialParamsReason("rejoin_rejected")
	return c.credential(ctx, id, r.Epoch, &reason)
}

// VolumeKey is POST /v1/volumes/{tenant}:{workload}/keys?epoch=N (201): the
// (tenant, workload) DO volume's data key, released only to the lease
// holder at epoch.
func (c *Client) VolumeKey(ctx context.Context, tenantID, workload string, epoch int64) ([]byte, error) {
	resp, err := c.V1().CreateVolumeKeyGrantWithResponse(ctx, tenantID+":"+workload, &v1.CreateVolumeKeyGrantParams{Epoch: epoch})
	if err != nil {
		return nil, err
	}
	var out VolumeKey
	if err := result(resp.HTTPResponse, resp.Body, &out); err != nil {
		return nil, err
	}
	return out.Key, nil
}

// Ready is PATCH /v1/objects/{id} {"status":"ready", …} (merge patch).
func (c *Client) Ready(ctx context.Context, id string, r ReadyRequest) error {
	body, err := jsonBody(map[string]any{"status": "ready", "nodeID": r.NodeID, "stableNodeID": r.StableNodeID, "magicDNS": r.MagicDNS, "epoch": r.Epoch})
	if err != nil {
		return err
	}
	resp, err := c.V1().UpdateObjectWithBodyWithResponse(ctx, id, &v1.UpdateObjectParams{}, "application/merge-patch+json", body)
	if err != nil {
		return err
	}
	return result(resp.HTTPResponse, resp.Body, nil)
}

// Kill is DELETE /v1/objects/{id}?cascade=&reason= (202 KillResult).
func (c *Client) Kill(ctx context.Context, id string, r KillRequest) (*KillResponse, error) {
	params := &v1.DeleteObjectParams{Cascade: &r.Cascade}
	if r.Reason != "" {
		params.Reason = &r.Reason
	}
	resp, err := c.V1().DeleteObjectWithResponse(ctx, id, params)
	if err != nil {
		return nil, err
	}
	var out KillResponse
	if err := result(resp.HTTPResponse, resp.Body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Audit is POST /v1/audit (201).
func (c *Client) Audit(ctx context.Context, recs []AuditRecord) error {
	body, err := jsonBody(AuditBatch{Records: recs})
	if err != nil {
		return err
	}
	resp, err := c.V1().AppendAuditWithBodyWithResponse(ctx, "application/json", body)
	if err != nil {
		return err
	}
	return result(resp.HTTPResponse, resp.Body, nil)
}

// NodeCert is GET /v1/certificates/{stableNodeID}?tenant= (RT-01): the
// certificate of the tenant's object whose node is stableNodeID. A 404
// means the node is not a twd object of that tenant (ErrNoCert).
func (c *Client) NodeCert(ctx context.Context, tenantID, stableNodeID string) (*NodeCert, error) {
	resp, err := c.V1().GetCertificateWithResponse(ctx, stableNodeID, &v1.GetCertificateParams{Tenant: tenantID})
	if err != nil {
		return nil, err
	}
	var out NodeCert
	err = result(resp.HTTPResponse, resp.Body, &out)
	if e, ok := AsError(err); ok && (e.Status == 404 || e.Code == CodeNotFound) {
		return nil, ErrNoCert
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ErrNoCert: the node is not a (live) twd object of the tenant, so no
// certificate exists and no lim applies.
var ErrNoCert = errors.New("mgmtapi: no node certificate for that node")

// ChainHash is the audit hash of rec chained to prev (the contract's
// AuditRecord): body = the record JSON without "hash" and "hostSig"
// (including "prevHash"), keys sorted, no whitespace, no HTML escaping;
// hash = lowercase hex(SHA-256(body)).
func ChainHash(prev string, rec AuditRecord) string {
	rec.PrevHash = prev
	b, _ := json.Marshal(rec)
	c, err := Canonicalize(b, "hash", "hostSig")
	if err != nil {
		return ""
	}
	h := sha256.Sum256(c)
	return hex.EncodeToString(h[:])
}

// Canonicalize re-encodes a JSON object with sorted keys, dropping keys.
func Canonicalize(raw []byte, drop ...string) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	for _, k := range drop {
		delete(m, k)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// HostSig is base64(ed25519.Sign(key, utf8(hash))).
func HostSig(key ed25519.PrivateKey, hash string) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(key, []byte(hash)))
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
