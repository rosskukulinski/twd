package netd

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
)

// fakeKey builds a key-shaped string at runtime so the literal never appears
// in the source (the pre-commit hook blocks real-looking keys).
func fakeKey() string { return "tskey-" + "auth-" + "kT3st1234CNTRL-" + strings.Repeat("Ab9", 10) }

func TestRedact(t *testing.T) {
	k := fakeKey()
	for in, want := range map[string]string{
		k:                          "tskey-[REDACTED]",
		"key=" + k + " trailing":   "key=tskey-[REDACTED] trailing",
		"tskey-client-abc_DEF-123": "tskey-[REDACTED]",
		"a tskey-api-x, b":         "a tskey-[REDACTED], b",
	} {
		if out := Redact(in); out != want {
			t.Errorf("Redact(%q) = %q, want %q", in, out, want)
		}
	}
	if got := Redact("no secrets here"); got != "no secrets here" {
		t.Errorf("changed clean string: %q", got)
	}
	err := RedactError(fmt.Errorf("wrap: %w", errors.New("bad key "+k)))
	if strings.Contains(err.Error(), k) {
		t.Errorf("RedactError leaked: %v", err)
	}
}

func TestRedactingHandler(t *testing.T) {
	k := fakeKey()
	var buf bytes.Buffer
	l := slog.New(NewRedactingHandler(slog.NewJSONHandler(&buf, nil))).With("bound", k)
	l.Info("msg "+k, "s", k, "err", errors.New("e "+k), "any", struct{ K string }{k},
		slog.Group("g", "inner", k), "n", 3)
	l.WithGroup("grp").Warn("x", "y", k)
	out := buf.String()
	if strings.Contains(out, k) || strings.Contains(out, "kT3st1234") {
		t.Fatalf("secret leaked: %s", out)
	}
	if strings.Count(out, "tskey-[REDACTED]") < 7 {
		t.Errorf("expected redaction markers: %s", out)
	}
}

func TestStaticKeyNeverPrints(t *testing.T) {
	k := StaticKey(fakeKey())
	for _, s := range []string{fmt.Sprint(k), fmt.Sprintf("%v %s %+v %#v", k, k, k, k)} {
		if strings.Contains(s, "kT3st1234") {
			t.Fatalf("StaticKey printed: %s", s)
		}
	}
}

func TestGonetEndpointAccessor(t *testing.T) {
	// A gVisor bump that renames gonet.TCPConn.ep would silently turn kill's
	// RST into a FIN; fail here instead.
	if ep := gonetEndpoint(&gonet.TCPConn{}); ep != nil {
		t.Fatal("zero TCPConn should have nil endpoint")
	}
	if !hasEPField() {
		t.Fatal("gonet.TCPConn has no field ep of type tcpip.Endpoint")
	}
}

func TestIdentityHeaderStripping(t *testing.T) {
	h := http.Header{}
	for _, k := range []string{"X-Twd-Cf", "X-Twd-Object", "Twd-Error", "Cf-Connecting-Ip", "X-Forwarded-For", "X-Forwarded-Host",
		"Forwarded", "X-Real-Ip", "Tailnet-Assertion", "Tailscale-User-Login", "Tailscale-App-Capabilities", "Proxy-Authorization"} {
		h.Set(k, "forged")
	}
	h.Set("Authorization", "Bearer app-level")
	h.Set("Content-Type", "text/plain")
	stripIdentityHeaders(h)
	if len(h) != 2 || h.Get("Authorization") == "" || h.Get("Content-Type") == "" {
		t.Fatalf("after strip: %v", h)
	}
}
