package store

import (
	"context"
	"database/sql"
	"github.com/maxya/TapePilot/internal/progress"
	"time"
)

// SaveProgress stores one latest sample per job. Older samples cannot regress
// counters or overwrite newer observations. Reporter enforces the write rate.
func (s *Store) SaveProgress(ctx context.Context, p progress.Snapshot) error {
	if e := p.Validate(); e != nil {
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, e := s.db.ExecContext(ctx, `INSERT INTO job_progress VALUES(?,?,?,?,?,?,?,?,?,?,?)
 ON CONFLICT(job_id) DO UPDATE SET phase=excluded.phase,accepted_bytes=excluded.accepted_bytes,
 readback_bytes=excluded.readback_bytes,phase_total_bytes=excluded.phase_total_bytes,
 sampled_at=excluded.sampled_at,started_at=excluded.started_at,elapsed_seconds=excluded.elapsed_seconds,
 speed_mib_per_second=excluded.speed_mib_per_second,estimated_percent=excluded.estimated_percent,eta_seconds=excluded.eta_seconds
 WHERE excluded.sampled_at>=job_progress.sampled_at AND excluded.accepted_bytes>=job_progress.accepted_bytes
 AND excluded.readback_bytes>=job_progress.readback_bytes AND excluded.started_at=job_progress.started_at`, p.JobID, p.Phase, p.AcceptedBytes, p.ReadbackBytes, p.PhaseTotalBytes, p.SampledAt.Format("2006-01-02T15:04:05.000000000Z"), p.StartedAt.Format("2006-01-02T15:04:05.000000000Z"), p.ElapsedSeconds, p.SpeedMiBPerSecond, p.EstimatedPercent, p.ETASeconds)
	if e != nil {
		return e
	}
	count, e := result.RowsAffected()
	if e == nil && count == 0 {
		return ErrConflict
	}
	return e
}
func (s *Store) Progress(ctx context.Context, id string) (progress.Snapshot, error) {
	var p progress.Snapshot
	var sampled, started string
	var total sql.NullInt64
	var percent, eta sql.NullFloat64
	e := s.db.QueryRowContext(ctx, `SELECT job_id,phase,accepted_bytes,readback_bytes,phase_total_bytes,sampled_at,started_at,elapsed_seconds,speed_mib_per_second,estimated_percent,eta_seconds FROM job_progress WHERE job_id=?`, id).Scan(&p.JobID, &p.Phase, &p.AcceptedBytes, &p.ReadbackBytes, &total, &sampled, &started, &p.ElapsedSeconds, &p.SpeedMiBPerSecond, &percent, &eta)
	if e != nil {
		return p, e
	}
	p.SampledAt, e = time.Parse(time.RFC3339Nano, sampled)
	if e != nil {
		return p, e
	}
	p.StartedAt, e = time.Parse(time.RFC3339Nano, started)
	if e != nil {
		return p, e
	}
	if total.Valid {
		p.PhaseTotalBytes = &total.Int64
	}
	if percent.Valid {
		p.EstimatedPercent = &percent.Float64
	}
	if eta.Valid {
		p.ETASeconds = &eta.Float64
	}
	return p, p.Validate()
}
