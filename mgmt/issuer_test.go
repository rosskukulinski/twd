package mgmt

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func TestIssuerJWKSAndJWT(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "issuer.key")
	is, err := LoadOrCreateIssuer("https://twd-mgmt.example.ts.net", keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(keyPath); st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", st.Mode())
	}
	if _, err := LoadOrCreateIssuer("http://insecure", filepath.Join(dir, "x.key")); err == nil {
		t.Fatal("http issuer accepted")
	}
	srv := httptest.NewServer(is.Handler())
	defer srv.Close()
	get := func(p string) []byte {
		res, err := srv.Client().Get(srv.URL + p)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("GET %s: %v %v", p, err, res)
		}
		if ct := res.Header.Get("Content-Type"); ct != "application/json" {
			t.Fatalf("content-type %q", ct)
		}
		b, _ := io.ReadAll(res.Body)
		return b
	}
	var disc map[string]any
	json.Unmarshal(get("/t/tn_8c2f/.well-known/openid-configuration"), &disc)
	if disc["issuer"] != "https://twd-mgmt.example.ts.net/t/tn_8c2f" || disc["jwks_uri"] != "https://twd-mgmt.example.ts.net/t/tn_8c2f/jwks" {
		t.Fatalf("discovery %v", disc)
	}
	if algs := disc["id_token_signing_alg_values_supported"].([]any); algs[0] != "ES256" {
		t.Fatal(algs)
	}
	json.Unmarshal(get("/.well-known/openid-configuration"), &disc)
	if disc["issuer"] != "https://twd-mgmt.example.ts.net" {
		t.Fatal(disc)
	}
	jwksBytes := get("/t/tn_8c2f/jwks")
	if strings.Contains(string(jwksBytes), `"d"`) {
		t.Fatal("JWKS leaks the private key")
	}

	tok, err := is.Sign("tn_8c2f", MintSubject("tn_8c2f"), "client-123")
	if err != nil {
		t.Fatal(err)
	}
	// Our verifier.
	claims, err := VerifyJWT(tok, jwksBytes, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if claims["sub"] != "twd:tenant:tn_8c2f:mint" || claims["aud"] != "client-123" || claims["iss"] != "https://twd-mgmt.example.ts.net/t/tn_8c2f" {
		t.Fatalf("claims %v", claims)
	}
	if exp, iat := claims["exp"].(float64), claims["iat"].(float64); exp-iat > 300 {
		t.Fatalf("ttl %v > 5 min", exp-iat)
	}
	if _, err := VerifyJWT(tok, jwksBytes, time.Now().Add(6*time.Minute)); err == nil {
		t.Fatal("expired token verified")
	}
	if _, err := VerifyJWT(tok[:len(tok)-4]+"AAAA", jwksBytes, time.Now()); err == nil {
		t.Fatal("tampered signature verified")
	}

	// Independent verification with go-jose (what an OIDC relying party does).
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(jwksBytes, &set); err != nil || len(set.Keys) != 1 || !set.Keys[0].Valid() || !set.Keys[0].IsPublic() {
		t.Fatalf("JWKS not a valid public key set: %v", err)
	}
	if set.Keys[0].Algorithm != "ES256" || set.Keys[0].Use != "sig" {
		t.Fatalf("jwk %+v", set.Keys[0])
	}
	parsed, err := jwt.ParseSigned(tok, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal(err)
	}
	var std jwt.Claims
	if err := parsed.Claims(set.Keys[0].Key, &std); err != nil {
		t.Fatalf("go-jose rejected our signature: %v", err)
	}
	if err := std.ValidateWithLeeway(jwt.Expected{Issuer: "https://twd-mgmt.example.ts.net/t/tn_8c2f",
		Subject: "twd:tenant:tn_8c2f:mint", AnyAudience: jwt.Audience{"client-123"}, Time: time.Now()}, 0); err != nil {
		t.Fatal(err)
	}
	if parsed.Headers[0].KeyID != set.Keys[0].KeyID {
		t.Fatal("kid mismatch")
	}

	// Reload uses the same key (same kid).
	is2, err := LoadOrCreateIssuer("https://twd-mgmt.example.ts.net", keyPath)
	if err != nil || is2.kid != is.kid {
		t.Fatal("reload changed key", err)
	}
}
