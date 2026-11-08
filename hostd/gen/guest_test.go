package gen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosskukulinski/twd/hostd/bundle"
)

func testApp(t *testing.T, twd string) *bundle.App {
	t.Helper()
	app, err := bundle.Parse(map[string][]byte{
		"wrangler.jsonc":  []byte(`{"name":"g","main":"dist/index.js","compatibility_date":"2026-04-01"}`),
		"twd.jsonc":       []byte(twd),
		"twd-bundle.json": []byte(`{"main":"dist/index.js"}`),
		"dist/index.js":   []byte(`export default { fetch() { return new Response("ok") } }`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// TestGuestNet: the microVM tier's config listens and dials over TCP (the
// guest NIC), keeps DO storage guest-local, and creates nothing on the host
// for it.
func TestGuestNet(t *testing.T) {
	app := testApp(t, `{"tier":"microvm","tailnet":{"tags":["tag:ttest"]}}`)
	if app.Tier != bundle.TierMicroVM {
		t.Fatalf("tier %q", app.Tier)
	}
	dir := t.TempDir()
	hostDO := filepath.Join(dir, "host-do")
	res, err := Generate(app, Options{Dir: filepath.Join(dir, "i"), DOStorage: hostDO, DeploymentID: "d1", Workload: "g",
		Guest: &GuestNet{Ingress: "*:8080", EgressObj: "192.168.72.1:3128", EgressWL: "192.168.72.1:3129", HostAPI: "192.168.72.1:3130", DOStorage: "/run/twd/do"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(res.Config)
	c := string(b)
	for _, want := range []string{`address = "*:8080"`, `address = "192.168.72.1:3128", http = (style = proxy)`,
		`address = "192.168.72.1:3129", http = (style = proxy)`, `address = "192.168.72.1:3130"`, `path = "/run/twd/do"`} {
		if !strings.Contains(c, want) {
			t.Errorf("config lacks %s", want)
		}
	}
	if strings.Contains(c, "unix:") {
		t.Error("guest config references a unix socket")
	}
	if _, err := os.Stat(hostDO); err == nil {
		t.Error("host DO storage dir created for a guest config")
	}
}

func TestBadTier(t *testing.T) {
	_, err := bundle.Parse(map[string][]byte{
		"wrangler.jsonc": []byte(`{"name":"g","main":"dist/index.js","compatibility_date":"2026-04-01"}`),
		"twd.jsonc":      []byte(`{"tier":"gvisor"}`),
		"dist/index.js":  []byte(`export default {}`),
	})
	if err == nil || !strings.Contains(err.Error(), "tier") {
		t.Fatalf("want a tier error, got %v", err)
	}
}
