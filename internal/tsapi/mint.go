package tsapi

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/rosskukulinski/twd/netd"
)

// MintSource returns a netd.KeySource that mints one single-use,
// pre-authorized key per object (5 min expiry) via the API. Single-use keys
// are consumed by the join; RevokeAll cleans up any that were not.
func MintSource(api *Client, log *slog.Logger) *Minter {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Minter{api: api, log: log}
}

// Minter implements netd.KeySource.
type Minter struct {
	api *Client
	log *slog.Logger
	mu  sync.Mutex
	ids []string
}

var _ netd.KeySource = (*Minter)(nil)

func (m *Minter) AuthKey(ctx context.Context, req netd.KeyRequest) (string, error) {
	if len(req.Tags) == 0 {
		return "", errors.New("minting requires tags")
	}
	k, err := m.api.CreateKey(ctx, KeyOptions{Tags: req.Tags, Ephemeral: req.Ephemeral, Preauthorized: true,
		ExpirySeconds: 300, Description: "twd " + req.ObjectID})
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	m.ids = append(m.ids, k.ID)
	m.mu.Unlock()
	m.log.Info("minted auth key", "object", req.ObjectID, "key_id", k.ID)
	return k.Secret, nil
}

// KeyIDs returns the IDs of every key minted so far.
func (m *Minter) KeyIDs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.ids...)
}

// RevokeAll deletes every minted key (404s are ignored).
func (m *Minter) RevokeAll(ctx context.Context) error {
	var errs []error
	for _, id := range m.KeyIDs() {
		if err := m.api.DeleteKey(ctx, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
