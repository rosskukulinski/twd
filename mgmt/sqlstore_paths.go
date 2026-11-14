package mgmt

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
)

// ---- smart placement: measured paths (ADR-029) ----

// PutHostPaths replaces what host reported for tenant (one transaction:
// the (host, tenant) rows are deleted and the new ones inserted, so a
// target the host no longer measures disappears). Rows of this host older
// than pathRetention are dropped at the same time. The caller bounds ms
// (MaxPathTargets).
func (s *SQLStore) PutHostPaths(ctx context.Context, hostID, tenantID string, ms []PathMeasurement, now time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, s.q("DELETE FROM host_paths WHERE host_id=? AND (tenant_id=? OR measured_at<?)"),
			hostID, tenantID, now.Add(-pathRetention).UnixMilli()); err != nil {
			return err
		}
		for _, m := range ms {
			if _, err := tx.ExecContext(ctx, s.q(`INSERT INTO host_paths (host_id, tenant_id, target, peer, path, rtt_us, derp_region, measured_at)
				VALUES (?,?,?,?,?,?,?,?) ON CONFLICT (tenant_id, target, host_id) DO UPDATE SET peer=excluded.peer, path=excluded.path,
				rtt_us=excluded.rtt_us, derp_region=excluded.derp_region, measured_at=excluded.measured_at`),
				hostID, tenantID, m.Target, m.Peer, m.Path, m.RTTMicros, m.DERPRegion, m.MeasuredAt); err != nil {
				return err
			}
		}
		return nil
	})
}

// HostPaths returns every host's measurements of the tenant's targets,
// ordered by (target, host).
func (s *SQLStore) HostPaths(ctx context.Context, tenantID string, targets []string) ([]PathMeasurement, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	args := []any{tenantID}
	for _, t := range targets {
		args = append(args, t)
	}
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT host_id, tenant_id, target, peer, path, rtt_us, derp_region, measured_at FROM host_paths
		WHERE tenant_id=? AND target IN (?`+strings.Repeat(",?", len(targets)-1)+`) ORDER BY target, host_id`), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PathMeasurement
	for rows.Next() {
		var m PathMeasurement
		if err := rows.Scan(&m.HostID, &m.TenantID, &m.Target, &m.Peer, &m.Path, &m.RTTMicros, &m.DERPRegion, &m.MeasuredAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func placementStr(p *PlacementDecision) string {
	if p == nil {
		return ""
	}
	b, _ := json.Marshal(p)
	return string(b)
}

func parsePlacement(s string) *PlacementDecision {
	if s == "" {
		return nil
	}
	var p PlacementDecision
	if json.Unmarshal([]byte(s), &p) != nil {
		return nil
	}
	return &p
}
