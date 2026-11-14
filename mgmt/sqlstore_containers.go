package mgmt

import (
	"context"
	"database/sql"
	"errors"
)

// ---- container reservations (ADR-032 D9) ----

const containerCols = "id, tenant_id, workload, class, object, host_id, uvcpu, mem_mib, disk_mib, created_at, renewed_at"

func scanContainer(sc interface{ Scan(...any) error }) (ContainerReservation, error) {
	var r ContainerReservation
	var mv, mem, disk int64
	err := sc.Scan(&r.ID, &r.TenantID, &r.Workload, &r.Class, &r.Object, &r.HostID, &mv, &mem, &disk, &r.CreatedAt, &r.RenewedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	r.Resources.VCPU, r.Resources.MemMiB, r.Resources.DiskMiB = float64(mv)/1e6, int(mem), int(disk)
	return r, err
}

func (s *SQLStore) insertContainer(ctx context.Context, tx *sql.Tx, r ContainerReservation) error {
	_, err := tx.ExecContext(ctx, s.q("INSERT INTO container_reservations ("+containerCols+") VALUES (?,?,?,?,?,?,?,?,?,?,?)"),
		r.ID, r.TenantID, r.Workload, r.Class, r.Object, r.HostID, r.Resources.MicroVCPU(), r.Resources.MemMiB, r.Resources.DiskMiB, r.CreatedAt, r.RenewedAt)
	return err
}

func (s *SQLStore) ReserveContainer(ctx context.Context, r ContainerReservation, q ContainerQuota, fence *Fence) (ContainerReservation, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ContainerReservation{}, false, err
	}
	defer tx.Rollback()
	if fence != nil {
		if err := s.checkFence(ctx, tx, fence); err != nil {
			return ContainerReservation{}, false, err
		}
	}
	// The tenant row serialises every reservation of the tenant (Postgres:
	// FOR UPDATE; SQLite: the immediate transaction is the write lock).
	var x string
	if err := tx.QueryRowContext(ctx, s.q("SELECT id FROM tenants WHERE id=?"+s.forUpdate()), r.TenantID).Scan(&x); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ContainerReservation{}, false, ErrNotFound
		}
		return ContainerReservation{}, false, err
	}
	old, err := scanContainer(tx.QueryRowContext(ctx, s.q("SELECT "+containerCols+" FROM container_reservations WHERE tenant_id=? AND workload=? AND class=? AND object=?"+s.forUpdate()),
		r.TenantID, r.Workload, r.Class, r.Object))
	exists := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ContainerReservation{}, false, err
	}
	// A renewal that fits what the key already holds needs no headroom (a
	// restart during a quota squeeze still works).
	if !exists || !r.Resources.Fits(old.Resources) {
		var n, mem, mv, classN int64
		if err := tx.QueryRowContext(ctx, s.q(`SELECT COUNT(*), COALESCE(SUM(mem_mib),0), COALESCE(SUM(uvcpu),0) FROM container_reservations
			WHERE tenant_id=? AND renewed_at>=? AND id<>?`), r.TenantID, q.StaleBefore, old.ID).Scan(&n, &mem, &mv); err != nil {
			return ContainerReservation{}, false, err
		}
		if err := tx.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM container_reservations
			WHERE tenant_id=? AND workload=? AND class=? AND renewed_at>=? AND id<>?`), r.TenantID, r.Workload, r.Class, q.StaleBefore, old.ID).Scan(&classN); err != nil {
			return ContainerReservation{}, false, err
		}
		switch {
		case q.MaxContainers > 0 && n+1 > int64(q.MaxContainers):
			return ContainerReservation{}, false, &ContainerQuotaError{Quota: "maxContainers", Limit: float64(q.MaxContainers), Used: float64(n), Requested: 1}
		case q.ClassMax > 0 && classN+1 > int64(q.ClassMax):
			return ContainerReservation{}, false, &ContainerQuotaError{Quota: "max_instances", Limit: float64(q.ClassMax), Used: float64(classN), Requested: 1}
		case q.MaxMemMiB > 0 && mem+int64(r.Resources.MemMiB) > int64(q.MaxMemMiB):
			return ContainerReservation{}, false, &ContainerQuotaError{Quota: "maxContainerMemMiB", Limit: float64(q.MaxMemMiB), Used: float64(mem), Requested: float64(r.Resources.MemMiB)}
		case q.MaxMicroVCPU > 0 && mv+r.Resources.MicroVCPU() > q.MaxMicroVCPU:
			return ContainerReservation{}, false, &ContainerQuotaError{Quota: "maxContainerVCPU", Limit: float64(q.MaxMicroVCPU) / 1e6, Used: float64(mv) / 1e6, Requested: r.Resources.VCPU}
		}
	}
	if exists {
		res := old.Resources
		if !r.Resources.Fits(old.Resources) {
			res = r.Resources
		}
		if _, err := tx.ExecContext(ctx, s.q("UPDATE container_reservations SET host_id=?, uvcpu=?, mem_mib=?, disk_mib=?, renewed_at=? WHERE id=?"),
			r.HostID, res.MicroVCPU(), res.MemMiB, res.DiskMiB, r.RenewedAt, old.ID); err != nil {
			return ContainerReservation{}, false, err
		}
		old.HostID, old.Resources, old.RenewedAt = r.HostID, res, r.RenewedAt
		r = old
	} else if err := s.insertContainer(ctx, tx, r); err != nil {
		if isUnique(err) {
			return ContainerReservation{}, false, ErrConflict
		}
		return ContainerReservation{}, false, err
	}
	if err := s.commitWithIntents(ctx, tx); err != nil {
		return ContainerReservation{}, false, err
	}
	return r, !exists, nil
}

func (s *SQLStore) GetContainerReservation(ctx context.Context, id string) (ContainerReservation, error) {
	return scanContainer(s.db.QueryRowContext(ctx, s.q("SELECT "+containerCols+" FROM container_reservations WHERE id=?"), id))
}

func (s *SQLStore) ReleaseContainer(ctx context.Context, id, hostID string) error {
	return s.tx(ctx, func(tx *sql.Tx) error { // the release's audit intent commits with it (H22)
		res, err := tx.ExecContext(ctx, s.q("DELETE FROM container_reservations WHERE id=? AND host_id=?"), id, hostID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func (s *SQLStore) ListContainerReservations(ctx context.Context, f ContainerFilter, pg Page) ([]ContainerReservation, error) {
	var conds []string
	var args []any
	if f.TenantID != "" {
		conds, args = append(conds, "tenant_id=?"), append(args, f.TenantID)
	}
	if f.HostID != "" {
		conds, args = append(conds, "host_id=?"), append(args, f.HostID)
	}
	if cond, a := afterKey(pg, "created_at", "id"); cond != "" {
		conds, args = append(conds, cond), append(args, a...)
	}
	q := "SELECT " + containerCols + " FROM container_reservations"
	for i, c := range conds {
		if i == 0 {
			q += " WHERE " + c
		} else {
			q += " AND " + c
		}
	}
	rows, err := s.db.QueryContext(ctx, s.q(q+" ORDER BY created_at, id"+limitClause(pg)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ContainerReservation
	for rows.Next() {
		r, err := scanContainer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *SQLStore) ReconcileContainers(ctx context.Context, hostID string, running []ContainerReservation, now, releaseBefore int64,
	adopt func(ContainerReservation) bool) (adopted, released []ContainerReservation, err error) {
	err = s.tx(ctx, func(tx *sql.Tx) error {
		adopted, released = nil, nil
		for _, r := range running {
			res, err := tx.ExecContext(ctx, s.q("UPDATE container_reservations SET renewed_at=? WHERE tenant_id=? AND workload=? AND class=? AND object=? AND host_id=?"),
				now, r.TenantID, r.Workload, r.Class, r.Object, hostID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 || adopt == nil || !adopt(r) {
				continue
			}
			var x string
			err = tx.QueryRowContext(ctx, s.q("SELECT id FROM container_reservations WHERE tenant_id=? AND workload=? AND class=? AND object=?"),
				r.TenantID, r.Workload, r.Class, r.Object).Scan(&x)
			if err == nil {
				continue // another host holds the key
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			r.HostID, r.CreatedAt, r.RenewedAt = hostID, now, now
			if err := s.insertContainer(ctx, tx, r); err != nil {
				return err
			}
			adopted = append(adopted, r)
		}
		rows, err := tx.QueryContext(ctx, s.q("SELECT "+containerCols+" FROM container_reservations WHERE host_id=? AND renewed_at<?"+s.forUpdate()), hostID, releaseBefore)
		if err != nil {
			return err
		}
		for rows.Next() {
			r, err := scanContainer(rows)
			if err != nil {
				rows.Close()
				return err
			}
			released = append(released, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range released {
			if _, err := tx.ExecContext(ctx, s.q("DELETE FROM container_reservations WHERE id=?"), r.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return adopted, released, err
}
