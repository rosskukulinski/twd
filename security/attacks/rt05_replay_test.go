package attacks

// Pass 2 (live) — RT-05: the egress MAC now carries a nonce and hostd keeps
// a replay cache (hostd/gen.NonceCache, VerifyEgressOnce). A captured
// (class,name,nonce,ts,mac) tuple replayed inside the skew window must be
// refused the second time, and a tuple outside the skew window must be
// refused outright. This exercises the real verify path with a per-object
// key derived the way the platform derives it (gen.ObjectMACKey).

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd/gen"
)

func macHeaders(root, class, name, method, url, ts, nonce string) http.Header {
	h := http.Header{}
	h.Set(gen.HeaderClass, class)
	h.Set(gen.HeaderName, name)
	h.Set(gen.HeaderTS, ts)
	h.Set(gen.HeaderNonce, nonce)
	key := gen.ObjectMACKey(root, class, name)
	h.Set(gen.HeaderMAC, gen.ComputeMAC(key, class, name, method, url, ts, nonce))
	return h
}

func TestRT05_EgressMACReplayRefused(t *testing.T) {
	const root = "deadbeefdeadbeefdeadbeefdeadbeef"
	const class, name, method, url = "Agent", "a1", "GET", "http://peer/x"
	now := time.Now()
	ts := itoa(now.UnixMilli())
	nonce := "nonce-abc-123"

	var cache gen.NonceCache

	// First use with a valid, fresh MAC: accepted.
	h := macHeaders(root, class, name, method, url, ts, nonce)
	if c, n, ok := gen.VerifyEgressOnce(root, h, method, url, now, &cache); !ok || c != class || n != name {
		t.Fatalf("RT-05 precondition: a valid fresh MAC was refused (%v %v %v)", c, n, ok)
	}

	// Replay the identical tuple inside the skew window: must be refused.
	h2 := macHeaders(root, class, name, method, url, ts, nonce)
	if _, _, ok := gen.VerifyEgressOnce(root, h2, method, url, now.Add(10*time.Second), &cache); ok {
		t.Fatalf("RT-05 BROKEN: a replayed egress MAC (same nonce) was accepted within the skew window")
	}

	// A fresh nonce from the same object is fine (not a replay).
	h3 := macHeaders(root, class, name, method, url, ts, "nonce-xyz-999")
	if _, _, ok := gen.VerifyEgressOnce(root, h3, method, url, now, &cache); !ok {
		t.Fatalf("RT-05: a fresh nonce must be accepted")
	}

	// A tuple whose timestamp is outside the skew window is refused even with
	// a correct MAC (replay of an old captured header after the cache forgot it).
	old := now.Add(-20 * time.Minute)
	ots := itoa(old.UnixMilli())
	h4 := macHeaders(root, class, name, method, url, ots, "nonce-old")
	if _, _, ok := gen.VerifyEgressOnce(root, h4, method, url, now, &cache); ok {
		t.Fatalf("RT-05 BROKEN: an out-of-skew egress MAC was accepted")
	}

	// A MAC computed under a different object's key must not verify for this
	// object (keys are per-object; RT-04 derivation).
	wrong := macHeaders("cafebabecafebabecafebabecafebabe", class, name, method, url, itoa(now.UnixMilli()), "nonce-wrong")
	if _, _, ok := gen.VerifyEgressOnce(root, wrong, method, url, now, &cache); ok {
		t.Fatalf("RT-05/RT-04 BROKEN: a MAC under another object's key verified")
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}
