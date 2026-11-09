package tsapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// OAuth client support (M12): a Tailscale OAuth client (scope auth_keys,
// tag tag:twd; optionally devices:core and policy_file:read) is exchanged
// for a short-lived API access token with the client-credentials grant
// (POST /api/v2/oauth/token). The secret never expires and lives only in a
// 0600 file on the host; it is re-read at every exchange and never logged.

// OAuthCredentials is a client ID and secret. Secret must not be logged.
type OAuthCredentials struct {
	ClientID string
	Secret   string
}

func (c OAuthCredentials) String() string {
	return "tsapi.OAuthCredentials{" + c.ClientID + ", [REDACTED]}"
}

// GoString keeps %#v from printing the secret.
func (c OAuthCredentials) GoString() string { return c.String() }

// InsecureFileError means a credential file is readable or writable by
// other users (mode & 0o077 != 0).
type InsecureFileError struct {
	Path string
	Mode os.FileMode
}

func (e *InsecureFileError) Error() string {
	return fmt.Sprintf("%s has mode %v: an OAuth client secret file must be readable by its owner only (chmod 600 %s)", e.Path, e.Mode.Perm(), e.Path)
}

// IsInsecureFile reports an InsecureFileError.
func IsInsecureFile(err error) bool {
	var e *InsecureFileError
	return errors.As(err, &e)
}

// OAuthSecretPrefix starts every Tailscale OAuth client secret.
const OAuthSecretPrefix = "tskey-client-"

// LoadOAuthFile reads TS_OAUTH_CLIENT_ID / TS_OAUTH_CLIENT_SECRET from a
// dotenv-style file. It refuses a file whose mode grants any access to
// group or others. TS_OAUTH_CLIENT_ID may be omitted: it is the third
// segment of the secret (tskey-client-<id>-<random>).
func LoadOAuthFile(path string) (OAuthCredentials, error) {
	st, err := os.Stat(path)
	if err != nil {
		return OAuthCredentials{}, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return OAuthCredentials{}, &InsecureFileError{Path: path, Mode: st.Mode()}
	}
	f, err := os.Open(path)
	if err != nil {
		return OAuthCredentials{}, err
	}
	defer f.Close()
	var c OAuthCredentials
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimPrefix(strings.TrimSpace(sc.Text()), "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "TS_OAUTH_CLIENT_ID":
			c.ClientID = v
		case "TS_OAUTH_CLIENT_SECRET":
			c.Secret = v
		}
	}
	if err := sc.Err(); err != nil {
		return OAuthCredentials{}, err
	}
	if c.Secret == "" {
		return OAuthCredentials{}, fmt.Errorf("TS_OAUTH_CLIENT_SECRET not found in %s", path)
	}
	if !strings.HasPrefix(c.Secret, OAuthSecretPrefix) {
		return OAuthCredentials{}, fmt.Errorf("TS_OAUTH_CLIENT_SECRET in %s is not an OAuth client secret (it must start with %s)", path, OAuthSecretPrefix)
	}
	if id := ClientIDFromSecret(c.Secret); c.ClientID == "" {
		c.ClientID = id
	} else if id != "" && id != c.ClientID {
		return OAuthCredentials{}, fmt.Errorf("TS_OAUTH_CLIENT_ID in %s does not match the client ID inside TS_OAUTH_CLIENT_SECRET", path)
	}
	if c.ClientID == "" {
		return OAuthCredentials{}, fmt.Errorf("TS_OAUTH_CLIENT_ID not found in %s", path)
	}
	return c, nil
}

// ClientIDFromSecret extracts <id> from tskey-client-<id>-<random> ("" if
// the secret has another shape).
func ClientIDFromSecret(secret string) string {
	rest, ok := strings.CutPrefix(secret, OAuthSecretPrefix)
	if !ok {
		return ""
	}
	id, _, ok := strings.Cut(rest, "-")
	if !ok {
		return ""
	}
	return id
}

// OAuthToken does the client-credentials grant: POST
// {baseURL}/api/v2/oauth/token with client_id and client_secret.
func OAuthToken(ctx context.Context, hc *http.Client, baseURL string, c OAuthCredentials) (TokenResponse, error) {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {c.ClientID}, "client_secret": {c.Secret}}
	req, err := http.NewRequestWithContext(ctx, "POST", baseURL+"/api/v2/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := hc.Do(req)
	if err != nil {
		// The transport error names the URL, never the form body.
		return TokenResponse{}, fmt.Errorf("oauth token: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		var m struct {
			Message string `json:"message"`
			Error   string `json:"error"`
			Desc    string `json:"error_description"`
		}
		json.Unmarshal(b, &m)
		msg := m.Message
		if msg == "" {
			msg = strings.TrimSpace(m.Error + " " + m.Desc)
		}
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return TokenResponse{}, &APIError{Status: resp.StatusCode, Message: "oauth token: " + msg, RetryAfter: retryAfter(resp.Header)}
	}
	var tr TokenResponse
	if err := json.Unmarshal(b, &tr); err != nil {
		return TokenResponse{}, fmt.Errorf("oauth token: bad response: %w", err)
	}
	if tr.AccessToken == "" {
		return TokenResponse{}, errors.New("oauth token: no access_token")
	}
	return tr, nil
}

// TokenSource supplies bearer tokens to a Client. Invalidate drops a cached
// token after the API answered 401, so the next Token exchanges afresh.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
	Invalidate()
}

// OAuthSource caches an OAuth access token until shortly before it
// expires. Load is called at every exchange (the secret is re-read from
// its file, so a rotated secret or a tightened mode takes effect without a
// restart, and the secret is not kept between exchanges).
type OAuthSource struct {
	BaseURL string
	HTTP    *http.Client
	Load    func() (OAuthCredentials, error)
	Now     func() time.Time // default time.Now

	mu        sync.Mutex
	tok       string
	exp       time.Time
	Exchanges int // how many token exchanges ran (tests, doctor)
}

func (s *OAuthSource) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Token returns the cached access token or exchanges for a new one.
func (s *OAuthSource) Token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if s.tok != "" && now.Before(s.exp) {
		return s.tok, nil
	}
	c, err := s.Load()
	if err != nil {
		return "", err
	}
	tr, err := OAuthToken(ctx, s.HTTP, s.BaseURL, c)
	if err != nil {
		return "", err
	}
	life := time.Duration(tr.ExpiresIn) * time.Second
	if life <= 0 {
		life = 5 * time.Minute
	}
	// Refresh at 90% of the lifetime (and at least 30 s before expiry).
	early := min(life/10, 5*time.Minute)
	early = max(early, min(30*time.Second, life/2))
	s.tok, s.exp = tr.AccessToken, now.Add(life-early)
	s.Exchanges++
	return s.tok, nil
}

// Invalidate drops the cached token.
func (s *OAuthSource) Invalidate() {
	s.mu.Lock()
	s.tok, s.exp = "", time.Time{}
	s.mu.Unlock()
}

// IsTagsNotPermitted reports the control plane's refusal to mint a key for
// tags the credential may not use ("requested tags [tag:x] are invalid or
// not permitted"): the tailnet policy lacks the tagOwners entry.
func IsTagsNotPermitted(err error) bool {
	if err == nil {
		return false
	}
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "tags") && (strings.Contains(m, "not permitted") || strings.Contains(m, "invalid"))
}

// IsForbidden reports a 403 (e.g. an OAuth client without the scope).
func IsForbidden(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusForbidden
}

// IsUnauthorized reports a 401.
func IsUnauthorized(err error) bool {
	var e *APIError
	return errors.As(err, &e) && e.Status == http.StatusUnauthorized
}

// CreateOAuthClient creates an OAuth client (POST /keys, keyType "client";
// the caller's key needs the oauth_keys scope or admin rights). The
// returned Key's Secret is the client secret (tskey-client-…): never log
// it. Delete it with DeleteKey(ID).
func (c *Client) CreateOAuthClient(ctx context.Context, scopes, tags []string, description string) (Key, error) {
	body := map[string]any{"keyType": "client", "scopes": scopes, "tags": tags}
	if description != "" {
		body["description"] = description
	}
	var k Key
	if err := c.do(ctx, "POST", c.tailnetPath("/keys"), body, &k); err != nil {
		return Key{}, err
	}
	if k.ID == "" || k.Secret == "" {
		return Key{}, errors.New("tsapi: oauth client response missing id or key")
	}
	return k, nil
}

// DiscoverTailnet names the tailnet a token belongs to from its devices
// (GET /tailnet/-/devices: the MagicDNS suffix of any device name). It
// needs devices:core (read); without it the caller falls back to the
// netmap of a node it joined.
func (c *Client) DiscoverTailnet(ctx context.Context) (string, error) {
	cc := *c
	cc.Tailnet = "-"
	devs, err := cc.ListDevices(ctx)
	if err != nil {
		return "", err
	}
	for _, d := range devs {
		if _, suf, ok := strings.Cut(strings.TrimSuffix(d.Name, "."), "."); ok && strings.Contains(suf, ".") {
			return suf, nil
		}
	}
	return "", errors.New("no device names the tailnet yet")
}
