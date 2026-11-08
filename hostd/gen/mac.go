package gen

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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

// ComputeMAC is the platform Egress MAC: hex(HMAC-SHA256(key,
// class\nname\nmethod\nurl\nts\nnonce)). key is the hex string itself (as
// bytes), matching the platform's importKey("raw", utf8(MAC_KEY)).
func ComputeMAC(key, class, name, method, rawURL, ts, nonce string) string {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(strings.Join([]string{class, name, method, rawURL, ts, nonce}, "\n")))
	return hex.EncodeToString(m.Sum(nil))
}

// VerifyEgress checks a facet egress request's MAC and returns the object's
// class and name. rawURL must be the absolute URL as workerd sent it.
func VerifyEgress(key string, h http.Header, method, rawURL string, now time.Time) (class, name string, ok bool) {
	class = h.Get(HeaderClass)
	nameEsc := h.Get(HeaderName)
	ts, nonce, mac := h.Get(HeaderTS), h.Get(HeaderNonce), h.Get(HeaderMAC)
	if class == "" || nameEsc == "" || ts == "" || nonce == "" || mac == "" {
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
	want := ComputeMAC(key, class, n, method, rawURL, ts, nonce)
	if !hmac.Equal([]byte(want), []byte(mac)) {
		return "", "", false
	}
	return class, n, true
}

// StripPlatformHeaders removes the platform's egress headers.
func StripPlatformHeaders(h http.Header) {
	for _, k := range []string{HeaderClass, HeaderName, HeaderTS, HeaderNonce, HeaderMAC} {
		h.Del(k)
	}
}
