package mgmt

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rosskukulinski/twd/internal/tsapi"
)

// TailnetAPI is the per-tenant Tailscale API surface mgmt uses.
type TailnetAPI interface {
	CreateKey(ctx context.Context, o tsapi.KeyOptions) (tsapi.Key, error)
	DeleteKey(ctx context.Context, id string) error
	DeleteDevice(ctx context.Context, id string) error
	GetPolicy(ctx context.Context, etag string) ([]byte, string, bool, error)
}

// Credential purposes (RFC §4.4: separate mint and lifecycle credentials).
const (
	purposeMint      = "mint"
	purposeLifecycle = "lifecycle"
	purposePolicy    = "policy"
)

// WIFAudience is the audience Tailscale generates for a federated trust
// credential by default: "api.tailscale.com/<clientID>" (so every tenant's
// tokens are bound to its own client ID).
func WIFAudience(clientID string) string { return "api.tailscale.com/" + clientID }

// ErrNoLifecycle means the tenant has no credential that can delete devices.
var ErrNoLifecycle = errors.New("tenant has no lifecycle credential (kill tier 2 / reaper device delete unavailable)")

// Broker builds per-tenant API clients:
//   - apikey: the tenant's API key file (homelab / tests) for every purpose;
//   - wif: sign a 5-minute JWT with the platform issuer and exchange it at
//     /api/v2/oauth/token-exchange for the purpose's trust credential
//     (mint → wifClientID; lifecycle and policy reads → wifLifecycleClientID);
//   - oauth: the tenant's OAuth client (secret in a 0600 file on this host,
//     re-read at each exchange) for every purpose: client-credentials grant
//     at /api/v2/oauth/token; a 401 drops the token and retries once. What
//     the client may do is its scopes' business (auth_keys always;
//     devices:core for device deletes, policy_file:read for preflight).
//
// Access tokens are cached until shortly before they expire.
type Broker struct {
	BaseURL string // Tailscale API, default tsapi.DefaultBaseURL
	HTTP    *http.Client
	Issuer  *Issuer
	Clock   Clock

	mu     sync.Mutex
	tokens map[string]cachedToken
	oauth  map[string]*tsapi.OAuthSource // tenantID|clientID|file
}

type cachedToken struct {
	token string
	exp   time.Time
}

func (b *Broker) clock() Clock {
	if b.Clock == nil {
		return realClock{}
	}
	return b.Clock
}

// API returns a client for the tenant and purpose.
func (b *Broker) API(ctx context.Context, t Tenant, purpose string) (TailnetAPI, error) {
	c := &tsapi.Client{BaseURL: b.BaseURL, Tailnet: t.Tailnet, HTTP: b.HTTP}
	switch t.MintMode {
	case MintAPIKey:
		if t.APIKeyFile == "" {
			return nil, errors.New("apikey mode needs apiKeyFile")
		}
		k, err := tsapi.LoadEnvFile(t.APIKeyFile, "TS_API_KEY")
		if err != nil {
			return nil, fmt.Errorf("tenant %s api key: %w", t.ID, err)
		}
		c.APIKey = k
		return c, nil
	case MintWIF:
		clientID, subject := t.WIFClientID, MintSubject(t.ID)
		if purpose != purposeMint {
			if t.WIFLifecycleClientID == "" {
				return nil, ErrNoLifecycle
			}
			clientID, subject = t.WIFLifecycleClientID, LifecycleSubject(t.ID)
		}
		tok, err := b.token(ctx, t.ID, clientID, subject)
		if err != nil {
			return nil, err
		}
		c.APIKey = tok
		return c, nil
	case MintOAuth:
		c.Tokens = b.oauthSource(t)
		return c, nil
	}
	return nil, fmt.Errorf("unknown mint mode %q", t.MintMode)
}

// oauthSource returns the tenant's cached OAuth token source.
func (b *Broker) oauthSource(t Tenant) *tsapi.OAuthSource {
	key := t.ID + "|" + t.OAuthClientID + "|" + t.OAuthClientFile
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.oauth[key]; ok {
		return s
	}
	if b.oauth == nil {
		b.oauth = map[string]*tsapi.OAuthSource{}
	}
	file, wantID := t.OAuthClientFile, t.OAuthClientID
	s := &tsapi.OAuthSource{BaseURL: b.BaseURL, HTTP: b.HTTP, Now: func() time.Time { return b.clock().Now() },
		Load: func() (tsapi.OAuthCredentials, error) {
			c, err := tsapi.LoadOAuthFile(file)
			if err != nil {
				return c, fmt.Errorf("tenant %s oauth client: %w", t.ID, err)
			}
			if wantID != "" && c.ClientID != wantID {
				return tsapi.OAuthCredentials{}, fmt.Errorf("tenant %s oauth client: %s now holds client %s, not %s (update the tenant)", t.ID, file, c.ClientID, wantID)
			}
			return c, nil
		}}
	b.oauth[key] = s
	return s
}

// checkOAuthFile validates an oauth tenant's credential file (absolute,
// 0600, a tskey-client- secret) and returns the client ID (wantID, or the
// one the file names). The secret is not kept.
func checkOAuthFile(file, wantID string) (string, error) {
	if file == "" {
		return "", errf(400, "bad_request", "mintMode oauth needs oauthClientFile (a 0600 file with TS_OAUTH_CLIENT_ID and TS_OAUTH_CLIENT_SECRET)")
	}
	if !filepath.IsAbs(file) {
		return "", errf(400, "bad_request", "oauthClientFile must be an absolute path")
	}
	c, err := tsapi.LoadOAuthFile(file)
	if err != nil {
		return "", errf(400, "bad_request", "oauthClientFile: %v", err)
	}
	if wantID != "" && wantID != c.ClientID {
		return "", errf(400, "bad_request", "oauthClientID %s does not match the client in %s (%s)", wantID, file, c.ClientID)
	}
	return c.ClientID, nil
}

// Forget drops a tenant's cached access tokens (after its client IDs
// changed, or to force a fresh exchange).
func (b *Broker) Forget(tenantID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k := range b.tokens {
		if strings.HasPrefix(k, tenantID+"|") {
			delete(b.tokens, k)
		}
	}
	for k := range b.oauth {
		if strings.HasPrefix(k, tenantID+"|") {
			delete(b.oauth, k)
		}
	}
}

func (b *Broker) token(ctx context.Context, tenantID, clientID, subject string) (string, error) {
	if b.Issuer == nil {
		return "", errors.New("wif mode needs an issuer (-issuer-url)")
	}
	key := tenantID + "|" + clientID
	now := b.clock().Now()
	b.mu.Lock()
	if ct, ok := b.tokens[key]; ok && now.Before(ct.exp) {
		b.mu.Unlock()
		return ct.token, nil
	}
	b.mu.Unlock()
	jwt, err := b.Issuer.Sign(tenantID, subject, WIFAudience(clientID))
	if err != nil {
		return "", err
	}
	tr, err := tsapi.ExchangeToken(ctx, b.HTTP, b.BaseURL, clientID, jwt)
	if err != nil {
		return "", err
	}
	life := time.Duration(tr.ExpiresIn) * time.Second
	if life <= 0 {
		life = 5 * time.Minute
	}
	b.mu.Lock()
	if b.tokens == nil {
		b.tokens = map[string]cachedToken{}
	}
	b.tokens[key] = cachedToken{tr.AccessToken, now.Add(life * 9 / 10)}
	b.mu.Unlock()
	return tr.AccessToken, nil
}
