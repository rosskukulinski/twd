package mgmt

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

// ErrNoLifecycle means the tenant has no credential that can delete devices.
var ErrNoLifecycle = errors.New("tenant has no lifecycle credential (kill tier 2 / reaper device delete unavailable)")

// Broker builds per-tenant API clients:
//   - apikey: the tenant's API key file (homelab / tests) for every purpose;
//   - wif: sign a 5-minute JWT with the platform issuer and exchange it at
//     /api/v2/oauth/token-exchange for the purpose's trust credential
//     (mint → wifClientID; lifecycle and policy reads → wifLifecycleClientID).
//
// Access tokens are cached until shortly before they expire.
type Broker struct {
	BaseURL string // Tailscale API, default tsapi.DefaultBaseURL
	HTTP    *http.Client
	Issuer  *Issuer
	Clock   Clock

	mu     sync.Mutex
	tokens map[string]cachedToken
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
	}
	return nil, fmt.Errorf("unknown mint mode %q", t.MintMode)
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
	jwt, err := b.Issuer.Sign(tenantID, subject, clientID)
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
