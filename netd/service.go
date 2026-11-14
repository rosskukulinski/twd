package netd

import (
	"context"
	"fmt"
	"slices"

	"github.com/rosskukulinski/twd/internal/tssvc"
)

// AdvertiseService makes object id's node a host for the Tailscale Service
// name on the ingress port. Service flows reach the same attributed ingress
// as the node's own address: WhoIs runs on the caller's real tailnet
// address. Control routes the Service here only once it is defined and this
// node is approved (policy autoApprovers.services for the node's tags).
func (r *Registry) AdvertiseService(ctx context.Context, id, name string) error {
	if err := tssvc.Validate(name); err != nil {
		return err
	}
	o := r.Get(id)
	if o == nil {
		return ErrNotFound
	}
	o.mu.Lock()
	svc, state := o.svc, o.state
	o.mu.Unlock()
	if state != StateReady || svc == nil {
		return fmt.Errorf("netd: object %s is %s; a Service needs a ready node", id, state)
	}
	if err := svc.Advertise(ctx, name, uint16(r.cfg.IngressPort)); err != nil {
		return err
	}
	o.mu.Lock()
	if st := o.state; st != StateReady || o.svc != svc { // killed or rejoined meanwhile
		o.mu.Unlock()
		svc.Withdraw(ctx, name)
		return fmt.Errorf("netd: object %s became %s while advertising", id, st)
	}
	if !slices.Contains(o.services, name) {
		o.services = append(o.services, name)
	}
	shs := o.svcHTTP
	o.mu.Unlock()
	if shs != nil {
		shs.SetKeepAlivesEnabled(true)
	}
	return nil
}

// WithdrawService stops object id's node advertising name. Requests in
// flight finish, but once no Service is left, Service connections stop
// being kept alive (idle ones close, active ones answer Connection: close),
// so clients re-resolve and control routes them to a host that still
// serves the Service. It is idempotent and a no-op for unknown or closed
// objects (a closed node advertises nothing).
func (r *Registry) WithdrawService(ctx context.Context, id, name string) error {
	o := r.Get(id)
	if o == nil {
		return nil
	}
	o.mu.Lock()
	svc, state, shs := o.svc, o.state, o.svcHTTP
	o.services = slices.DeleteFunc(o.services, func(s string) bool { return s == name })
	none := len(o.services) == 0
	o.mu.Unlock()
	if svc == nil || state != StateReady {
		return nil
	}
	err := svc.Withdraw(ctx, name)
	if none && shs != nil {
		shs.SetKeepAlivesEnabled(false)
	}
	return err
}
