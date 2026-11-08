package attacks

// RT-02 regression test (High): the host API `audit` op
// (hostd/http.go, case "audit") lets tenant code submit audit records via
// ctx.tailnet.audit([...]). ObjectID is forced to the attested caller (good),
// but for kinds allow|deny|egress the handler copies attacker-controlled
// `traceID`, `actingFor`, `dst`, `grant`, `code`, and `detail` verbatim into
// the SAME hash chain as platform-generated security records:
//
//     if tid, _ := rec["traceID"].(string); tid != "" { ar.TraceID = tid }
//     ar.ActingFor, _ = rec["actingFor"].(string)
//     ar.Grant, _  = rec["grant"].(string)
//     ar.Code, _   = rec["code"].(string)
//
// A prompt-injected agent can therefore fabricate "allow" records with a
// forged grant to make an exfiltration look authorized, frame a human via
// `actingFor`, and inject fake/foreign trace IDs so `twd audit --trace <id>`
// cross-links its fabricated entries into a victim principal's trace. The hash
// chain stays valid, so `chain verified` is TRUE while the content is forged.
//
// Desired invariant (post-fix), encoded below: an app-submitted record must be
// provenance-marked (e.g. Kind carries an "app." namespace or a dedicated
// Source field) so it can never be confused with a platform authorization
// decision, and a caller-supplied traceID must not silently override the
// request's real trace. Skip-marked with the finding ID.

import (
	"strings"
	"testing"

	mgmtapi "github.com/rosskukulinski/twd/mgmt"
)

// appAudit mirrors exactly what hostd/http.go does with a tenant-submitted
// record of kind "allow" (the vulnerable field copy). It is reproduced here
// because the handler is unexported; the real fix must change the handler.
func appAudit(objectID, realTraceID string, rec map[string]any) mgmtapi.AuditRecord {
	kind, _ := rec["kind"].(string)
	ar := mgmtapi.AuditRecord{ObjectID: objectID, Kind: kind, TraceID: realTraceID}
	ar.Dst, _ = rec["dst"].(string)
	ar.Grant, _ = rec["grant"].(string)
	ar.Code, _ = rec["code"].(string)
	ar.ActingFor, _ = rec["actingFor"].(string)
	if tid, _ := rec["traceID"].(string); tid != "" {
		ar.TraceID = tid // attacker overrides the real trace
	}
	return ar
}

func TestRT02_AppAuditCannotForgeAuthorization(t *testing.T) {
	t.Skip("RT-02: app audit records accept attacker traceID/actingFor/grant/code and are indistinguishable from platform records. Remove skip to reproduce.")

	forged := appAudit("obj-attacker", "trace-REAL", map[string]any{
		"kind":      "allow",
		"grant":     "example.com/cap/mcp: refund", // a cap it was never granted
		"actingFor": "ceo@victim.example",          // frame a human
		"traceID":   "trace-of-victim",             // cross-link into a victim trace
		"detail":    "wired $1M",
	})

	// DESIRED post-fix behaviour:
	// 1. app records are provenance-namespaced so they can't masquerade as
	//    platform authorization decisions.
	if !strings.HasPrefix(forged.Kind, "app.") {
		t.Fatalf("RT-02: app-submitted record kind %q is not provenance-marked; it is indistinguishable from a platform %q record", forged.Kind, forged.Kind)
	}
	// 2. a caller must not be able to re-point its record at another trace.
	if forged.TraceID != "trace-REAL" {
		t.Fatalf("RT-02: tenant overrode the trace ID to %q, polluting a victim's audit trail", forged.TraceID)
	}
}
