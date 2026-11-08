package gen

import (
	"net/http"
	"strconv"
	"testing"
	"time"
)

func egressHeaders(root, cls, name, method, url string, now time.Time, nonce string) http.Header {
	h := http.Header{}
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	h.Set(HeaderClass, cls)
	h.Set(HeaderName, name)
	h.Set(HeaderTS, ts)
	h.Set(HeaderNonce, nonce)
	h.Set(HeaderMAC, ComputeMAC(ObjectMACKey(root, cls, name), cls, name, method, url, ts, nonce))
	return h
}

// RT-04: MACs are keyed per object; RT-05: a nonce is accepted once.
func TestEgressMACPerObjectAndReplay(t *testing.T) {
	root, now := "0123456789abcdef0123456789abcdef", time.Now()
	h := egressHeaders(root, "Agent", "alice", "GET", "http://peer/x", now, "n1")
	var seen NonceCache
	if c, n, ok := VerifyEgressOnce(root, h, "GET", "http://peer/x", now, &seen); !ok || c != "Agent" || n != "alice" {
		t.Fatalf("valid MAC rejected: %v %s %s", ok, c, n)
	}
	if _, _, ok := VerifyEgressOnce(root, h, "GET", "http://peer/x", now.Add(time.Second), &seen); ok {
		t.Fatal("replayed nonce accepted")
	}
	// A sibling's key signs nothing for another object: re-label alice's
	// MAC as bob's.
	h2 := egressHeaders(root, "Agent", "alice", "GET", "http://peer/x", now, "n2")
	h2.Set(HeaderName, "bob")
	if _, _, ok := VerifyEgress(root, h2, "GET", "http://peer/x", now); ok {
		t.Fatal("MAC for alice verified as bob")
	}
	// Keys differ per object even for the same root.
	if ObjectMACKey(root, "Agent", "alice") == ObjectMACKey(root, "Agent", "bob") || ObjectMACKey(root, "Agent", "alice") == root {
		t.Fatal("object keys are not distinct from each other / the root")
	}
	// Skew.
	old := egressHeaders(root, "Agent", "alice", "GET", "http://peer/x", now.Add(-3*time.Minute), "n3")
	if _, _, ok := VerifyEgress(root, old, "GET", "http://peer/x", now); ok {
		t.Fatal("stale MAC accepted")
	}
	// The cache forgets after the skew window.
	if !seen.Add("k", now) || seen.Add("k", now.Add(time.Second)) || !seen.Add("k", now.Add(2*MACSkew+time.Second)) {
		t.Fatal("nonce cache window wrong")
	}
}
