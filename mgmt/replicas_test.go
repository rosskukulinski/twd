package mgmt

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMM_F6_ReplicaRegistry: replicas sharing a store must share custody;
// file custody is never shared (H11). A crashed replica restarting with its
// stable ID is not a second replica; a clean shutdown frees the store.
func TestMM_F6_ReplicaRegistry(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := OpenStore(ctx, filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fileCust := func(name string) Custody {
		c, err := OpenFileCustody(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	start := func(id string, c Custody) (*Mgmt, error) {
		t.Helper()
		m, err := New(Config{Store: st, Custody: c, Broker: &Broker{}, IDSecret: []byte("0123456789abcdef"),
			ReplicaID: id, ReapInterval: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Start(); err != nil {
			m.cancel() // Start refused (and already unregistered itself): just stop its context
			return nil, err
		}
		return m, nil
	}

	// 1. One file-custody replica: fine. A second one is refused, clearly.
	a, err := start("rp_a", fileCust("ca"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := start("rp_b", fileCust("cb")); err == nil || !strings.Contains(err.Error(), "file custody") || !strings.Contains(err.Error(), "rp_a") {
		t.Fatalf("second file-custody replica: want a refusal naming rp_a and file custody, got %v", err)
	}
	// 2. A crashed rp_a (no clean shutdown, its row still live) restarting
	// with the same stable ID starts.
	a.cancel() // simulate a crash: no Close, no unregister
	a2, err := start("rp_a", fileCust("ca"))
	if err != nil {
		t.Fatalf("restart of the same replica ID within the live window must start: %v", err)
	}
	if s := a2.replicaStatus(); !s.OK || s.Live != 1 {
		t.Fatalf("status after restart = %+v, want ok and 1 live", s)
	}
	// 3. Clean shutdown frees the store at once for another replica.
	a2.Close()
	b, err := start("rp_b", fileCust("cb"))
	if err != nil {
		t.Fatalf("after rp_a's clean shutdown rp_b must start: %v", err)
	}
	b.Close()

	// 4. Shared setec: several replicas fine; a different setec URL is refused.
	s1, err := start("rp_s1", &SetecCustody{BaseURL: "https://setec.example/"})
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	s2, err := start("rp_s2", &SetecCustody{BaseURL: "https://setec.example"})
	if err != nil {
		t.Fatalf("same setec URL (trailing slash aside) must be accepted: %v", err)
	}
	defer s2.Close()
	if s := s2.replicaStatus(); !s.OK || s.Live != 2 {
		t.Fatalf("two setec replicas: status %+v, want ok and 2 live", s)
	}
	if _, err := start("rp_s3", &SetecCustody{BaseURL: "https://other-setec.example"}); err == nil || !strings.Contains(err.Error(), "custody") {
		t.Fatalf("different setec: want a refusal, got %v", err)
	}
	if _, err := start("rp_f", fileCust("cf")); err == nil {
		t.Fatal("a file-custody replica joining setec replicas must be refused")
	}
}
