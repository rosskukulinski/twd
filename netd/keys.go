package netd

import (
	"context"
	"errors"
)

// KeyRequest describes the node an auth key is wanted for.
type KeyRequest struct {
	ObjectID  string
	Hostname  string
	Tags      []string
	Ephemeral bool
	// Remint: the object had saved node state, but control rejected it
	// (NeedsLogin: the device expired or was removed, which is what happens
	// to an ephemeral node that is offline during a restart). The object
	// needs a fresh key under the same hostname; mgmt-backed sources must
	// move the object out of "ready" first (POST /v0/objects/{id}/remint).
	Remint bool
}

// KeySource supplies auth keys for objects that have no saved node state,
// or whose saved state control rejected (KeyRequest.Remint).
//
// Implementations must not log the key.
type KeySource interface {
	AuthKey(ctx context.Context, req KeyRequest) (string, error)
}

// StaticKey is a KeySource that returns the same (typically reusable) key for
// every object. Its String form never reveals the key.
type StaticKey string

func (k StaticKey) AuthKey(context.Context, KeyRequest) (string, error) {
	if k == "" {
		return "", errors.New("netd: empty static auth key")
	}
	return string(k), nil
}

func (k StaticKey) String() string   { return "netd.StaticKey([REDACTED])" }
func (k StaticKey) GoString() string { return k.String() }

// MintFunc is a KeySource backed by a callback, e.g. twd-mgmt's mint broker
// minting a single-use, pre-authorized key per object.
type MintFunc func(ctx context.Context, req KeyRequest) (string, error)

func (f MintFunc) AuthKey(ctx context.Context, req KeyRequest) (string, error) {
	if f == nil {
		return "", errors.New("netd: nil MintFunc")
	}
	return f(ctx, req)
}
