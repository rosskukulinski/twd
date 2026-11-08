package hostd

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestSignCallerFormat(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	h := signCaller(key, map[string]any{"v": 1, "caller": map[string]any{"nodeId": "n1"}, "iat": 5})
	parts := strings.Split(h, ".")
	if len(parts) != 3 || parts[0] != "v1" {
		t.Fatalf("format: %s", h)
	}
	m := hmac.New(sha256.New, key)
	m.Write([]byte("v1." + parts[1]))
	if base64.RawURLEncoding.EncodeToString(m.Sum(nil)) != parts[2] {
		t.Fatal("signature mismatch")
	}
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c map[string]any
	if json.Unmarshal(b, &c) != nil || c["caller"].(map[string]any)["nodeId"] != "n1" {
		t.Fatalf("payload: %s", b)
	}
}

func TestParseProxyAuth(t *testing.T) {
	ok := "Basic " + base64.StdEncoding.EncodeToString([]byte("twd:tok:en"))
	if u, p, good := parseProxyAuth(ok); !good || u != "twd" || p != "tok:en" {
		t.Fatal(u, p, good)
	}
	for _, v := range []string{"", "Bearer x", "Basic !!!", "Basic " + base64.StdEncoding.EncodeToString([]byte("nocolon"))} {
		if _, _, good := parseProxyAuth(v); good {
			t.Fatalf("%q accepted", v)
		}
	}
}
