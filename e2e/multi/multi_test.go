//go:build e2emulti

// E2E-MULTI (BUILD-PLAN definition of done 2) on this machine: one twd host
// (process tier) serves tenant A on tailnet 1 (`twd up`) and tenant B on
// tailnet-2 (`twd tenant add`), with the same agent app deployed to both;
// e2e/cliflow MultiEnv checks (a)–(f), then `twd down` and zero leftovers
// in both tailnets.
//
// Safety: only tailnet 1 and tailnet-2 (allow list; tailnet-2's name is
// discovered and checked before anything is written). Policy additions
// (cliflow.MultiPolicy) are validated and written with If-Match, keeping
// every existing rule, with a pre-change copy saved. Cleanup deletes exactly
// the IDs this run created (prefix ttest-m7-<run>). Keys are never printed.
package multi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rosskukulinski/twd/e2e/cliflow"
	"github.com/rosskukulinski/twd/internal/policy"
	"github.com/rosskukulinski/twd/internal/tsapi"
	"github.com/rosskukulinski/twd/netd"
	"github.com/rosskukulinski/twd/netd/nettest"
)

const prodControl = "https://controlplane.tailscale.com"

// The two test tailnets. Ross's personal tailnet is never touched.
var (
	allowedA  = map[string]bool{"tail7906d5.ts.net": true}
	allowedB  = map[string]bool{"tail2c3a48.ts.net": true}
	forbidden = []string{"stonecat-wage.ts.net", "kukulinski.com"}
)

type side struct {
	key, keyFile, tailnet string
	api                   *tsapi.Client
	minter                *tsapi.Minter
	tracker               *tsapi.Tracker
	res                   *cliflow.Results
	nodes                 []*nettest.Node
	pre                   map[string]*nettest.Node // role → node started before Run
}

func TestE2EMulti(t *testing.T) {
	kfA, kfB := os.Getenv("TWD_E2E_API_KEY_FILE"), os.Getenv("TWD_E2E_API_KEY_FILE_B")
	tailnetA := os.Getenv("TWD_E2E_TAILNET")
	if kfA == "" || kfB == "" || tailnetA == "" {
		t.Skip("TWD_E2E_API_KEY_FILE / TWD_E2E_API_KEY_FILE_B / TWD_E2E_TAILNET not set")
	}
	if !allowedA[tailnetA] {
		t.Fatalf("refusing to run against tailnet %q (not in the allow list)", tailnetA)
	}
	wd, err := nettest.WorkerdBinary()
	if err != nil {
		t.Skip(err)
	}
	t.Setenv("TWD_WORKERD", wd)
	b := make([]byte, 3)
	rand.Read(b)
	run := hex.EncodeToString(b)
	prefix := "ttest-m7-" + run + "-"
	if p := os.Getenv("TWD_E2E_PREFIX"); p != "" {
		prefix = p + run + "-"
	}
	root, _ := nettest.FindRepoRoot()
	resultsTo := filepath.Join(root, "e2e", "real", "results", "m7-"+time.Now().Format("20060102-150405")+"-"+run+".json")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	quiet := slog.New(netd.NewRedactingHandler(slog.NewTextHandler(io.Discard, nil)))
	work := t.TempDir()

	newSide := func(key, keyFile, tailnet string) *side {
		k, err := tsapi.LoadEnvFile(keyFile, "TS_API_KEY")
		if err != nil {
			t.Fatal(err)
		}
		api := &tsapi.Client{Tailnet: tailnet, APIKey: k}
		return &side{key: key, keyFile: keyFile, tailnet: tailnet, api: api, minter: tsapi.MintSource(api, quiet),
			tracker: tsapi.NewTracker(api), pre: map[string]*nettest.Node{}}
	}
	A := newSide("a", kfA, tailnetA)
	// Tailnet-2's name: the API with "-" (the key's own tailnet), checked
	// against the allow list before anything is written there.
	B := newSide("b", kfB, "-")
	startNode := func(s *side, role string) *nettest.Node {
		k, err := s.minter.AuthKey(ctx, netd.KeyRequest{ObjectID: role, Tags: []string{cliflow.TestTag}, Ephemeral: true})
		if err != nil {
			t.Fatalf("mint %s: %v", role, netd.RedactError(err))
		}
		n, err := nettest.NewNode(ctx, prodControl, k, prefix+role, filepath.Join(work, role))
		if err != nil {
			t.Fatalf("%s: %v", role, netd.RedactError(err))
		}
		s.nodes = append(s.nodes, n)
		if st, err := n.LC.StatusWithoutPeers(ctx); err == nil && st.Self != nil {
			s.tracker.Track(string(st.Self.ID), role)
		}
		return n
	}
	var notes []string
	var downDone bool
	var m *cliflow.MultiEnv
	state := filepath.Join("/tmp", "twd-e2e-m7-"+run)
	cleanup := func() {
		if m != nil && !downDone {
			if _, se, _, err := m.Twd("down", "--purge"); err != nil {
				t.Logf("twd down (cleanup): %v %s", err, netd.Redact(se))
			}
		}
		if t.Failed() {
			for _, c := range []string{"supervisor", "mgmt", "hostd"} {
				b, _ := os.ReadFile(filepath.Join(state, "logs", c+".log"))
				t.Logf("%s log (tail):\n%s", c, netd.Redact(string(b[max(0, len(b)-6000):])))
			}
		}
		cctx, ccancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer ccancel()
		summary := map[string]any{}
		for _, s := range []*side{A, B} {
			for _, n := range s.nodes {
				n.Close()
			}
			if s.res == nil {
				continue
			}
			devs, keys := cliflow.Leftovers(cctx, s.api, s.res)
			c := s.res.Cleanup
			c["twd_devices_tracked"], c["twd_keys_tracked"] = len(s.res.NodeIDs), len(s.res.KeyIDs)
			c["twd_devices_left_after_down"], c["twd_keys_valid_after_down"] = len(devs), len(keys)
			for _, id := range s.res.NodeIDs {
				s.tracker.Track(id, "twd")
			}
			crep := s.tracker.Cleanup(cctx, prefix)
			for _, k := range keys {
				s.api.DeleteKey(cctx, k)
			}
			var revErrs []string
			if err := s.minter.RevokeAll(cctx); err != nil {
				revErrs = append(revErrs, netd.Redact(err.Error()))
			}
			c["devices_tracked"], c["devices_deleted_by_cleanup"], c["devices_left"] = crep.Tracked, crep.Deleted, crep.Left
			c["untracked_run_devices"], c["test_keys_minted"], c["test_keys_revoke_errors"] = crep.Untracked, len(s.minter.KeyIDs()), revErrs
			summary[s.tailnet] = s.res
			if len(devs) > 0 || len(keys) > 0 {
				t.Errorf("%s after twd down: leftover twd devices %v, valid keys %d", s.tailnet, devs, len(keys))
			}
			if crep.Left != 0 || len(crep.Untracked) > 0 {
				t.Errorf("%s cleanup left devices: %+v", s.tailnet, crep)
			}
		}
		os.RemoveAll(state)
		out := map[string]any{"run": "m7-" + run, "notes": notes, "tenants": summary}
		if m != nil {
			out["checks"], out["numbers"], out["notes"] = m.Res.Checks, m.Res.Numbers, append(notes, m.Res.Notes...)
		}
		os.MkdirAll(filepath.Dir(resultsTo), 0o755)
		js, _ := json.MarshalIndent(out, "", "  ")
		js = []byte(netd.Redact(string(js)))
		os.WriteFile(resultsTo, js, 0o644)
		t.Logf("results: %s\n%s", resultsTo, js)
	}
	t.Cleanup(cleanup)

	// Tailnet-2's DNS name: from its device list, or (an empty tailnet)
	// from the first test node we join with an API-minted key.
	var tailnetB string
	if devs, err := B.api.ListDevices(ctx); err == nil {
		for _, d := range devs {
			if _, suf, ok := strings.Cut(d.Name, "."); ok && strings.HasSuffix(suf, ".ts.net") {
				tailnetB = suf
				break
			}
		}
	}
	bClient := startNode(B, "b-client")
	st, _ := bClient.LC.StatusWithoutPeers(ctx)
	_, joined, _ := strings.Cut(strings.TrimSuffix(st.Self.DNSName, "."), ".")
	if tailnetB != "" && tailnetB != joined {
		t.Fatalf("tailnet-2 name mismatch: devices say %s, the joined node says %s", tailnetB, joined)
	}
	tailnetB = joined
	for _, f := range forbidden {
		if strings.Contains(tailnetB, f) {
			t.Fatalf("tailnet-2 resolved to %s: refusing (personal tailnet)", tailnetB)
		}
	}
	if !allowedB[tailnetB] || tailnetB == tailnetA {
		t.Fatalf("tailnet-2 resolved to %q: not in the allow list", tailnetB)
	}
	B.tailnet, B.api.Tailnet = tailnetB, tailnetB
	B.pre["b-client"] = bClient
	notes = append(notes, "tailnet-2 discovered: "+tailnetB+" (from the API-minted test client's MagicDNS name)")
	A.res, B.res = cliflow.NewResults("m7-"+run+"-A", tailnetA), cliflow.NewResults("m7-"+run+"-B", tailnetB)

	// Policy: add what the scenario needs to each tailnet (no-op if present).
	for _, s := range []*side{A, B} {
		ar, err := policy.ApplyWith(ctx, s.api, cliflow.MultiPolicy(), policy.Options{AllowTags: []string{cliflow.TestTag}})
		if err != nil {
			t.Fatalf("policy %s: %v", s.tailnet, netd.RedactError(err))
		}
		if ar.Changed {
			name := map[string]string{"a": "tailnet1", "b": "tailnet2"}[s.key]
			bk := filepath.Join(os.Getenv("HOME"), ".config", "tailnet-agents-design", name+"_acl.pre-m7-"+run+".hujson")
			if err := os.WriteFile(bk, ar.Before, 0o600); err != nil {
				t.Fatal(err)
			}
			notes = append(notes, s.tailnet+": policy changed (validated, If-Match "+ar.OldETag+"): added "+ar.Added.HuJSON()+" (previous copy: "+bk+")")
		} else {
			notes = append(notes, s.tailnet+": policy already had the scenario's tagOwners and grants (not written)")
		}
	}

	bin := cliflow.BuildBinaries(t)
	mk := func(s *side) *cliflow.Tenant {
		return &cliflow.Tenant{Tailnet: s.tailnet, APIKeyFile: s.keyFile, ControlURL: prodControl, Res: s.res,
			NewNode: func(role string) *nettest.Node {
				if n := s.pre[role]; n != nil {
					return n
				}
				return startNode(s, role)
			}}
	}
	m = &cliflow.MultiEnv{Env: &cliflow.Env{T: t, BinDir: bin, StateDir: state, WorkDir: work, Prefix: prefix,
		ClientTagged: true, PolicyGrantsParentRefund: true, Res: cliflow.NewResults("m7-"+run, tailnetA+"+"+tailnetB)},
		A: mk(A), B: mk(B), RequireSandbox: true, GlobalNodeIDs: true}
	m.Run()
	m.Down()
	downDone = true
	time.Sleep(2 * time.Second) // device deletes propagate asynchronously
}
