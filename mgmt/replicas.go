package mgmt

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"
)

// Replica is one twd-mgmt process sharing the store (F6, H11).
type Replica struct {
	ID        string `json:"id"`
	Custody   string `json:"custody"` // CustodyFingerprint
	StartedAt int64  `json:"startedAt"`
	LastSeen  int64  `json:"lastSeen"` // store clock, unix ms; 0 = shut down cleanly
}

// ReplicaLiveTTL: a replica not seen for this long is treated as gone. It is
// refreshed at Start and every reaper tick, so keep ReapInterval well below.
const ReplicaLiveTTL = 30 * time.Second

// LoadOrCreateReplicaID returns the stable replica ID kept in path, creating
// it on first use. Stable across restarts, so a replica that crashed and
// restarts is not mistaken for a second replica.
func LoadOrCreateReplicaID(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil && strings.HasPrefix(strings.TrimSpace(string(b)), "rp_") {
		return strings.TrimSpace(string(b)), nil
	}
	var r [8]byte
	rand.Read(r[:])
	id := "rp_" + hex.EncodeToString(r[:])
	if err := os.WriteFile(path, []byte(id+"\n"), 0o600); err != nil {
		return "", err
	}
	return id, nil
}

// replicaConflict reports why this replica must not share the store with the
// live replicas it can see, or "" if it may: file custody is never shared,
// and every live replica must use the same custody.
func (m *Mgmt) replicaConflict(rs []Replica, now time.Time) string {
	return replicaConflictFor(m.replicaID, CustodyFingerprint(m.custody), rs, now)
}

func replicaConflictFor(myID, mine string, rs []Replica, now time.Time) string {
	for _, r := range rs {
		if r.ID == myID || r.LastSeen == 0 || now.Sub(time.UnixMilli(r.LastSeen)) > ReplicaLiveTTL {
			continue
		}
		seen := now.Sub(time.UnixMilli(r.LastSeen)).Round(time.Second)
		switch {
		case strings.HasPrefix(mine, "file:"):
			return fmt.Sprintf("another twd-mgmt replica (%s, seen %v ago) shares this store, but this replica keeps auth keys in file custody on this machine only, so the other replica could not release them. Run every replica with -custody setec -setec-url <same URL>, or run a single replica", r.ID, seen)
		case strings.HasPrefix(r.Custody, "file:"):
			return fmt.Sprintf("replica %s (seen %v ago) shares this store with file custody on another machine; its keys are unreadable here. Run every replica with -custody setec", r.ID, seen)
		case r.Custody != mine:
			return fmt.Sprintf("replica %s (seen %v ago) uses custody %q, this replica %q; replicas sharing a store must share custody", r.ID, seen, r.Custody, mine)
		}
	}
	return ""
}

// PreflightReplica runs the replica-registry custody check before twd-mgmt
// loads any shared secret (ID secret, issuer key), so a misconfigured
// replica is refused with the explanation rather than failing later on a
// secret that lives in another replica's custody.
func PreflightReplica(ctx context.Context, st Store, cust Custody, replicaID string) error {
	rs, err := st.ListReplicas(ctx)
	if err != nil {
		return fmt.Errorf("replica registry: %w", err)
	}
	now := time.Now()
	if sc, ok := st.(StoreClock); ok {
		now = sc.Now()
	}
	if msg := replicaConflictFor(replicaID, CustodyFingerprint(cust), rs, now); msg != "" {
		return fmt.Errorf("refusing to start: %s", msg)
	}
	return nil
}

// registerReplica records this replica and checks it against the others.
func (m *Mgmt) registerReplica(ctx context.Context) (string, error) {
	now := m.now()
	if m.startedAt == 0 {
		m.startedAt = now.UnixMilli()
	}
	if err := m.store.UpsertReplica(ctx, Replica{ID: m.replicaID, Custody: CustodyFingerprint(m.custody),
		StartedAt: m.startedAt, LastSeen: now.UnixMilli()}); err != nil {
		return "", err
	}
	rs, err := m.store.ListReplicas(ctx)
	if err != nil {
		return "", err
	}
	live := 0
	for _, r := range rs {
		if r.LastSeen != 0 && now.Sub(time.UnixMilli(r.LastSeen)) <= ReplicaLiveTTL {
			live++
		}
	}
	msg := m.replicaConflict(rs, now)
	m.mu.Lock()
	changed := msg != m.replicaMsg
	m.replicaMsg, m.liveReplicas = msg, live
	m.mu.Unlock()
	if changed && msg != "" {
		m.log.Warn("replica conflict", "message", msg)
	}
	return msg, nil
}

// unregisterReplica marks this replica gone (clean shutdown), so a
// replacement may start at once.
func (m *Mgmt) unregisterReplica(ctx context.Context) {
	m.store.UpsertReplica(ctx, Replica{ID: m.replicaID, Custody: CustodyFingerprint(m.custody), StartedAt: m.startedAt, LastSeen: 0})
}

// ReplicaStatus is the "replicas" object of GET /v1/status.
type ReplicaStatus struct {
	ID      string `json:"id"`
	Live    int    `json:"live"`
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

func (m *Mgmt) replicaStatus() ReplicaStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return ReplicaStatus{ID: m.replicaID, Live: m.liveReplicas, OK: m.replicaMsg == "", Message: m.replicaMsg}
}
