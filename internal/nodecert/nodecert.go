// Package nodecert is the node certificate of ADR-010 / RFC §4.5–4.6: at
// `ready`, twd-mgmt signs {tenant, objectID, stableNodeID, tags, lim,
// parent, root, iat, exp} with the tenant's OBO root key (Ed25519). A
// receiving netd fetches the sender's certificate from mgmt (keyed by the
// WhoIs stable node ID), verifies it against the tenant's pinned OBO public
// key, and narrows the sender's WhoIs CapMap by cert.lim before tenant code
// sees request.cf.tailscale.caller.
//
// Wire form: "<base64url(payload JSON)>.<base64url(ed25519 sig)>". The
// signature covers "twd-nodecert-v1\n" + payload bytes.
package nodecert

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// TTL is how long a certificate is valid after it is issued.
const TTL = 24 * time.Hour

const domain = "twd-nodecert-v1\n"

// Payload is the signed content.
type Payload struct {
	V            int             `json:"v"`
	TenantID     string          `json:"tenant"`
	ObjectID     string          `json:"objectID"`
	StableNodeID string          `json:"stableNodeID"`
	Tags         []string        `json:"tags"`
	Lim          json.RawMessage `json:"lim,omitempty"`
	ParentID     string          `json:"parent,omitempty"`
	RootID       string          `json:"root"`
	IAT          int64           `json:"iat"` // unix seconds
	EXP          int64           `json:"exp"` // unix seconds
}

var (
	ErrMalformed = errors.New("nodecert: malformed certificate")
	ErrSignature = errors.New("nodecert: bad signature")
	ErrExpired   = errors.New("nodecert: expired")
	ErrNodeID    = errors.New("nodecert: certificate is for another node")
)

var b64 = base64.RawURLEncoding

// Sign encodes and signs p (V is forced to 1).
func Sign(priv ed25519.PrivateKey, p Payload) (string, error) {
	p.V = 1
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(priv, append([]byte(domain), b...))
	return b64.EncodeToString(b) + "." + b64.EncodeToString(sig), nil
}

// Verify checks the signature against pub, that the certificate names
// stableNodeID (when non-empty) and that it is valid at now.
func Verify(pub ed25519.PublicKey, cert, stableNodeID string, now time.Time) (*Payload, error) {
	pb, sb, ok := strings.Cut(cert, ".")
	if !ok {
		return nil, ErrMalformed
	}
	payload, err := b64.DecodeString(pb)
	if err != nil {
		return nil, ErrMalformed
	}
	sig, err := b64.DecodeString(sb)
	if err != nil || len(sig) != ed25519.SignatureSize || len(pub) != ed25519.PublicKeySize {
		return nil, ErrMalformed
	}
	if !ed25519.Verify(pub, append([]byte(domain), payload...), sig) {
		return nil, ErrSignature
	}
	var p Payload
	if err := json.Unmarshal(payload, &p); err != nil || p.V != 1 || p.StableNodeID == "" || p.ObjectID == "" {
		return nil, ErrMalformed
	}
	if stableNodeID != "" && p.StableNodeID != stableNodeID {
		return nil, ErrNodeID
	}
	if now.Unix() >= p.EXP || now.Unix()+300 < p.IAT {
		return nil, ErrExpired
	}
	return &p, nil
}

// Decode parses the payload without verifying (display/debugging only).
func Decode(cert string) (*Payload, error) {
	pb, _, ok := strings.Cut(cert, ".")
	if !ok {
		return nil, ErrMalformed
	}
	payload, err := b64.DecodeString(pb)
	if err != nil {
		return nil, ErrMalformed
	}
	var p Payload
	if err := json.Unmarshal(payload, &p); err != nil {
		return nil, ErrMalformed
	}
	return &p, nil
}
