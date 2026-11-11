package mgmt

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"
)

// Issuer is twd-mgmt's OIDC issuer for Tailscale workload identity
// federation (RFC §4.4, ADR-013). Each tenant has its own issuer URL,
// <base>/t/<tenantID>, so a tenant's trust credential can't accept another
// tenant's tokens; all tenants share one ES256 signing key (MVP: on disk at
// 0600; production: KMS).
//
// Endpoints (Handler):
//
//	/.well-known/openid-configuration            (issuer = base)
//	/jwks
//	/t/<tenant>/.well-known/openid-configuration (issuer = base/t/<tenant>)
//	/t/<tenant>/jwks
type Issuer struct {
	BaseURL string // https origin, no trailing slash (e.g. https://twd-mgmt.tail7906d5.ts.net)
	key     *ecdsa.PrivateKey
	kid     string
	now     func() time.Time
}

// LoadOrCreateIssuer loads the P-256 signing key at keyPath (PKCS#8 PEM,
// 0600), generating it on first use.
func LoadOrCreateIssuer(baseURL, keyPath string) (*Issuer, error) {
	if !strings.HasPrefix(baseURL, "https://") {
		return nil, errors.New("issuer URL must be https")
	}
	var key *ecdsa.PrivateKey
	b, err := os.ReadFile(keyPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, err
		}
		f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der})
		if err := f.Close(); err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		if st, _ := os.Stat(keyPath); st != nil && st.Mode().Perm()&0o077 != 0 {
			return nil, fmt.Errorf("issuer key %s must be mode 0600", keyPath)
		}
		blk, _ := pem.Decode(b)
		if blk == nil {
			return nil, errors.New("issuer key: no PEM block")
		}
		k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, err
		}
		var ok bool
		if key, ok = k.(*ecdsa.PrivateKey); !ok || key.Curve != elliptic.P256() {
			return nil, errors.New("issuer key: want a P-256 ECDSA key")
		}
	}
	return NewIssuer(baseURL, key), nil
}

// NewIssuer wraps an existing key.
func NewIssuer(baseURL string, key *ecdsa.PrivateKey) *Issuer {
	jwk := publicJWK(&key.PublicKey, "")
	// RFC 7638 thumbprint as kid.
	tp, _ := json.Marshal(map[string]string{"crv": jwk["crv"], "kty": jwk["kty"], "x": jwk["x"], "y": jwk["y"]})
	sum := sha256.Sum256(tp)
	return &Issuer{BaseURL: strings.TrimRight(baseURL, "/"), key: key, kid: b64(sum[:]), now: time.Now}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func publicJWK(pub *ecdsa.PublicKey, kid string) map[string]string {
	x, y := make([]byte, 32), make([]byte, 32)
	pub.X.FillBytes(x)
	pub.Y.FillBytes(y)
	m := map[string]string{"kty": "EC", "crv": "P-256", "x": b64(x), "y": b64(y)}
	if kid != "" {
		m["kid"], m["use"], m["alg"] = kid, "sig", "ES256"
	}
	return m
}

// TenantIssuer is the issuer URL for a tenant.
func (is *Issuer) TenantIssuer(tenantID string) string { return is.BaseURL + "/t/" + tenantID }

// MintSubject / LifecycleSubject are the pinned per-tenant subjects (RFC §4.4).
func MintSubject(tenantID string) string      { return "twd:tenant:" + tenantID + ":mint" }
func LifecycleSubject(tenantID string) string { return "twd:tenant:" + tenantID + ":lifecycle" }

// TokenTTL is the lifetime of issued tokens (≤5 min).
const TokenTTL = 5 * time.Minute

// Sign issues a JWT for a tenant: iss = TenantIssuer, aud = clientID.
func (is *Issuer) Sign(tenantID, subject, audience string) (string, error) {
	now := is.now()
	jti := make([]byte, 12)
	rand.Read(jti)
	claims := map[string]any{
		"iss": is.TenantIssuer(tenantID), "sub": subject, "aud": audience,
		"iat": now.Unix(), "nbf": now.Unix() - 30, "exp": now.Add(TokenTTL).Unix(), "jti": b64(jti),
	}
	return is.signClaims(claims)
}

func (is *Issuer) signClaims(claims map[string]any) (string, error) {
	h, _ := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": is.kid})
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, is.key, sum[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return input + "." + b64(sig), nil
}

// JWKS returns the public key set.
func (is *Issuer) JWKS() map[string]any {
	return map[string]any{"keys": []map[string]string{publicJWK(&is.key.PublicKey, is.kid)}}
}

func (is *Issuer) discovery(issuer string) map[string]any {
	return map[string]any{
		"issuer":                                issuer,
		"jwks_uri":                              issuer + "/jwks",
		"response_types_supported":              []string{"id_token"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"ES256"},
		"claims_supported":                      []string{"iss", "sub", "aud", "exp", "iat", "nbf", "jti"},
	}
}

// Handler serves discovery and JWKS (and nothing else: it is what Funnel exposes).
func (is *Issuer) Handler() http.Handler {
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, is.discovery(is.BaseURL))
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, is.JWKS()) })
	mux.HandleFunc("GET /t/{tenant}/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, is.discovery(is.TenantIssuer(r.PathValue("tenant"))))
	})
	mux.HandleFunc("GET /t/{tenant}/jwks", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, is.JWKS()) })
	return mux
}

// VerifyJWT verifies an ES256 JWT against a JWKS document and returns the
// claims. It exists for tests and for `twd doctor`; Tailscale is the real
// verifier.
func VerifyJWT(token string, jwks []byte, now time.Time) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("jwt: want 3 parts")
	}
	hb, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	var hdr struct{ Alg, Kid string }
	if err := json.Unmarshal(hb, &hdr); err != nil {
		return nil, err
	}
	if hdr.Alg != "ES256" {
		return nil, fmt.Errorf("jwt: alg %q", hdr.Alg)
	}
	var set struct {
		Keys []map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(jwks, &set); err != nil {
		return nil, err
	}
	var pub *ecdsa.PublicKey
	for _, k := range set.Keys {
		if k["kid"] == hdr.Kid && k["kty"] == "EC" && k["crv"] == "P-256" {
			x, _ := base64.RawURLEncoding.DecodeString(k["x"])
			y, _ := base64.RawURLEncoding.DecodeString(k["y"])
			pub = &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		}
	}
	if pub == nil {
		return nil, fmt.Errorf("jwt: kid %q not in JWKS", hdr.Kid)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return nil, errors.New("jwt: bad signature encoding")
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(pub, sum[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		return nil, errors.New("jwt: bad signature")
	}
	cb, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := json.Unmarshal(cb, &claims); err != nil {
		return nil, err
	}
	exp, _ := claims["exp"].(float64)
	nbf, _ := claims["nbf"].(float64)
	if now.Unix() >= int64(exp) {
		return nil, errors.New("jwt: expired")
	}
	if now.Unix() < int64(nbf) {
		return nil, errors.New("jwt: not yet valid")
	}
	return claims, nil
}

// secretChooser is the part of Store that arbitrates shared secrets.
type secretChooser interface {
	ChooseSecret(ctx context.Context, name, keyID string, now int64) (string, error)
	SecretChoice(ctx context.Context, name string) (string, error)
}

func issuerChoiceName(baseURL string) string { return "issuer:" + strings.TrimRight(baseURL, "/") }
func issuerKeyName(keyID string) string      { return "twd/mgmt/issuer/" + keyID }

// LoadSharedIssuer returns the WIF issuer for baseURL with a signing key
// every mgmt replica shares (mm-loop F11b, H13): the key lives in custody and
// a secret_choice row names it. The first replica to start offers its
// existing <state-dir>/issuer.key (keys Tailscale may already trust) or a new
// key; later replicas, and losers of a concurrent start, use the chosen one.
func LoadSharedIssuer(ctx context.Context, baseURL, keyPath string, st secretChooser, cust Custody) (*Issuer, error) {
	if !strings.HasPrefix(baseURL, "https://") {
		return nil, errors.New("issuer URL must be https")
	}
	name := issuerChoiceName(baseURL)
	kid, err := st.SecretChoice(ctx, name)
	if errors.Is(err, ErrNotFound) {
		var key *ecdsa.PrivateKey
		if b, rerr := os.ReadFile(keyPath); rerr == nil {
			if st, _ := os.Stat(keyPath); st != nil && st.Mode().Perm()&0o077 != 0 {
				return nil, fmt.Errorf("issuer key %s must be mode 0600", keyPath)
			}
			if key, err = parseIssuerKey(b); err != nil {
				return nil, err
			}
		} else if errors.Is(rerr, os.ErrNotExist) {
			if key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
				return nil, err
			}
		} else {
			return nil, rerr
		}
		pemKey, err := encodeIssuerKey(key)
		if err != nil {
			return nil, err
		}
		mine := newID("iss_")
		if err := cust.Put(ctx, issuerKeyName(mine), pemKey); err != nil {
			return nil, fmt.Errorf("custody: issuer key: %w", err)
		}
		if kid, err = st.ChooseSecret(ctx, name, mine, time.Now().UnixMilli()); err != nil {
			return nil, err
		}
		if kid != mine {
			cust.Delete(ctx, issuerKeyName(mine))
		}
	} else if err != nil {
		return nil, err
	}
	b, err := cust.Get(ctx, issuerKeyName(kid))
	if err != nil {
		return nil, fmt.Errorf("custody: issuer key %s: %w", kid, err)
	}
	key, err := parseIssuerKey(b)
	if err != nil {
		return nil, err
	}
	return NewIssuer(baseURL, key), nil
}

func parseIssuerKey(b []byte) (*ecdsa.PrivateKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("issuer key: no PEM block")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("issuer key: want a P-256 ECDSA key")
	}
	return key, nil
}

func encodeIssuerKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}
