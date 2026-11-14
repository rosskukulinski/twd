package gen_test

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/rosskukulinski/twd/hostd/hostdtest"
)

// getPublic sends a request as netd's public (Funnel) listener would: a cf
// blob with tailscale.requester {kind: public} and NO caller (ADR-029).
func (in *inst) getPublic(path string, twd map[string]any) (int, http.Header, map[string]any) {
	req, _ := http.NewRequest("GET", "http://wl.ttest.ts.net"+path, nil)
	blob := map[string]any{
		"tailscale": map[string]any{"requester": map[string]any{"kind": "public", "ip": "203.0.113.9", "port": 51000, "proto": "https", "host": "wl.ttest.ts.net"}},
		"objectId":  "o1", "twd": twd, "traceId": "tr-pub",
	}
	b, _ := json.Marshal(blob)
	req.Header.Set("x-twd-cf", string(b))
	resp, err := in.hc.Do(req)
	if err != nil {
		return 0, nil, map[string]any{"err": err.Error()}
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if json.Unmarshal(rb, &out) != nil {
		out = map[string]any{"raw": string(rb)}
	}
	return resp.StatusCode, resp.Header, out
}

// opsFor returns the hostd ops that named class/name (in "as" or top level).
func (in *inst) opsFor(class, name string) []map[string]any {
	in.f.mu.Lock()
	defer in.f.mu.Unlock()
	var out []map[string]any
	for _, o := range in.f.ops {
		if as, ok := o["as"].(map[string]any); ok && as["class"] == class && as["name"] == name {
			out = append(out, o)
		} else if o["class"] == class && o["name"] == name {
			out = append(out, o)
		}
	}
	return out
}

func hasOp(ops []map[string]any, op string, pred func(map[string]any) bool) bool {
	for _, o := range ops {
		if o["op"] == op && (pred == nil || pred(o)) {
			return true
		}
	}
	return false
}

func TestGeneratedWrapperPublic(t *testing.T) {
	in := start(t, hostdtest.Fixture("probe"), nil)

	t.Run("TwdPathNeverPublic", func(t *testing.T) {
		code, h, r := in.getPublic("/_twd/o/Probe/pz/", wl)
		if code != 404 || h.Get("twd-error") != "not_public" {
			t.Fatalf("%d %v", code, r)
		}
		if ops := in.opsFor("Probe", "pz"); len(ops) != 0 {
			t.Fatalf("hostd saw %v", ops)
		}
	})
	t.Run("OnlyWorkloadRoute", func(t *testing.T) {
		code, h, r := in.getPublic("/", obj("Probe", "pub-ok-direct"))
		if code != 403 || h.Get("twd-error") != "public_request" {
			t.Fatalf("%d %v", code, r)
		}
	})
	t.Run("GetCallerFailsClosed", func(t *testing.T) {
		code, h, r := in.getPublic("/", wl)
		if code != 403 || h.Get("twd-error") != "public_request" {
			t.Fatalf("%d %v", code, r)
		}
	})
	t.Run("NewObjectRefusedBeforeCreation", func(t *testing.T) {
		code, h, r := in.getPublic("/probe/fresh1/env", wl)
		if code != 403 || h.Get("twd-error") != "public_create_denied" {
			t.Fatalf("%d %v", code, r)
		}
		ops := in.opsFor("Probe", "fresh1")
		if !hasOp(ops, "admit", nil) {
			t.Fatalf("no admit: %v", ops)
		}
		if hasOp(ops, "ensure", nil) {
			t.Fatalf("object was ensured (created) for a refused public request: %v", ops)
		}
	})
	t.Run("RPCToNewObjectRefused", func(t *testing.T) {
		code, h, r := in.getPublic("/rpc?name=fresh2", wl)
		if code != 403 || h.Get("twd-error") != "public_create_denied" {
			t.Fatalf("%d %v", code, r)
		}
		if ops := in.opsFor("Probe", "fresh2"); hasOp(ops, "ensure", nil) {
			t.Fatalf("object was ensured: %v", ops)
		}
	})
	t.Run("ExistingObjectReached", func(t *testing.T) {
		code, _, r := in.getPublic("/probe/pub-ok-1/env", wl)
		if code != 200 || r["hasTailnet"] != true {
			t.Fatalf("%d %v", code, r)
		}
		ops := in.opsFor("Probe", "pub-ok-1")
		if !hasOp(ops, "admit", nil) {
			t.Fatalf("no admit: %v", ops)
		}
		// The Host DO re-checks with hostd before the facet runs.
		if !hasOp(ops, "ensure", func(o map[string]any) bool { return o["public"] == true }) {
			t.Fatalf("no ensure{public:true} from the Host DO: %v", ops)
		}
		// actingFor is never a caller name for a public request.
		for _, o := range ops {
			if af, ok := o["actingFor"].(string); ok && af != "" && af != "public" {
				t.Fatalf("actingFor %q on a public request", af)
			}
		}
	})
	t.Run("HostDORechecksWithHostd", func(t *testing.T) {
		// The fake admits "pub-adm*" but refuses ensure{public:true} for
		// it, as hostd would if the tenant-side guard had been bypassed:
		// the Host DO refuses before the facet runs (no wake ensure).
		code, h, r := in.getPublic("/probe/pub-adm-1/env", wl)
		if code != 403 || h.Get("twd-error") != "public_create_denied" {
			t.Fatalf("%d %v", code, r)
		}
		ops := in.opsFor("Probe", "pub-adm-1")
		if !hasOp(ops, "ensure", func(o map[string]any) bool { return o["public"] == true }) {
			t.Fatalf("no ensure{public:true}: %v", ops)
		}
		if hasOp(ops, "ensure", func(o map[string]any) bool { return o["public"] != true }) {
			t.Fatalf("facet woke (non-public ensure) despite the refusal: %v", ops)
		}
	})
	t.Run("TailnetUnchanged", func(t *testing.T) {
		code, _, r := in.get("/probe/fresh4/env", wl)
		if code != 200 {
			t.Fatalf("%d %v", code, r)
		}
		if ops := in.opsFor("Probe", "fresh4"); hasOp(ops, "admit", nil) {
			t.Fatalf("tailnet request was admitted: %v", ops)
		}
	})
}
