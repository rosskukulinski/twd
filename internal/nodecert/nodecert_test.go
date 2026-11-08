package nodecert

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"
)

func TestSignVerify(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	p := Payload{TenantID: "tn", ObjectID: "o1", StableNodeID: "nABC", Tags: []string{"tag:sub"},
		Lim: json.RawMessage(`{"x":{"tools":["a"]}}`), ParentID: "p1", RootID: "p1", IAT: now.Unix(), EXP: now.Add(TTL).Unix()}
	c, err := Sign(priv, p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Verify(pub, c, "nABC", now)
	if err != nil || got.ObjectID != "o1" || string(got.Lim) != `{"x":{"tools":["a"]}}` || got.V != 1 {
		t.Fatalf("verify: %v %+v", err, got)
	}
	if _, err := Verify(pub, c, "nOTHER", now); err != ErrNodeID {
		t.Fatalf("other node: %v", err)
	}
	if _, err := Verify(pub, c, "nABC", now.Add(TTL+time.Second)); err != ErrExpired {
		t.Fatalf("expired: %v", err)
	}
	pub2, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := Verify(pub2, c, "nABC", now); err != ErrSignature {
		t.Fatalf("wrong key: %v", err)
	}
	// Tampered payload (lim widened) fails.
	d, _ := Decode(c)
	d.Lim = json.RawMessage(`{"x":{"tools":["a","refund"]}}`)
	b, _ := json.Marshal(d)
	tampered := b64.EncodeToString(b) + "." + c[len(c)-86:]
	if _, err := Verify(pub, tampered, "nABC", now); err != ErrSignature && err != ErrMalformed {
		t.Fatalf("tampered: %v", err)
	}
	if _, err := Verify(pub, "garbage", "", now); err != ErrMalformed {
		t.Fatalf("garbage: %v", err)
	}
}
