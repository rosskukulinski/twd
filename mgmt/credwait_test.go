package mgmt

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestMM_H05_CredentialWaitSeesOtherReplicasMint: a host long-polls replica B
// for a credential while replica A mints the object. B is never notified
// locally (m.changed is per replica), so it must notice the mint through the
// store promptly, not after the host's whole wait (hostd asks wait=10 s).
func TestMM_H05_CredentialWaitSeesOtherReplicasMint(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, _ := OpenStore(ctx, filepath.Join(dir, "m.db"))
	defer st.Close()
	cust, _ := OpenFileCustody(filepath.Join(dir, "c"))
	b, err := New(Config{Store: st, Custody: cust, Broker: &Broker{}, IDSecret: []byte("0123456789abcdef"), ReapInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	kf := filepath.Join(dir, "api.env")
	os.WriteFile(kf, []byte("TS_API_KEY=x\n"), 0o600)
	tn, _ := b.CreateTenant(ctx, TenantRequest{Tailnet: "t.ts.net", MintMode: "apikey", APIKeyFile: kf})
	l, err := st.AcquireLease(ctx, LeaseName(tn.ID, "w"), "h1", NamespaceLeaseTTL, st.Now())
	if err != nil {
		t.Fatal(err)
	}
	o := Object{ID: "o1", TenantID: tn.ID, Workload: "w", Namespace: "A", Name: "o1", Hostname: "o1", RootID: "o1",
		Lease: LeaseName(tn.ID, "w"), Status: StatusPending}
	if err := st.CreateObject(ctx, o); err != nil {
		t.Fatal(err)
	}
	// "Replica A" mints 300 ms from now: custody + row, no notify on B.
	go func() {
		time.Sleep(300 * time.Millisecond)
		o.KeyID = "k1"
		cust.Put(ctx, custodyName(o), []byte("fake-authkey-k1"))
		st.UpdateObject(ctx, "o1", nil, func(x *Object) error {
			x.Status, x.KeyID, x.KeyExpires = StatusMinted, "k1", time.Now().Add(KeyExpiry).UnixMilli()
			return nil
		})
	}()
	start := time.Now()
	cred, err := b.Credential(ctx, "o1", "h1", l.Epoch, 10*time.Second)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	t.Logf("credential released after %v (minted by another replica at ~300 ms)", took.Round(time.Millisecond))
	if cred.AuthKey != "fake-authkey-k1" {
		t.Fatalf("wrong key %q", cred.AuthKey)
	}
	if took > 2*time.Second {
		t.Fatalf("I8: the long-poll only noticed another replica's mint after %v (the whole wait), not promptly", took.Round(time.Millisecond))
	}
}
