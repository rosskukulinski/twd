package hostd_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/hostd/fakemgmt"
	"github.com/rosskukulinski/twd/hostd/hostdtest"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/hostd/sandbox/sandboxtest"
	"github.com/rosskukulinski/twd/internal/oci"
	"github.com/rosskukulinski/twd/internal/registry"
	"github.com/rosskukulinski/twd/netd/nettest"
	"tailscale.com/tailcfg"
)

// pushImage writes a linux/<host arch> image (entrypoint sleep, IMAGE_ENV
// env) into repo:tag of dst and returns its manifest digest.
func pushImage(t *testing.T, dst oci.Target, repo, tag, imageEnv string) (manifest, config string) {
	t.Helper()
	ctx := context.Background()
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	tw.WriteHeader(&tar.Header{Name: "etc/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.WriteHeader(&tar.Header{Name: "etc/variant", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(imageEnv))})
	tw.Write([]byte(imageEnv))
	tw.Close()
	cfg, _ := json.Marshal(map[string]any{"architecture": runtime.GOARCH, "os": "linux",
		"config": map[string]any{"Entrypoint": []string{"sleep", "3600"}, "Env": []string{"PATH=/usr/bin:/bin", "IMAGE_ENV=" + imageEnv}, "WorkingDir": "/"}})
	for _, b := range [][]byte{layer.Bytes(), cfg} {
		if err := dst.WriteBlob(ctx, repo, oci.Digest(b), int64(len(b)), bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
	}
	m, _ := json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest,
		Config: &oci.Descriptor{MediaType: oci.MediaTypeConfig, Digest: oci.Digest(cfg), Size: int64(len(cfg))},
		Layers: []oci.Descriptor{{MediaType: oci.MediaTypeLayer, Digest: oci.Digest(layer.Bytes()), Size: int64(layer.Len())}}})
	if err := dst.WriteManifest(ctx, repo, oci.Digest(m), oci.MediaTypeManifest, m); err != nil {
		t.Fatal(err)
	}
	if tag != "" {
		if err := dst.WriteManifest(ctx, repo, tag, oci.MediaTypeManifest, m); err != nil {
			t.Fatal(err)
		}
	}
	return oci.Digest(m), oci.Digest(cfg)
}

// TestContainerRegistryPull (ADR-032 D3) through hostd + workerd + the
// polyfill: a deploy pins the upstream image's digest (mirrored into the
// tenant's namespace) and twd deploy's own build (twd.local/<tenant>/…);
// hostd pulls exactly those digests from mgmt's registry as a host and
// starts containers from them. Retagging upstream after the deploy
// changes nothing that runs; a tenant naming another tenant's image is
// refused at deploy.
func TestContainerRegistryPull(t *testing.T) {
	ctx := context.Background()
	procs := &sandboxtest.Procs{}
	// The upstream registry ("docker.io" in production; mgmt mirrors it).
	upReg := &registry.Registry{Dir: t.TempDir(), Meta: &registry.MemMeta{}, Authorize: func(*http.Request, string, bool) error { return nil }}
	upSrv := httptest.NewServer(upReg)
	defer upSrv.Close()
	up := &oci.Client{Base: upSrv.URL, HTTP: upSrv.Client()}
	upDigest, upCfg := pushImage(t, up, "acme/box", "1", "upstream-v1")

	st, err := sandboxtest.ImageStore(t.TempDir(), []string{"sleep", "3600"})
	if err != nil {
		t.Fatal(err)
	}
	st.Sources = nil // pinned images only: nothing on the host's disk
	fake := fakemgmt.Config{RegistryDir: t.TempDir(), RegistryUpstream: func(string) *oci.Client { return up }}
	s := startSuiteCapsOpts(t, fake, tailcfg.PeerCapMap{nettest.TestCap: []tailcfg.RawMessage{nettest.TestCapValue}}, func(o *hostdtest.Options) {
		o.Containers, o.Images, o.ContainerGrace = procs, st, 2*time.Second
	})
	e := s.e
	tn := e.TenantID
	// twd deploy's push of its build: the admin (local socket, no host ID).
	adm := e.Mgmt.OCI()
	built, builtCfg := pushImage(t, adm, tn+"/ctr-pbox", "", "built")

	files, err := hostdtest.Build(hostdtest.Fixture("ctr"), nil)
	if err != nil {
		t.Fatal(err)
	}
	files["twd.jsonc"] = []byte(strings.Replace(strings.Replace(string(files["twd.jsonc"]),
		`"Box": { "image": "twd.test/box:1" }`, `"Box": { "image": "upstream.test/acme/box:1" }`, 1),
		`"PBox": { "image": "twd.test/box:1" },`, ``, 1))
	dep, err := e.DeployBuilt(files, "ctr", map[string]mgmtapi.BuiltImages{"PBox": {Image: "twd.local/" + tn + "/ctr-pbox@" + built}})
	if err != nil {
		t.Fatal(err)
	}
	wl := e.WaitActive("ctr", dep, 60*time.Second)
	s.waitPeer(wl.IP)
	base := "http://" + wl.IP
	call := func(path string, q ...string) map[string]any {
		t.Helper()
		u := base + path
		if len(q) > 0 {
			u += "?" + url.Values{q[0]: {q[1]}}.Encode()
		}
		code, m := s.getJSON(u)
		if code == 0 {
			t.Fatalf("%s: %v", path, m)
		}
		return m
	}
	// Upstream retags acme/box:1 before anything starts.
	_, v2Cfg := pushImage(t, up, "acme/box", "1", "upstream-v2")

	if r := call("/f/a/start"); r["running"] != true {
		t.Fatalf("start: %v", r)
	}
	if r := call("/f/a/exec", "argv", `["sh","-c","echo ok"]`); strings.TrimSpace(fmt.Sprint(r["stdout"])) != "ok" {
		t.Fatalf("exec: %v", r)
	}
	if r := call("/p/a/start"); r["running"] != true {
		t.Fatalf("start PBox: %v", r)
	}
	// What runs is the image ID (config digest) of what was pinned.
	ran := map[string]string{}
	for dl := time.Now().Add(10 * time.Second); time.Now().Before(dl) && (ran["Box"] == "" || ran["PBox"] == ""); time.Sleep(20 * time.Millisecond) {
		for _, c := range e.Host.Containers() {
			ran[c.Class] = c.Digest
		}
	}
	if ran["Box"] != upCfg || ran["Box"] == v2Cfg {
		t.Fatalf("Box runs %s; pinned upstream v1 is %s (the retag is %s)", ran["Box"], upCfg, v2Cfg)
	}
	if ran["PBox"] != builtCfg {
		t.Fatalf("PBox runs %s, want twd deploy's build %s", ran["PBox"], builtCfg)
	}
	// The pins recorded for the deployment are what ran.
	var as []mgmtapi.Assignment
	if as, err = e.Mgmt.Assignments(ctx, "h1"); err != nil {
		t.Fatal(err)
	}
	for _, a := range as {
		for _, c := range a.Containers {
			for _, p := range c.Pins {
				switch c.Class {
				case "Box":
					if p.Digest != upDigest || p.Repository != tn+"/mirror/upstream.test/acme/box" {
						t.Fatalf("Box pin %+v (upstream v1 %s)", p, upDigest)
					}
				case "PBox":
					if p.Digest != built || p.Repository != tn+"/ctr-pbox" {
						t.Fatalf("PBox pin %+v", p)
					}
				}
			}
		}
	}

	// Tenant B: naming tenant A's build is refused at deploy (the per-tenant
	// cache scopes are covered by hostd/image TestPinnedScopes).
	eb := e.AddTenant(mgmtapi.Tenant{Tailnet: "ttest.ts.net", ControlURL: s.ctl.URL, MintMode: "static"})
	if _, err := eb.DeployBuilt(files, "ctrb", map[string]mgmtapi.BuiltImages{"PBox": {Image: "twd.local/" + tn + "/ctr-pbox@" + built}}); err == nil ||
		!strings.Contains(err.Error(), "not in this tenant's registry namespace") {
		t.Fatalf("tenant B deployed tenant A's image: %v", err)
	}
}
