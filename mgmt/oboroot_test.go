package mgmt

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestMM_F11_OBORootChosenOnce (H13): two replicas racing to create a
// tenant's OBO root key end up with the same key; a pre-F11 legacy key is
// adopted by both.
func TestMM_F11_OBORootChosenOnce(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := OpenStore(ctx, filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cust, _ := OpenFileCustody(filepath.Join(dir, "c"))
	mk := func() *Mgmt {
		m, err := New(Config{Store: st, Custody: cust, Broker: &Broker{}, IDSecret: []byte("0123456789abcdef"), ReapInterval: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(m.Close)
		return m
	}
	a, b := mk(), mk()
	race := func(tid string) (ka, kb ed25519.PrivateKey) {
		var wg sync.WaitGroup
		gate := make(chan struct{})
		var ea, eb error
		wg.Add(2)
		go func() { defer wg.Done(); <-gate; ka, ea = a.oboKey(ctx, tid) }()
		go func() { defer wg.Done(); <-gate; kb, eb = b.oboKey(ctx, tid) }()
		close(gate)
		wg.Wait()
		if ea != nil || eb != nil {
			t.Fatalf("%s: %v / %v", tid, ea, eb)
		}
		return ka, kb
	}
	for i := range 40 {
		tid := fmt.Sprintf("tn_race%02d", i)
		ka, kb := race(tid)
		if !bytes.Equal(ka, kb) {
			t.Fatalf("%s: replicas chose different OBO root keys", tid)
		}
		// Exactly one candidate is left in custody.
		kid, _ := st.SecretChoice(ctx, oboChoiceName(tid))
		if _, err := cust.Get(ctx, oboKeyName(tid, kid)); err != nil {
			t.Fatalf("%s: chosen key %s missing from custody: %v", tid, kid, err)
		}
	}
	// Legacy: a key stored under the pre-F11 name is adopted by both.
	legacy := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	cust.Put(ctx, oboCustodyName("tn_old"), legacy)
	ka, kb := race("tn_old")
	if !bytes.Equal(ka, kb) || !bytes.Equal(ka.Seed(), legacy) {
		t.Fatal("a legacy OBO root key must be adopted unchanged by every replica")
	}
}
