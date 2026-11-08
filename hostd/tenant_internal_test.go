package hostd

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/rosskukulinski/twd/netd"
)

// A tenant's netd registry only ever gets keys for its own objects.
func TestKeySourceRefusesOtherTenantsObjects(t *testing.T) {
	h := &Host{byNetd: map[string]*object{"ob": {ID: "ob", TenantID: "tnB", Workload: "agent"}},
		workloads: map[string]*workload{"tnB/agent": {}}}
	_, err := (&keySource{h: h, tenantID: "tnA"}).AuthKey(context.Background(), netd.KeyRequest{ObjectID: "ob"})
	if err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("tenant A's key source fetched a key for tenant B's object: %v", err)
	}
}

func TestTenantIDsAreSafePathElements(t *testing.T) {
	for id, ok := range map[string]bool{"tn_tail7906d5": true, "tn_a-b.c": true, "..": false, "a/b": false, "": false, "../x": false, "a..b": false} {
		if got := tenantIDRE.MatchString(id) && !strings.Contains(id, ".."); got != ok {
			t.Errorf("%q: safe=%v, want %v", id, got, ok)
		}
	}
}

func TestSeatbeltProfile(t *testing.T) {
	dir := t.TempDir()
	p := SeatbeltProfile([]string{dir + "/run/i1", dir + "/data/tenants/A/workloads/w/do"}, []string{"/opt/workerd"},
		[]string{dir + "/data", dir + "/run", "", dir + "/data"})
	rd := realPath(dir)
	lines := strings.Split(strings.TrimSpace(p), "\n")
	idx := func(s string) int {
		for i, l := range lines {
			if strings.Contains(l, s) {
				return i
			}
		}
		t.Fatalf("profile lacks %q:\n%s", s, p)
		return -1
	}
	deny := idx(`(deny file-read* file-write* (subpath "` + rd + `/data"))`)
	allow := idx(`(allow file-read* file-write* (subpath "` + rd + `/data/tenants/A/workloads/w/do"))`)
	idx(`(deny network-outbound (remote unix-socket (subpath "` + rd + `/run")))`)
	idx(`(allow network-outbound (remote unix-socket (subpath "` + rd + `/run/i1")))`)
	idx(`(allow file-read* (literal "/opt/workerd"))`)
	idx(`(allow file-read-metadata (literal "` + rd + `/data/tenants"))`)
	if allow < deny {
		t.Fatal("own dirs must be allowed after the denies (last match wins)")
	}
	if strings.Count(p, `(deny file-read* file-write* (subpath "`+rd+`/data"))`) != 1 {
		t.Fatal("duplicate deny")
	}
	if strings.HasPrefix(dir, "/var/") && !strings.HasPrefix(rd, "/private/") {
		t.Fatalf("symlinks not resolved: %s", rd)
	}
}

// The Node tier's profile: the node install prefix is readable (nvm puts it
// under the denied home dir), never writable, and denies still precede it.
func TestSeatbeltProfileNodePrefix(t *testing.T) {
	dir := realPath(t.TempDir())
	bin := dir + "/home/.nvm/versions/node/v25/bin/node"
	if got := nodePrefix(bin); len(got) != 1 || got[0] != dir+"/home/.nvm/versions/node/v25" {
		t.Fatalf("nodePrefix(%s) = %v", bin, got)
	}
	for _, b := range []string{"/usr/bin/node", "/node", "/opt/node/node"} {
		if got := nodePrefix(b); got != nil {
			t.Fatalf("nodePrefix(%s) = %v, want none", b, got)
		}
	}
	p := seatbeltProfile([]string{dir + "/run/i2", dir + "/data/tenants/A/workloads/w/node"}, []string{bin}, nodePrefix(bin),
		[]string{dir + "/home", dir + "/data", dir + "/run"})
	lines := strings.Split(p, "\n")
	at := func(s string) int {
		for i, l := range lines {
			if l == s {
				return i
			}
		}
		t.Fatalf("profile lacks %q:\n%s", s, p)
		return -1
	}
	deny := at(`(deny file-read* file-write* (subpath "` + dir + `/home"))`)
	tree := at(`(allow file-read* (subpath "` + dir + `/home/.nvm/versions/node/v25"))`)
	at(`(allow file-read* file-write* (subpath "` + dir + `/data/tenants/A/workloads/w/node"))`)
	at(`(allow file-read-metadata (literal "` + dir + `/home/.nvm/versions"))`)
	if tree < deny {
		t.Fatal("the node prefix must be allowed after the denies")
	}
	if strings.Contains(p, `file-write* (subpath "`+dir+`/home/.nvm`) {
		t.Fatal("node prefix must be read-only")
	}
}

// The Landlock profile shape (Linux): sandboxOf maps a recorded profile to
// its sandbox, and "landlock" is refused where the kernel lacks it.
func TestSandboxKinds(t *testing.T) {
	for p, want := range map[string]string{"/r/i1-ab/sandbox.sb": SandboxSeatbelt, "/r/i1-ab/sandbox.json": SandboxLandlock, "": SandboxNone} {
		if got := sandboxOf(p); got != want {
			t.Fatalf("sandboxOf(%q) = %q, want %q", p, got, want)
		}
	}
	sb, err := ResolveSandbox(SandboxLandlock)
	if runtime.GOOS != "linux" && err == nil {
		t.Fatalf("landlock resolved on %s: %q", runtime.GOOS, sb)
	}
	h := &Host{cfg: Config{RunDir: "/r"}}
	a, b := h.instanceDir(1), h.instanceDir(1)
	if a == b || !strings.HasPrefix(a, "/r/i1-") || len(filepath.Base(a)) != len("i1-")+16 {
		t.Fatalf("instance dirs not unguessable: %s %s", a, b)
	}
}
