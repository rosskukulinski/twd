package hostd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/rosskukulinski/twd/hostd/image"
	"github.com/rosskukulinski/twd/hostd/mgmtapi"
	"github.com/rosskukulinski/twd/hostd/sandbox/sandboxtest"
	"github.com/rosskukulinski/twd/internal/container"
)

// fakeQuota is mgmt's reservation API: reachable or down, granting up to
// max reservations (a 429 past that), idempotent per key.
type fakeQuota struct {
	mu       sync.Mutex
	down     bool
	max      int
	byKey    map[string]*mgmtapi.ContainerReservation
	reserves []mgmtapi.ContainerReservationRequest
	released []string
}

func newFakeQuota() *fakeQuota {
	return &fakeQuota{max: 100, byKey: map[string]*mgmtapi.ContainerReservation{}}
}

var errUnreachable = errors.New("dial unix mgmt.sock: connection refused")

func (q *fakeQuota) Reserve(_ context.Context, r mgmtapi.ContainerReservationRequest) (*mgmtapi.ContainerReservation, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.reserves = append(q.reserves, r)
	if q.down {
		return nil, errUnreachable
	}
	k := r.TenantID + "|" + r.Workload + "|" + r.Class + "|" + r.Object
	if x := q.byKey[k]; x != nil {
		return x, nil
	}
	if len(q.byKey) >= q.max {
		return nil, &mgmtapi.Error{Status: 429, Code: "container_quota_exceeded", Message: "maxContainers reached",
			Missing: []byte(`{"quota":"maxContainers","limit":1,"used":1,"requested":1}`)}
	}
	x := &mgmtapi.ContainerReservation{ReservationID: fmt.Sprintf("cr_%d", len(q.reserves)), TenantID: r.TenantID, Workload: r.Workload,
		Class: r.Class, Object: r.Object, Resources: r.Resources}
	q.byKey[k] = x
	return x, nil
}

func (q *fakeQuota) Release(_ context.Context, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.down {
		return errUnreachable
	}
	q.released = append(q.released, id)
	for k, x := range q.byKey {
		if x.ReservationID == id {
			delete(q.byKey, k)
		}
	}
	return nil
}

func (q *fakeQuota) setDown(d bool) { q.mu.Lock(); q.down = d; q.mu.Unlock() }
func (q *fakeQuota) calls() int     { q.mu.Lock(); defer q.mu.Unlock(); return len(q.reserves) }

// admitHost is a Host with a (fake) microVM and container backend (vm), the
// given capacity, the fake quota and one workload "tn/w" at epoch 7.
func admitHost(t *testing.T, vm bool, capVec mgmtapi.HostCapacity) (*Host, *fakeQuota) {
	t.Helper()
	q := newFakeQuota()
	mc, _ := mgmtapi.NewClient("unix:///nonexistent")
	cfg := Config{HostID: "h1", Mgmt: mc, DataDir: t.TempDir(), RunDir: t.TempDir(), WorkerdBin: "/bin/true", ContainerQuota: q, Capacity: &capVec}
	if vm {
		cfg.VM, cfg.Containers, cfg.Images = fakeBackend{}, &sandboxtest.Procs{}, &image.Store{Dir: t.TempDir()}
	}
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.workloads["tn/w"] = &workload{h: h, tenantID: "tn", name: "w", epoch: 7}
	return h, q
}

var roomy = mgmtapi.HostCapacity{VMs: 10, VMMemMiB: 1 << 20, VCPUs: 64, VCPUOvercommit: 1, ScratchMiB: 1 << 20}

func key(obj string) ContainerKey {
	return ContainerKey{TenantID: "tn", Workload: "w", Class: "Sandbox", Object: obj}
}

var lite, basic = container.InstanceTypes["lite"], container.InstanceTypes["basic"]

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != code {
		t.Fatalf("err = %v, want %s", err, code)
	}
	if st := errStatus[code]; st == 0 {
		t.Fatalf("%s has no HTTP status", code)
	}
}

// A host without a container backend refuses (never runs a container
// another way), before it asks mgmt for anything.
func TestContainerNeedsBackend(t *testing.T) {
	h, q := admitHost(t, false, roomy)
	_, err := h.AcquireContainer(context.Background(), key("a"), lite)
	wantCode(t, err, "tier_unavailable")
	if q.calls() != 0 {
		t.Fatal("reserved quota for a container this host can't run")
	}
	// A microVM backend alone (no usable container backend: an old helper,
	// a kernel without erofs, no mkfs.erofs) offers the microvm tier but
	// not containers, and refuses them the same way.
	h, q = admitHost(t, false, roomy)
	h.cfg.VM = fakeBackend{}
	if got := h.tiers(); !slices.Equal(got, []string{"process", "microvm"}) {
		t.Fatalf("tiers %v", got)
	}
	_, err = h.AcquireContainer(context.Background(), key("a"), lite)
	wantCode(t, err, "tier_unavailable")
	// twd dev (ForceProcessTier) offers neither, even with both backends.
	h, q2 := admitHost(t, true, roomy)
	h.cfg.ForceProcessTier = true
	if got := h.tiers(); !slices.Equal(got, []string{"process"}) {
		t.Fatalf("forced tiers %v", got)
	}
	_, err = h.AcquireContainer(context.Background(), key("a"), lite)
	wantCode(t, err, "tier_unavailable")
	if q.calls()+q2.calls() != 0 {
		t.Fatal("reserved quota for a container this host can't run")
	}
}

// Local capacity is checked before mgmt is asked: over any dimension is 503
// capacity_exhausted (no spill), and a released slot frees its room.
func TestContainerCapacity(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		cap   mgmtapi.HostCapacity
		fits  int
		res   container.Resources
		which string
	}{
		{"vms", mgmtapi.HostCapacity{VMs: 2, VMMemMiB: 1 << 20, VCPUs: 64, VCPUOvercommit: 1, ScratchMiB: 1 << 20}, 2, lite, "vms"},
		{"memory", mgmtapi.HostCapacity{VMs: 10, VMMemMiB: 2048, VCPUs: 64, VCPUOvercommit: 1, ScratchMiB: 1 << 20}, 2, basic, "vmMemMiB"},
		{"vcpus with overcommit", mgmtapi.HostCapacity{VMs: 10, VMMemMiB: 1 << 20, VCPUs: 1, VCPUOvercommit: 0.5, ScratchMiB: 1 << 20}, 2, basic, "vcpus"},
		{"scratch", mgmtapi.HostCapacity{VMs: 10, VMMemMiB: 1 << 20, VCPUs: 64, VCPUOvercommit: 1, ScratchMiB: 9000}, 2, basic, "scratchMiB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, q := admitHost(t, true, tc.cap)
			var slots []*ContainerSlot
			for i := range tc.fits {
				s, err := h.AcquireContainer(ctx, key(fmt.Sprint("o", i)), tc.res)
				if err != nil {
					t.Fatalf("container %d: %v", i, err)
				}
				slots = append(slots, s)
			}
			n := q.calls()
			_, err := h.AcquireContainer(ctx, key("over"), tc.res)
			wantCode(t, err, "capacity_exhausted")
			if ae := asAPIError(err); fmt.Sprint(ae.Extra["missing"].(map[string]any)["capacity"]) != tc.which {
				t.Fatalf("refused on %v, want %s", ae.Extra, tc.which)
			}
			if q.calls() != n {
				t.Fatal("asked mgmt for quota for a container the host has no room for")
			}
			slots[0].Release(ctx)
			if _, err := h.AcquireContainer(ctx, key("over"), tc.res); err != nil {
				t.Fatalf("after a release: %v", err)
			}
		})
	}
}

// Concurrent starts never overrun local capacity.
func TestContainerCapacityConcurrent(t *testing.T) {
	h, _ := admitHost(t, true, mgmtapi.HostCapacity{VMs: 3, VMMemMiB: 1 << 20, VCPUs: 64, VCPUOvercommit: 1, ScratchMiB: 1 << 20})
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.AcquireContainer(context.Background(), key(fmt.Sprint("o", i)), lite); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 3 || h.usage().Containers != 3 {
		t.Fatalf("%d started (usage %d), capacity 3", ok, h.usage().Containers)
	}
}

// mgmt's quota decides: a 429 is passed on typed, and the local capacity it
// held is given back. The reservation is for the workload's lease epoch.
func TestContainerQuotaRefused(t *testing.T) {
	ctx := context.Background()
	h, q := admitHost(t, true, roomy)
	q.max = 1
	if _, err := h.AcquireContainer(ctx, key("a"), lite); err != nil {
		t.Fatal(err)
	}
	if q.reserves[0].Epoch != 7 {
		t.Fatalf("reserved at epoch %d, want the workload's 7", q.reserves[0].Epoch)
	}
	_, err := h.AcquireContainer(ctx, key("b"), lite)
	wantCode(t, err, "container_quota_exceeded")
	if asAPIError(err).Extra["missing"] == nil {
		t.Fatal("the quota refusal lost its missing{}")
	}
	if u := h.usage(); u.Containers != 1 {
		t.Fatalf("a refused container holds capacity: %+v", u)
	}
	_, err = h.AcquireContainer(ctx, key("a"), lite)
	wantCode(t, err, "container_running")
	if _, err := h.AcquireContainer(ctx, ContainerKey{TenantID: "tn", Workload: "other", Class: "S", Object: "x"}, lite); err == nil {
		t.Fatal("a container of a workload this host doesn't run was admitted")
	}
}

// Fail closed: with mgmt unreachable no container starts, except a restart
// of one whose reservation this host still holds (the same or a smaller
// container). Once mgmt acknowledges the release, the exception is gone.
func TestContainerFailClosedAndRestart(t *testing.T) {
	ctx := context.Background()
	h, q := admitHost(t, true, roomy)
	q.setDown(true)
	_, err := h.AcquireContainer(ctx, key("new"), lite)
	wantCode(t, err, "quota_unavailable")
	if u := h.usage(); u.Containers != 0 || len(h.containers.running()) != 0 {
		t.Fatalf("a refused start holds capacity or is reported: %+v", u)
	}

	q.setDown(false)
	s, err := h.AcquireContainer(ctx, key("a"), basic)
	if err != nil || s.ReservationID == "" || s.Restarted {
		t.Fatalf("start a: %+v %v", s, err)
	}
	if rs := h.containers.running(); len(rs) != 1 || rs[0].ReservationID != s.ReservationID || rs[0].Object != "a" {
		t.Fatalf("heartbeat report: %+v", rs)
	}
	q.setDown(true)
	s.Release(ctx) // mgmt doesn't hear it: the reservation stays held
	if rs := h.containers.running(); len(rs) != 0 {
		t.Fatalf("a stopped container is reported running: %+v", rs)
	}
	_, err = h.AcquireContainer(ctx, key("a"), container.InstanceTypes["standard-1"])
	wantCode(t, err, "quota_unavailable") // bigger than what it holds
	_, err = h.AcquireContainer(ctx, key("b"), lite)
	wantCode(t, err, "quota_unavailable") // another key holds nothing
	r, err := h.AcquireContainer(ctx, key("a"), lite)
	if err != nil || !r.Restarted || r.ReservationID != s.ReservationID {
		t.Fatalf("restart within the held reservation: %+v %v", r, err)
	}
	r.Release(ctx)

	// mgmt is back: the release goes through after a good heartbeat, and the
	// exception with it.
	q.setDown(false)
	h.retryReleases(ctx)
	if len(q.released) != 1 || q.released[0] != s.ReservationID {
		t.Fatalf("released %v", q.released)
	}
	q.setDown(true)
	_, err = h.AcquireContainer(ctx, key("a"), lite)
	wantCode(t, err, "quota_unavailable")
}

// The capacity vector: VM dimensions only with a microVM backend; nodes
// are capacityNodes; usage counts DO microVMs and containers.
func TestCapacityVector(t *testing.T) {
	h, _ := admitHost(t, false, roomy)
	if c := h.capacity(); c.VMs != 0 || c.VMMemMiB != 0 || c.Nodes != 1000 {
		t.Fatalf("no backend: %+v", c)
	}
	h, _ = admitHost(t, true, roomy)
	if c := h.capacity(); c.VMs != 10 || c.Nodes != 1000 {
		t.Fatalf("with a backend: %+v", c)
	}
	h.workloads["tn/w"].active = &instance{tier: "microvm"}
	if _, err := h.AcquireContainer(context.Background(), key("a"), basic); err != nil {
		t.Fatal(err)
	}
	u := h.usage()
	if u.VMs != 2 || u.Containers != 1 || u.VMMemMiB != 512+1024 || u.VCPUs != 2.25 || u.ScratchMiB != basic.DiskMiB {
		t.Fatalf("usage: %+v", u)
	}
}
