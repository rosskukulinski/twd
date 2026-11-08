package hostd

import (
	"context"
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
