package main_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rosskukulinski/twd/e2e/cliflow"
	"github.com/rosskukulinski/twd/hostd/bundle"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
	"github.com/rosskukulinski/twd/internal/oci"
	"github.com/rosskukulinski/twd/mgmt"
	"github.com/rosskukulinski/twd/netd/nettest"
	"tailscale.com/tailcfg"
)

// writeLayout writes a one-layer linux/arm64 OCI image layout dir.
func writeLayout(t *testing.T, dir, body string) {
	t.Helper()
	ctx := context.Background()
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	tw.WriteHeader(&tar.Header{Name: "base.txt", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write([]byte(body))
	tw.Close()
	cfg, _ := json.Marshal(map[string]any{"architecture": "arm64", "os": "linux", "config": map[string]any{"Cmd": []string{"/bin/true"}},
		"rootfs": map[string]any{"type": "layers", "diff_ids": []string{oci.Digest(layer.Bytes())}}})
	w := &oci.WriteLayout{Dir: dir}
	for _, b := range [][]byte{layer.Bytes(), cfg} {
		if err := w.WriteBlob(ctx, "", oci.Digest(b), int64(len(b)), bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
	}
	m, _ := json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest,
		Config: &oci.Descriptor{MediaType: oci.MediaTypeConfig, Digest: oci.Digest(cfg), Size: int64(len(cfg))},
		Layers: []oci.Descriptor{{MediaType: oci.MediaTypeLayer, Digest: oci.Digest(layer.Bytes()), Size: int64(layer.Len())}}})
	if err := w.WriteManifest(ctx, "", oci.Digest(m), oci.MediaTypeManifest, m); err != nil {
		t.Fatal(err)
	}
	if err := w.Finish(oci.Descriptor{MediaType: oci.MediaTypeManifest, Digest: oci.Digest(m), Size: int64(len(m))}); err != nil {
		t.Fatal(err)
	}
}

// TestCLIImages (ADR-032 D3) with the real binaries: a developer machine
// linked to twd-mgmt over the tailnet (its own tsnet node; WhoIs admin)
// pushes an operator image into public/ (twd image push), marks it a public
// base, and `twd deploy` builds an app's Dockerfile with docker buildx,
// applying the arm64 alias map from twd's registry, and pushes it into the
// tenant's namespace over the same tailnet connection, audited under the
// developer's WhoIs identity. This Mac host offers no containers, so the
// deploy itself is refused with no_host after the push.
func TestCLIImages(t *testing.T) {
	if os.Getenv("TWD_TEST_DOCKER") == "" {
		t.Skip("set TWD_TEST_DOCKER=1 to run docker buildx (make test-images)")
	}
	if err := exec.Command("docker", "buildx", "version").Run(); err != nil {
		t.Skip("docker buildx unavailable:", err)
	}
	wd, err := nettest.WorkerdBinary()
	if err != nil || !hostdtest.HasNode() {
		t.Skip("needs workerd and node (make deps)")
	}
	t.Setenv("TWD_WORKERD", wd)
	ctl, err := nettest.StartControl()
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.Close()
	ctl.Server.SetGlobalAppCaps(tailcfg.PeerCapMap{
		nettest.TestCap: []tailcfg.RawMessage{nettest.TestCapValue},
		mgmt.AdminCap:   []tailcfg.RawMessage{`{}`},
	})
	fts := newFakeTS(t)
	api := httptest.NewServer(fts)
	defer api.Close()
	bin := cliflow.BuildBinaries(t)
	state, _ := hostdtest.ShortDir("twd-im-")
	work := t.TempDir()
	keyFile := filepath.Join(work, "api.env")
	os.WriteFile(keyFile, []byte("TS_API_KEY=fake-not-a-key\n"), 0o600)
	authFile := filepath.Join(work, "auth.env")
	os.WriteFile(authFile, []byte("TS_AUTHKEY="+nettest.DummyKey+"\n"), 0o600)
	host := &cliflow.Env{T: t, BinDir: bin, StateDir: state, WorkDir: work, Tailnet: "ttest.ts.net", APIKeyFile: keyFile,
		ControlURL: ctl.URL, TSAPIURL: api.URL, Prefix: "twd-i-", Res: cliflow.NewResults("hermetic-images", "ttest.ts.net")}
	dev := &cliflow.Env{T: t, BinDir: bin, StateDir: filepath.Join(work, "dev-state"), WorkDir: work, Res: host.Res}
	t.Cleanup(func() {
		if t.Failed() {
			for _, c := range []string{"mgmt", "hostd"} {
				b, _ := os.ReadFile(filepath.Join(state, "logs", c+".log"))
				t.Logf("%s log (tail):\n%s", c, hostdtest.Trim(string(b[max(0, len(b)-4000):]), 4000))
			}
		}
		host.Twd("down", "--purge")
		os.RemoveAll(state)
	})
	var up struct{ MgmtNode, MgmtIP string }
	host.MustJSON(&up, "up", "--tailnet", "ttest.ts.net", "--api-key-file", keyFile, "--yes", "--ephemeral", "--control-url", ctl.URL,
		"--tsapi-url", api.URL, "--hostname-prefix", "twd-i-", "--mgmt-hostname", "twd-mgmt", "--mgmt-authkey-file", authFile)
	t.Setenv("TWD_LINK_DISCOVER_NAME", "twd-mgmt-hermetic.invalid")
	var lk struct {
		Actor   string
		Tenants []struct{ TenantID string }
	}
	dev.MustJSON(&lk, "link", "http://"+up.MgmtIP, "--tsnet", "--tsnet-dir", filepath.Join(work, "dev-node"), "--tsnet-hostname", "ttest-dev",
		"--control-url", ctl.URL, "--tsnet-authkey-file", authFile)
	if len(lk.Tenants) != 1 {
		t.Fatalf("link: %+v", lk)
	}
	tenant := lk.Tenants[0].TenantID

	// The operator's arm64 rebuild of the alias source (a stand-in here)
	// goes to public/ over the tailnet.
	lay := filepath.Join(work, "rebuild")
	writeLayout(t, lay, "twd's arm64 rebuild\n")
	var pushed struct{ Digest string }
	dev.MustJSON(&pushed, "image", "push", lay, "twd.local/cloudflare-sandbox:1.0.0-arm64")
	if !oci.ValidDigest(pushed.Digest) {
		t.Fatalf("image push: %+v", pushed)
	}
	var pub struct {
		Items []mgmt.PublicImage `json:"items"`
	}
	dev.MustJSON(&pub, "image", "public", "add", "twd.local/cloudflare-sandbox:1.0.0-arm64")
	if len(pub.Items) != 1 || pub.Items[0].Repository != "public/cloudflare-sandbox" || pub.Items[0].Arch != "arm64" {
		t.Fatalf("public add: %+v", pub)
	}

	// An app with a Dockerfile container class (FROM the alias source,
	// through an ARG default as Sandbox SDK 1.0's example writes it).
	app := filepath.Join(work, "box")
	dev.MustJSON(nil, "init", app, "--template", "hello")
	os.WriteFile(filepath.Join(app, "Dockerfile"), []byte("ARG TOOLS=docker.io/cloudflare/sandbox:1.0.0\nFROM ${TOOLS}\nCOPY hello.txt /hello.txt\n"), 0o644)
	os.WriteFile(filepath.Join(app, "hello.txt"), []byte("hello\n"), 0o644)
	wr, _ := os.ReadFile(filepath.Join(app, "wrangler.jsonc"))
	var w map[string]any
	if err := bundle.UnmarshalJSONC(wr, &w); err != nil {
		t.Fatal(err)
	}
	w["containers"] = []any{map[string]any{"class_name": "Box", "image": "./Dockerfile"}}
	w["durable_objects"] = map[string]any{"bindings": []any{map[string]any{"name": "BOX", "class_name": "Box"}}}
	wb, _ := json.Marshal(w)
	os.WriteFile(filepath.Join(app, "wrangler.jsonc"), wb, 0o644)
	src, _ := os.ReadFile(filepath.Join(app, "src", "index.ts"))
	os.WriteFile(filepath.Join(app, "src", "index.ts"), append(src, []byte("\nimport { DurableObject } from \"cloudflare:workers\";\nexport class Box extends DurableObject {}\n")...), 0o644)

	so, se, _, err := dev.Twd("deploy", app, "--platform", "linux/arm64", "--allow-missing-grants")
	if err == nil || !strings.Contains(se, "no_host") {
		t.Fatalf("deploy on a host without containers should be no_host after the push: %v\n%s\n%s", err, so, se)
	}
	if !strings.Contains(so, "alias      docker.io/cloudflare/sandbox:1.0.0 → twd.local/cloudflare-sandbox:1.0.0-arm64 (linux/arm64, oci-layout)") ||
		!strings.Contains(so, "✓ image      Box → twd.local/"+tenant+"/") || !strings.Contains(so, "-box@sha256:") {
		t.Fatalf("deploy output:\n%s\n%s", so, se)
	}
	// The tenant push is in mgmt's audit chain under the developer's identity.
	var aud struct {
		Verified bool
		Records  []struct {
			Kind, ActingFor, TenantID string
			Detail                    json.RawMessage
		}
	}
	dev.MustJSON(&aud, "audit", "--all")
	repos := map[string]bool{}
	for _, r := range aud.Records {
		if r.Kind == "image_push" && r.ActingFor == lk.Actor {
			var d struct{ Repository string }
			json.Unmarshal(r.Detail, &d)
			repos[d.Repository] = true
		}
	}
	built := false
	for r := range repos {
		built = built || strings.HasPrefix(r, tenant+"/") && strings.HasSuffix(r, "-box")
	}
	// (public/ pushes are operator records outside any tenant's audit view;
	// `image public add` above found that push in public/.)
	if !aud.Verified || !built {
		t.Fatalf("audit image_push by %s: %v (verified %v)", lk.Actor, repos, aud.Verified)
	}
}
