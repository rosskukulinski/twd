package gen

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Headers set by the platform Egress entrypoint.
const (
	HeaderClass = "x-twd-class"
	HeaderName  = "x-twd-name"
	HeaderTS    = "x-twd-ts"
	HeaderNonce = "x-twd-nonce"
	HeaderMAC   = "x-twd-mac"
)

// MACSkew is how far a MAC timestamp may be from now.
const MACSkew = 2 * time.Minute

// objectKeyDomain separates per-object key derivation from the MAC itself.
const objectKeyDomain = "twd-egress-object-key-v1\n"

// ObjectMACKey derives the per-object egress key from the instance root key
// (RT-04): hex(HMAC-SHA256(root, domain + class + "\n" + name)). The
// platform derives the same key in JS (platform.js objectKey), so the key
// that signs an object's egress is specific to that object: a MAC for
// (class, name) verifies only under that object's key, and a key leaked for
// one object signs nothing for a sibling. root is the hex string itself (as
// bytes), matching the platform's importKey("raw", utf8(MAC_KEY)).
func ObjectMACKey(root, class, name string) string {
	m := hmac.New(sha256.New, []byte(root))
	m.Write([]byte(objectKeyDomain + class + "\n" + name))
	return hex.EncodeToString(m.Sum(nil))
}

// ComputeMAC is the platform Egress MAC: hex(HMAC-SHA256(key,
// class\nname\nmethod\nurl\nts\nnonce)). key is the per-object key (hex, as
// bytes), matching the platform's importKey("raw", utf8(objectKey)).
func ComputeMAC(key, class, name, method, rawURL, ts, nonce string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(strings.Join([]string{class, name, method, rawURL, ts, nonce}, "\n")))
	return hex.EncodeToString(m.Sum(nil))
}

// VerifyEgress checks a facet egress request's MAC under the per-object key
// derived from root and returns the object's class and name. rawURL must be
// the absolute URL as workerd sent it. It does not check for replays; use
// VerifyEgressOnce on a serving path.
func VerifyEgress(root string, h http.Header, method, rawURL string, now time.Time) (class, name string, ok bool) {
	class = h.Get(HeaderClass)
	nameEsc := h.Get(HeaderName)
	ts, nonce, mac := h.Get(HeaderTS), h.Get(HeaderNonce), h.Get(HeaderMAC)
	if class == "" || nameEsc == "" || ts == "" || nonce == "" || mac == "" || len(nonce) > 128 {
		return "", "", false
	}
	n, err := url.PathUnescape(nameEsc)
	if err != nil {
		return "", "", false
	}
	ms, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return "", "", false
	}
	if d := now.Sub(time.UnixMilli(ms)); d > MACSkew || d < -MACSkew {
		return "", "", false
	}
	want := ComputeMAC(ObjectMACKey(root, class, n), class, n, method, rawURL, ts, nonce)
	if !hmac.Equal([]byte(want), []byte(mac)) {
		return "", "", false
	}
	return class, n, true
}

// VerifyEgressOnce is VerifyEgress plus a replay check (RT-05): a (class,
// name, nonce) seen within the skew window is refused.
func VerifyEgressOnce(root string, h http.Header, method, rawURL string, now time.Time, seen *NonceCache) (class, name string, ok bool) {
	class, name, ok = VerifyEgress(root, h, method, rawURL, now)
	if !ok {
		return "", "", false
	}
	if seen != nil && !seen.Add(class+"\n"+name+"\n"+h.Get(HeaderNonce), now) {
		return "", "", false
	}
	return class, name, true
}

// NonceCache remembers nonces for the MAC skew window. The zero value is
// ready to use.
type NonceCache struct {
	mu    sync.Mutex
	seen  map[string]time.Time
	sweep time.Time
}

// Add records key at now and reports whether it was new.
func (c *NonceCache) Add(key string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]time.Time{}
	}
	if now.Sub(c.sweep) > MACSkew || len(c.seen) > 100_000 {
		for k, t := range c.seen {
			if now.Sub(t) > MACSkew {
				delete(c.seen, k)
			}
		}
		c.sweep = now
	}
	if _, dup := c.seen[key]; dup {
		return false
	}
	c.seen[key] = now
	return true
}

// StripPlatformHeaders removes the platform's egress headers.
func StripPlatformHeaders(h http.Header) {
	for _, k := range []string{HeaderClass, HeaderName, HeaderTS, HeaderNonce, HeaderMAC} {
		h.Del(k)
	}
}
