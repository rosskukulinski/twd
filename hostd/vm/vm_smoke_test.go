//go:build linux

package vm

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/guest/proto"
	"github.com/rosskukulinski/twd/hostd/sandbox"
)

const smokeJS = `export default {
  async fetch(req, env) {
    const u = new URL(req.url);
    if (u.pathname === "/_twd/admin") return new Response("ok");
    if (u.pathname === "/egress") { const r = await env.GW.fetch("http://anything/x"); return new Response("via host: " + await r.text()); }
    return new Response("hello from the guest " + (req.cf ? JSON.stringify(req.cf) : "nocf"));
  }
};
`

const smokeCapnp = `using Workerd = import "/workerd/workerd.capnp";
const config :Workerd.Config = (
  services = [ (name = "main", worker = .w), (name = "gw", external = (address = "192.168.72.1:3128", http = ())) ],
  sockets = [ (name = "in", address = "*:8080", service = "main", http = (cfBlobHeader = "x-twd-cf")) ],
);
const w :Workerd.Worker = (
  compatibilityDate = "2026-04-01",
  modules = [ (name = "w.js", esModule = embed "w.js") ],
  bindings = [ (name = "GW", service = "gw") ],
);
`

// TestSmoke boots the guest image with a tiny workerd config (needs root,
// KVM and TWD_VM_KERNEL/TWD_VM_ROOTFS).
func TestSmoke(t *testing.T) {
	kernel, rootfs := os.Getenv("TWD_VM_KERNEL"), os.Getenv("TWD_VM_ROOTFS")
	if kernel == "" || rootfs == "" {
		t.Skip("TWD_VM_KERNEL / TWD_VM_ROOTFS not set")
	}
	b, err := New(Config{Kernel: kernel, Rootfs: rootfs})
	if err != nil {
		t.Skip(err)
	}
	app := t.TempDir()
	os.WriteFile(filepath.Join(app, "w.js"), []byte(smokeJS), 0o600)
	os.WriteFile(filepath.Join(app, "config.capnp"), []byte(smokeCapnp), 0o600)
	ctx := context.Background()
	m, err := b.Boot(ctx, sandbox.Spec{ID: "smoke", AppDir: app, WorkerdArgs: []string{"serve", "config.capnp", "--experimental"},
		HealthCF: `{}`, Debug: true,
		Handlers: map[uint16]http.Handler{proto.EgressObj: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "host handler saw "+r.Method+" "+r.URL.Path)
		})}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	t.Logf("info %+v rss %d kB", m.Info(), m.RSSKB())
	cl := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return m.DialGuest(ctx) }}}
	for _, p := range []string{"/", "/egress"} {
		resp, err := cl.Get("http://guest" + p)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Logf("%s → %d %s", p, resp.StatusCode, body)
		if resp.StatusCode != 200 || (p == "/egress" && !strings.Contains(string(body), "host handler saw GET /x")) {
			t.Fatal("unexpected")
		}
	}
	r, err := m.Op(ctx, proto.Msg{Op: "probe", Targets: []string{"192.168.72.1:22", "192.168.72.1:3129", "127.0.0.1:22", "10.0.2.2:22", "1.1.1.1:443"}})
	t.Logf("probe %+v err %v refused %v", r.Results, err, m.Refused())
	st, err := m.Op(ctx, proto.Msg{Op: "stats"})
	t.Logf("stats %+v %v", st.Stats, err)
	m.Kill()
	k := time.Now()
	<-m.Done()
	t.Logf("kill → done %v", time.Since(k))
}
