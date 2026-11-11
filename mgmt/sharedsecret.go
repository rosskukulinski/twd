package mgmt

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"time"
)

// LoadSharedIDSecret returns the object-ID HMAC key (RFC §4.3) shared by
// every mgmt replica of the store (mm-loop F12, H14), so an object's ID and
// hostname don't depend on which replica created it. The first replica to
// start offers its existing <state-dir>/id.secret (keeping the IDs it has
// already issued) or a new 32-byte secret; the others use the chosen one.
func LoadSharedIDSecret(ctx context.Context, path string, st secretChooser, cust Custody) ([]byte, error) {
	const choice = "id-secret"
	name := func(kid string) string { return "twd/mgmt/id-secret/" + kid }
	kid, err := st.SecretChoice(ctx, choice)
	if errors.Is(err, ErrNotFound) {
		var secret []byte
		if b, rerr := os.ReadFile(path); rerr == nil && len(b) >= 16 {
			secret = b
		} else if rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			return nil, rerr
		} else {
			secret = make([]byte, 32)
			if _, err := rand.Read(secret); err != nil {
				return nil, err
			}
		}
		mine := newID("ids_")
		if err := cust.Put(ctx, name(mine), secret); err != nil {
			return nil, fmt.Errorf("custody: id secret: %w", err)
		}
		if kid, err = st.ChooseSecret(ctx, choice, mine, time.Now().UnixMilli()); err != nil {
			return nil, err
		}
		if kid != mine {
			cust.Delete(ctx, name(mine))
		}
	} else if err != nil {
		return nil, err
	}
	secret, err := cust.Get(ctx, name(kid))
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("custody: the ID secret chosen by another replica (%s) is not in this replica's custody (%s): replicas sharing a store must share custody (-custody setec -setec-url <same URL>)", kid, CustodyFingerprint(cust))
	} else if err != nil {
		return nil, fmt.Errorf("custody: id secret %s: %w", kid, err)
	}
	if len(secret) < 16 {
		return nil, errors.New("custody: id secret too short")
	}
	return secret, nil
}
