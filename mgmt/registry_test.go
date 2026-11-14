package mgmt

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/internal/container"
	"github.com/rosskukulinski/twd/internal/oci"
	"github.com/rosskukulinski/twd/internal/registry"
)

// handlerRT serves requests with h directly, as if from remote addr (so a
// fake WhoIs can tell callers apart).
type handlerRT struct {
	h    http.Handler
	addr string
}

func (rt handlerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.RemoteAddr = rt.addr
	r.RequestURI = r.URL.RequestURI()
	if r.Body == nil {
		r.Body = http.NoBody
	}
	w := httptest.NewRecorder()
	rt.h.ServeHTTP(w, r)
	return w.Result(), nil
}

var imgN int

// pushTestImage writes a one-layer linux/arch image into dst repo (tag
// optional) and returns its manifest digest.
func pushTestImage(t *testing.T, dst oci.Target, repo, arch, tag string) string {
	t.Helper()
	ctx := context.Background()
	imgN++
	var layer bytes.Buffer
	tw := tar.NewWriter(&layer)
	body := fmt.Sprintf("image %d %s", imgN, arch)
	tw.WriteHeader(&tar.Header{Name: "id", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	tw.Write([]byte(body))
	tw.Close()
	cfg, _ := json.Marshal(map[string]any{"architecture": arch, "os": "linux", "config": map[string]any{}})
	for _, b := range [][]byte{layer.Bytes(), cfg} {
		if err := dst.WriteBlob(ctx, repo, oci.Digest(b), int64(len(b)), bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
	}
	m, _ := json.Marshal(oci.Manifest{SchemaVersion: 2, MediaType: oci.MediaTypeManifest,
		Config: &oci.Descriptor{MediaType: oci.MediaTypeConfig, Digest: oci.Digest(cfg), Size: int64(len(cfg))},
		Layers: []oci.Descriptor{{MediaType: oci.MediaTypeLayer, Digest: oci.Digest(layer.Bytes()), Size: int64(layer.Len())}}})
	ref := oci.Digest(m)
	if tag != "" {
		ref = tag
	}
	if err := dst.WriteManifest(ctx, repo, ref, oci.MediaTypeManifest, m); err != nil {
		t.Fatal(err)
	}
	if tag != "" {
		dst.WriteManifest(ctx, repo, oci.Digest(m), oci.MediaTypeManifest, m)
	}
	return oci.Digest(m)
}

type regEnv struct {
	m        *Mgmt
	srv      *Server
	h        http.Handler
	a, b     Tenant
	upstream *oci.MemTarget
}

const (
	addrHostA = "100.64.0.1:1" // h1: will serve tenant A
	addrHostB = "100.64.0.2:1" // h2: will serve tenant B
	addrAdmin = "100.64.0.6:1"
	addrEve   = "100.64.0.4:1"
)

func newRegEnv(t *testing.T) *regEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	st, err := OpenStore(ctx, filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cust, _ := OpenFileCustody(filepath.Join(dir, "c"))
	up := &oci.MemTarget{}
	upSrv := httptest.NewServer(memUpstream(up))
	t.Cleanup(upSrv.Close)
	m, err := New(Config{Store: st, Custody: cust, Broker: &Broker{}, IDSecret: []byte("0123456789abcdef"), ReapInterval: time.Hour,
		RegistryDir:      filepath.Join(dir, "registry"),
		RegistryUpstream: func(string) *oci.Client { return &oci.Client{Base: upSrv.URL, HTTP: upSrv.Client()} },
		ImageAliases:     []registry.Alias{{From: "upstream.test/sandbox:1", To: "twd.local/sandbox:1-arm64", Arch: "arm64"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	kf := filepath.Join(dir, "api.env")
	os.WriteFile(kf, []byte("TS_API_KEY=x\n"), 0o600)
	e := &regEnv{m: m, upstream: up}
	if e.a, err = m.CreateTenant(ctx, TenantRequest{ID: "tna", Tailnet: "a.ts.net", MintMode: "apikey", APIKeyFile: kf}); err != nil {
		t.Fatal(err)
	}
	if e.b, err = m.CreateTenant(ctx, TenantRequest{ID: "tnb", Tailnet: "b.ts.net", MintMode: "apikey", APIKeyFile: kf}); err != nil {
		t.Fatal(err)
	}
	for id, node := range map[string]string{"h1": "nH1", "h2": "nH2"} {
		if _, _, err := m.RegisterHost(ctx, RegisterRequest{HostID: id, CapacityNodes: 10, Tiers: []string{"process", "microvm", "containers"}, Arch: "arm64"}, node, ""); err != nil {
			t.Fatal(err)
		}
	}
	e.srv = &Server{M: m}
	whois := func(ctx context.Context, addr string) (WhoIsInfo, error) {
		switch addr {
		case addrHostA:
			return WhoIsInfo{NodeID: "nH1", Tags: []string{HostTag}}, nil
		case addrHostB:
			return WhoIsInfo{NodeID: "nH2", Tags: []string{HostTag}}, nil
		case addrAdmin:
			return WhoIsInfo{NodeID: "nADM", Login: "alice@example.com", Caps: []string{AdminCap}}, nil
		case addrEve:
			return WhoIsInfo{NodeID: "nEVE", Login: "eve@example.com"}, nil
		}
		return WhoIsInfo{}, errors.New("unknown")
	}
	e.h = e.srv.TailnetHandler(whois, e.srv.Handler())
	return e
}

func (e *regEnv) client(addr string) *oci.Client {
	return &oci.Client{Base: "http://twd-mgmt", HTTP: &http.Client{Transport: handlerRT{e.h, addr}}}
}

// deploy deploys a container bundle for tenant t on host (pinned).
func (e *regEnv) deploy(t *testing.T, tn Tenant, host, image string, built map[string]BuiltImages) (DeployResponse, error) {
	t.Helper()
	files := map[string]string{"twd.jsonc": `{"tailnet": {"tags": ["tag:twd-web"]}}`, "index.js": "export default {}",
		"wrangler.jsonc": `{"name": "w", "main": "index.js", "containers": [{"class_name": "Sandbox", "image": "` + image + `"}],
			"durable_objects": {"bindings": [{"name": "SANDBOX", "class_name": "Sandbox"}]}}`}
	return e.m.Deploy(context.Background(), DeployRequest{TenantID: tn.ID, Workload: "w", Host: host, Bundle: makeBundle(t, files), ContainerImages: built, By: "alice@example.com"})
}

// TestRegistryAccess (ADR-032 D3): admins push into a tenant's namespace;
// a host pulls a tenant's images only while it holds a deployment of that
// tenant, never pushes, and always reads "public"; non-admins get nothing;
// a namespace that is not a tenant can't be pushed to.
func TestRegistryAccess(t *testing.T) {
	ctx := context.Background()
	e := newRegEnv(t)
	adm := e.client(addrAdmin)
	dA := pushTestImage(t, adm, "tna/app", "arm64", "v1")
	pushTestImage(t, adm, "public/base", "arm64", "1")
	h1, h2 := e.client(addrHostA), e.client(addrHostB)

	// Before any deployment, neither host may read tenant A's namespace.
	if _, _, _, err := h1.GetManifest(ctx, "tna/app", dA); err == nil || !strings.Contains(err.Error(), "DENIED") {
		t.Fatalf("h1 before deploy: %v", err)
	}
	pushTestImage(t, adm, "tna/box", "arm64", "1")
	depA, err := e.deploy(t, e.a, "h1", "twd.local/tna/box:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.deploy(t, e.b, "h2", "twd.local/sandbox:1-arm64", nil); err == nil {
		t.Fatal("tenant B deployed an image absent from public/")
	}
	pushTestImage(t, adm, "public/sandbox", "arm64", "1-arm64")
	if _, err := e.deploy(t, e.b, "h2", "twd.local/sandbox:1-arm64", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h1.GetManifest(ctx, "tna/app", dA); err != nil {
		t.Fatalf("h1 serves tenant A but can't pull: %v", err)
	}
	if _, _, _, err := h2.GetManifest(ctx, "tna/app", dA); err == nil || !strings.Contains(err.Error(), "DENIED") {
		t.Fatalf("h2 (tenant B only) pulled tenant A's manifest: %v", err)
	}
	if ok, err := h2.BlobExists(ctx, "tna/app", dA); ok || err == nil {
		t.Fatalf("h2 probed tenant A's namespace: %v %v", ok, err)
	}
	for _, h := range []*oci.Client{h1, h2} {
		if _, _, _, err := h.GetManifest(ctx, "public/base", "1"); err != nil {
			t.Fatalf("host can't read public/: %v", err)
		}
		if _, err := h.PutBlobBytes(ctx, "tna/app", []byte("x")); err == nil || !strings.Contains(err.Error(), "DENIED") {
			t.Fatalf("host pushed: %v", err)
		}
	}
	if _, _, _, err := e.client(addrEve).GetManifest(ctx, "public/base", "1"); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("non-admin pulled: %v", err)
	}
	if _, err := adm.PutBlobBytes(ctx, "nosuchtenant/app", []byte("x")); err == nil || !strings.Contains(err.Error(), "not a tenant") {
		t.Fatalf("push into a non-tenant namespace: %v", err)
	}
	// Pushes are audited with the WhoIs actor.
	recs, _ := e.m.store.ListAudit(ctx, AuditFilter{})
	found := false
	for _, r := range recs {
		if r.Kind == "image_push" && r.TenantID == "tna" && strings.Contains(string(r.Detail), "alice@example.com") {
			found = true
		}
	}
	if !found {
		t.Fatal("no image_push audit record for alice's push")
	}
	// Undeploying A ends h1's access.
	if _, err := e.m.Undeploy(ctx, depA.DeploymentID, "test", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h1.GetManifest(ctx, "tna/app", dA); err == nil || !strings.Contains(err.Error(), "DENIED") {
		t.Fatalf("h1 after undeploy: %v", err)
	}
}

// TestDeployPinsDigests: a deploy resolves the upstream tag once and
// records the digest; retagging upstream afterwards changes neither the
// assignment nor what the registry serves; a redeploy picks up the new
// digest. A built image must be in the deploying tenant's namespace.
func TestDeployPinsDigests(t *testing.T) {
	ctx := context.Background()
	e := newRegEnv(t)
	d1 := pushTestImage(t, e.upstream, "app", "arm64", "v1")
	dep, err := e.deploy(t, e.a, "", "upstream.test/app:v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	pin := func() container.Pin {
		as, err := e.m.Assignments(ctx, dep.Host)
		if err != nil || len(as) != 1 || len(as[0].Containers) != 1 || len(as[0].Containers[0].Pins) != 1 {
			t.Fatalf("assignments %+v %v", as, err)
		}
		return as[0].Containers[0].Pins[0]
	}
	p := pin()
	if p.Digest != d1 || p.Arch != "arm64" || p.Repository != "tna/mirror/upstream.test/app" || p.Ref != "upstream.test/app:v1" {
		t.Fatalf("pin %+v (want digest %s)", p, d1)
	}
	d2 := pushTestImage(t, e.upstream, "app", "arm64", "v1") // upstream retags v1
	if got := pin(); got.Digest != d1 {
		t.Fatalf("retag changed the pinned digest: %s", got.Digest)
	}
	if _, _, _, err := e.m.reg.Manifest(ctx, p.Repository, d1); err != nil {
		t.Fatalf("pinned content gone: %v", err)
	}
	v, err := e.m.GetDeployment(ctx, dep.DeploymentID)
	if err != nil || len(v.Containers) != 1 || v.Containers[0].Pins[0].Digest != d1 {
		t.Fatalf("deployment view: %+v %v", v.Containers, err)
	}
	dep2, err := e.deploy(t, e.a, "", "upstream.test/app:v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	dep = dep2
	if got := pin(); got.Digest != d2 {
		t.Fatalf("redeploy pinned %s, want the new %s", got.Digest, d2)
	}

	// Built images (twd deploy): only the tenant's own namespace.
	built := pushTestImage(t, e.client(addrAdmin), "tna/w-sandbox", "arm64", "")
	if _, err := e.deploy(t, e.a, "", "./Dockerfile", map[string]BuiltImages{"Sandbox": {Image: "twd.local/tna/w-sandbox@" + built}}); err != nil {
		t.Fatal(err)
	}
	if got := pin(); got.Repository != "tna/w-sandbox" || got.Digest != built {
		t.Fatalf("built pin %+v", got)
	}
	_, err = e.deploy(t, e.b, "", "./Dockerfile", map[string]BuiltImages{"Sandbox": {Image: "twd.local/tna/w-sandbox@" + built}})
	if err == nil || !strings.Contains(err.Error(), "not in this tenant's registry namespace") {
		t.Fatalf("tenant B deployed tenant A's built image: %v", err)
	}
	_, err = e.deploy(t, e.b, "", "./Dockerfile", nil)
	if err == nil || !strings.Contains(err.Error(), "not built") {
		t.Fatalf("an unbuilt Dockerfile deployed: %v", err)
	}
}

// TestDeployPlacesByPinnedArch: an image that exists only for amd64 is
// placed on the amd64 host of a mixed pool (never handed to the arm64
// host); the arm64 alias makes the stock ref placeable on arm64 too, with
// twd's rebuild.
func TestDeployPlacesByPinnedArch(t *testing.T) {
	ctx := context.Background()
	e := newRegEnv(t)
	if _, _, err := e.m.RegisterHost(ctx, RegisterRequest{HostID: "hx", CapacityNodes: 1, Tiers: []string{"process", "microvm", "containers"}, Arch: "amd64"}, "nHX", ""); err != nil {
		t.Fatal(err)
	}
	pushTestImage(t, e.upstream, "sandbox", "amd64", "1")
	dep, err := e.deploy(t, e.a, "", "upstream.test/sandbox:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if dep.Host != "hx" { // h1/h2 have more capacity, but no image for arm64
		t.Fatalf("placed on %s", dep.Host)
	}
	if _, err := e.deploy(t, e.b, "h1", "upstream.test/sandbox:1", nil); err == nil || !strings.Contains(err.Error(), "tier_unavailable") {
		t.Fatalf("pinned to an arm64 host with an amd64-only image: %v", err)
	}
	// With twd's arm64 rebuild in public/, the alias applies on arm64.
	rb := pushTestImage(t, e.client(addrAdmin), "public/sandbox", "arm64", "1-arm64")
	dep, err = e.deploy(t, e.b, "h1", "upstream.test/sandbox:1", nil)
	if err != nil {
		t.Fatal(err)
	}
	as, _ := e.m.Assignments(ctx, "h1")
	var arm *container.Pin
	for _, a := range as {
		if a.TenantID == "tnb" {
			arm = a.Containers[0].PinFor("upstream.test/sandbox:1", "arm64")
		}
	}
	if arm == nil || arm.Repository != "public/sandbox" || arm.Digest != rb {
		t.Fatalf("alias pin %+v", arm)
	}
}

// TestPublicImages: an operator-marked public base is mirrored once into
// public/ and every tenant's deploy pins that same digest (so hosts cache
// it once); unmarked, the next deploy mirrors it into the tenant.
func TestPublicImages(t *testing.T) {
	ctx := context.Background()
	e := newRegEnv(t)
	d := pushTestImage(t, e.upstream, "base", "arm64", "3")
	ps, err := e.m.AddPublicImage(ctx, "upstream.test/base:3", nil, "alice")
	if err != nil || len(ps) != 1 || ps[0].Digest != d || ps[0].Repository != "public/upstream.test/base" {
		t.Fatalf("add: %+v %v", ps, err)
	}
	for _, tn := range []Tenant{e.a, e.b} {
		if _, err := e.deploy(t, tn, "", "upstream.test/base:3", nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range []string{"h1", "h2"} {
		as, _ := e.m.Assignments(ctx, h)
		for _, a := range as {
			if p := a.Containers[0].Pins[0]; p.Repository != "public/upstream.test/base" || p.Digest != d {
				t.Fatalf("%s pin %+v", a.TenantID, p)
			}
		}
	}
	if err := e.m.RemovePublicImage(ctx, ps[0].ID, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.deploy(t, e.a, "", "upstream.test/base:3", nil); err != nil {
		t.Fatal(err)
	}
	as, _ := e.m.Assignments(ctx, "h1")
	as2, _ := e.m.Assignments(ctx, "h2")
	for _, a := range append(as, as2...) {
		if a.TenantID == "tna" && a.Containers[0].Pins[0].Repository != "tna/mirror/upstream.test/base" {
			t.Fatalf("after unmarking: %+v", a.Containers[0].Pins[0])
		}
	}
	// REST: list and the role gate (hosts read, only admins add).
	for addr, want := range map[string]int{addrAdmin: 200, addrHostA: 200, addrEve: 403} {
		r := httptest.NewRequest("GET", "/v1/images/public", nil)
		r.RemoteAddr = addr
		w := httptest.NewRecorder()
		e.h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("GET /v1/images/public as %s: %d %s", addr, w.Code, w.Body)
		}
	}
	r := httptest.NewRequest("POST", "/v1/images/public", strings.NewReader(`{"ref":"upstream.test/base:3"}`))
	r.RemoteAddr = addrHostA
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("host added a public image: %d", w.Code)
	}
}

// memUpstream serves a MemTarget read-only as an upstream registry.
func memUpstream(m *oci.MemTarget) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/v2/")
		if i := strings.LastIndex(p, "/manifests/"); i >= 0 {
			b, mt, d, err := m.Manifest(r.Context(), p[:i], p[i+len("/manifests/"):])
			if err != nil {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Type", mt)
			w.Header().Set("Docker-Content-Digest", d)
			w.Write(b)
			return
		}
		if i := strings.LastIndex(p, "/blobs/"); i >= 0 {
			rc, err := m.Blob(r.Context(), p[:i], p[i+len("/blobs/"):], -1)
			if err != nil {
				w.WriteHeader(404)
				return
			}
			io.Copy(w, rc)
			return
		}
		w.WriteHeader(200)
	})
}
