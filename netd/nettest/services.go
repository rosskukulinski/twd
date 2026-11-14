package nettest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
)

// HostService makes the local control behave as the real control plane does
// for an approved Tailscale Service host: the node gets the service-host
// capability (name → VIP), the VIP is routed to it, it is tagged, and the
// Service name resolves to the VIP in every node's MagicDNS extra records.
// Clients must also accept routes (AcceptRoutes): testcontrol models the VIP
// as a subnet route.
func (c *Control) HostService(ctx context.Context, lc *local.Client, name, vip string) error {
	st, err := lc.StatusWithoutPeers(ctx)
	if err != nil {
		return err
	}
	return c.hostService(st.Self.PublicKey, name, vip)
}

// HostServiceByName is HostService for the node with the given hostname
// (for nodes the test has no LocalClient of, e.g. hostd's workload nodes).
func (c *Control) HostServiceByName(hostname, name, vip string) error {
	for _, n := range c.Server.AllNodes() {
		if n.Hostinfo.Valid() && n.Hostinfo.Hostname() == hostname {
			return c.hostService(n.Key, name, vip)
		}
	}
	return fmt.Errorf("nettest: no node with hostname %q", hostname)
}

func (c *Control) hostService(nk key.NodePublic, name, vip string) error {
	n := c.Server.Node(nk)
	if n == nil {
		return fmt.Errorf("nettest: node %s unknown to control", nk.ShortString())
	}
	ipMap := tailcfg.ServiceIPMappings{}
	cm := tailcfg.NodeCapMap{}
	for k, v := range n.CapMap {
		cm[k] = v
	}
	if raw, ok := cm[tailcfg.NodeAttrServiceHost]; ok && len(raw) == 1 {
		json.Unmarshal([]byte(raw[0]), &ipMap)
	}
	ipMap[tailcfg.ServiceName(name)] = []netip.Addr{netip.MustParseAddr(vip)}
	b, _ := json.Marshal(ipMap)
	cm[tailcfg.NodeAttrServiceHost] = []tailcfg.RawMessage{tailcfg.RawMessage(b)}
	c.Server.SetNodeCapMap(nk, cm)

	var routes []netip.Prefix
	for _, p := range n.AllowedIPs {
		if !containsPrefix(n.Addresses, p) {
			routes = append(routes, p)
		}
	}
	routes = append(routes, netip.MustParsePrefix(vip+"/32"))
	c.Server.SetSubnetRoutes(nk, routes)
	if len(n.Tags) == 0 {
		n = c.Server.Node(nk)
		n.Tags = append(n.Tags, "tag:ttest-svc-host")
		c.Server.UpdateNode(n)
	}
	c.Server.AddDNSRecords(tailcfg.DNSRecord{Name: strings.TrimPrefix(name, "svc:") + "." + MagicDNSDomain, Value: vip})
	return nil
}

func containsPrefix(ps []netip.Prefix, p netip.Prefix) bool {
	for _, x := range ps {
		if x == p {
			return true
		}
	}
	return false
}

// AcceptRoutes turns on RouteAll (testcontrol delivers Service VIPs as
// subnet routes).
func AcceptRoutes(ctx context.Context, lc *local.Client) error {
	_, err := lc.EditPrefs(ctx, &ipn.MaskedPrefs{RouteAllSet: true, Prefs: ipn.Prefs{RouteAll: true}})
	return err
}

// WaitDNSRecord waits until the node's netmap has a DNS extra record for vip.
func WaitDNSRecord(ctx context.Context, lc *local.Client, vip string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		if dc, err := lc.DNSConfig(ctx); err == nil && dc != nil {
			for _, r := range dc.ExtraRecords {
				if r.Value == vip {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("nettest: no DNS record for %s: %w", vip, ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}
