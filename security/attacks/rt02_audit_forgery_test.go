package attacks

// RT-02 regression test (High): the host API `audit` op (hostd/http.go,
// case "audit") used to let tenant code (ctx.tailnet.audit) submit
// allow|deny|egress records whose `traceID`, `actingFor`, `grant`, `code`
// and `detail` were copied verbatim into the SAME hash chain as platform
// security records. A prompt-injected agent could fabricate an "allow" for a
// cap it never had, frame a human via actingFor, and re-point its records
// into a victim's trace while `chain verified` stayed true.
//
// Fixed invariant: tenant code can only produce records of kind
// `tenant_note` with source `tenant`; the platform assigns the object, the
// trace ID and the acting principal; the tenant's claims survive only
// inside the note's detail (claimedKind, parentHint). twd-mgmt refuses any
// host-submitted record whose source is "tenant" with another kind, and
// `twd audit` renders tenant notes distinctly. This test calls the real
// sanitizer hostd uses (hostd.TenantAuditRecord) and mgmt's ingest check.

import (
	"strings"
	"testing"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/mgmt"
)

func TestRT02_AppAuditCannotForgeAuthorization(t *testing.T) {
	forged := hostd.TenantAuditRecord("obj-attacker", "trace-REAL", "alice@tenant.example", map[string]any{
		"kind":      "allow",
		"grant":     "example.com/cap/mcp: refund", // a cap it was never granted
		"actingFor": "ceo@victim.example",          // frame a human
		"traceID":   "trace-of-victim",             // cross-link into a victim trace
		"detail":    "wired $1M",
	})

	// 1. Provenance: never a privileged kind, always marked as tenant content.
	if forged.Kind != mgmt.KindTenantNote || forged.Source != mgmt.SourceTenant {
		t.Fatalf("RT-02: tenant-submitted record is kind %q source %q; it must be %q/%q", forged.Kind, forged.Source, mgmt.KindTenantNote, mgmt.SourceTenant)
	}
	// 2. The tenant cannot re-point its record at another trace or principal.
	if forged.TraceID != "trace-REAL" {
		t.Fatalf("RT-02: tenant overrode the trace ID to %q, polluting a victim's audit trail", forged.TraceID)
	}
	if forged.ActingFor != "alice@tenant.example" {
		t.Fatalf("RT-02: tenant set actingFor to %q", forged.ActingFor)
	}
	if forged.Grant != "" || forged.Code != "" || forged.Dst != "" {
		t.Fatalf("RT-02: tenant controlled platform fields: %+v", forged)
	}
	// 3. What it claimed is kept, visibly as a claim.
	if !strings.Contains(forged.Detail, `"claimedKind":"allow"`) || !strings.Contains(forged.Detail, `"parentHint":"trace-of-victim"`) ||
		!strings.Contains(forged.Detail, "ceo@victim.example") {
		t.Fatalf("RT-02: the note lost the tenant's claims (needed for forensics): %s", forged.Detail)
	}

	// 4. mgmt's ingest contract: a "tenant" source may carry only a
	// tenant_note; a tenant_note must say it is tenant-sourced; nothing a
	// host submits may claim to be mgmt.
	for _, c := range []struct {
		source, kind string
		ok           bool
	}{
		{"tenant", "tenant_note", true},
		{"tenant", "allow", false},
		{"tenant", "deny", false},
		{"tenant", "kill", false},
		{"tenant", "spawn", false},
		{"tenant", "egress", false},
		{"host", "tenant_note", false},
		{"", "tenant_note", false},
		{"host", "deny", true},
		{"", "allow", true},
		{"mgmt", "kill", false},
		{"attacker", "allow", false},
	} {
		got := mgmt.CheckAuditSourceForTest(c.source, c.kind) == nil
		if got != c.ok {
			t.Fatalf("RT-02: ingest of source %q kind %q: accepted=%v, want %v", c.source, c.kind, got, c.ok)
		}
	}
}
