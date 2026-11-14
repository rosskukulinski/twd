package hostd

import (
	"net/netip"
	"testing"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"
	"tailscale.com/types/views"
)

func TestMatchPeers(t *testing.T) {
	tags := func(ts ...string) *views.Slice[string] { v := views.SliceOf(ts); return &v }
	peer := func(host, dns, ip string, tg *views.Slice[string]) *ipnstate.PeerStatus {
		return &ipnstate.PeerStatus{HostName: host, DNSName: dns, TailscaleIPs: []netip.Addr{netip.MustParseAddr(ip)}, Tags: tg}
	}
	st := &ipnstate.Status{Peer: map[key.NodePublic]*ipnstate.PeerStatus{
		key.NewNode().Public(): peer("pg-1", "pg-1.ttest.ts.net.", "100.64.0.1", tags("tag:db")),
		key.NewNode().Public(): peer("pg-2", "pg-2.ttest.ts.net.", "100.64.0.2", tags("tag:db", "tag:replica")),
		key.NewNode().Public(): peer("api", "api.ttest.ts.net.", "100.64.0.3", nil),
	}}
	for _, tc := range []struct {
		target string
		want   []string
	}{
		{"tag:db", []string{"pg-1", "pg-2"}},
		{"tag:replica", []string{"pg-2"}},
		{"tag:none", nil},
		{"api", []string{"api"}},
		{"API.ttest.ts.net", []string{"api"}},
		{"api.ttest.ts.net.:8080", []string{"api"}},
		{"100.64.0.1", []string{"pg-1"}},
		{"100.64.0.1:5432", []string{"pg-1"}},
		{"pg", nil}, // no prefix guessing
	} {
		var got []string
		for _, p := range matchPeers(st, tc.target) {
			got = append(got, p.HostName)
		}
		if len(got) != len(tc.want) || (len(got) > 0 && got[0] != tc.want[0]) {
			t.Errorf("%s: %v, want %v", tc.target, got, tc.want)
		}
	}
}
