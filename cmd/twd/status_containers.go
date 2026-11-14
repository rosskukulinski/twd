package main

import (
	"context"
	"fmt"

	"github.com/rosskukulinski/twd/hostd"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/mgmt"
)

// hostCapacity is one host's capacity vector and live usage (ADR-032 D9).
type hostCapacity struct {
	HostID   string             `json:"hostID"`
	Arch     string             `json:"arch,omitempty"`
	Tiers    []string           `json:"tiers"`
	Status   string             `json:"status"`
	Capacity *mgmt.HostCapacity `json:"capacity,omitempty"`
	Usage    *mgmt.HostUsage    `json:"usage,omitempty"`
}

// containerState reads every host's capacity vector and the container
// reservations (tenant "" = all) from mgmt. Errors leave them empty:
// status reports what it can.
func containerState(ctx context.Context, mc *mgmtapi.Client, tenant string) ([]hostCapacity, []mgmt.ContainerReservation) {
	hosts, _ := listAll[hostCapacity](ctx, mc, "/v1/hosts", 0)
	path := "/v1/container-reservations"
	if tenant != "" {
		path += "?tenant=" + tenant
	}
	res, _ := listAll[mgmt.ContainerReservation](ctx, mc, path, 0)
	return hosts, res
}

// printContainers is the containers part of `twd status`.
func printContainers(hosts []hostCapacity, res []mgmt.ContainerReservation, local []hostd.ContainerStatus, cacheBytes int64) {
	for _, h := range hosts {
		if h.Capacity == nil {
			continue
		}
		c, u := h.Capacity, h.Usage
		if u == nil {
			u = &mgmt.HostUsage{}
		}
		offers := "no containers"
		for _, t := range h.Tiers {
			if t == mgmt.CapContainers {
				offers = "containers"
			}
		}
		fmt.Printf("  capacity    %s (%s, %s) · nodes %d/%d · vms %d/%d · mem %d/%d MiB (huge %d/%d) · vcpu %.3g/%d×%.3g · scratch %d/%d MiB · %d containers\n",
			h.HostID, orDash(h.Arch), offers, u.Nodes, c.Nodes, u.VMs, c.VMs, u.VMMemMiB, c.VMMemMiB, u.HugeMemMiB, c.HugeMemMiB,
			u.VCPUs, c.VCPUs, c.VCPUOvercommit, u.ScratchMiB, c.ScratchMiB, u.Containers)
	}
	for _, r := range res {
		fmt.Printf("  reserved    %s/%s/%s/%s on %s · %.4g vCPU, %d MiB, %d MiB disk (%s)\n", r.TenantID, r.Workload, r.Class, r.Object, r.HostID,
			r.Resources.VCPU, r.Resources.MemMiB, r.Resources.DiskMiB, r.ID)
	}
	for _, c := range local {
		d := c.Digest
		if len(d) > 19 {
			d = d[:19] + "…"
		}
		extra := ""
		if c.State == "running" {
			extra = fmt.Sprintf(" · ready %d ms · rss %d MB", c.ReadyMS, c.RSSKB>>10)
		} else if c.ExitReason != "" {
			extra = fmt.Sprintf(" · exit %d (%s)", c.ExitCode, firstLine(c.ExitReason))
		}
		fmt.Printf("  container   %s/%s/%s %s · %s (%s)%s\n", c.Workload, c.Class, c.Object, c.State, c.Image, d, extra)
	}
	if cacheBytes > 0 {
		fmt.Printf("  images      %d MB converted on this host\n", cacheBytes>>20)
	}
}
