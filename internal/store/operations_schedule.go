package store

import (
	"context"
	"database/sql"
	"errors"
	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/domain"
	"strings"
	"time"
	_ "time/tzdata"
)

type Schedule struct {
	Operation domain.Kind `json:"operation"`
	ID        string      `json:"id"`
	ProfileID string      `json:"profile_id"`
	Cadence   string      `json:"cadence"`
	Timezone  string      `json:"timezone"`
	LocalTime string      `json:"local_time"`
	Weekday   int         `json:"weekday"`
	Enabled   bool        `json:"enabled"`
	Revision  int64       `json:"revision"`
	CreatedAt string      `json:"created_at"`
	UpdatedAt string      `json:"updated_at"`
	LastSlot  string      `json:"-"`
}

func ValidSchedule(s Schedule) error {
	if s.Operation != "" && s.Operation != domain.Backup && s.Operation != domain.Verify {
		return ErrOperations
	}
	if _, e := uuid.Parse(s.ProfileID); e != nil {
		return ErrOperations
	}
	if s.Cadence != "daily" && s.Cadence != "weekly" || s.Weekday < 0 || s.Weekday > 6 || s.Timezone == "" || s.Timezone == "Local" || len(s.Timezone) > 128 || strings.ContainsRune(s.Timezone, 0) {
		return ErrOperations
	}
	if _, e := time.LoadLocation(s.Timezone); e != nil {
		return ErrOperations
	}
	if len(s.LocalTime) != 5 || s.LocalTime[2] != ':' {
		return ErrOperations
	}
	parsed, e := time.Parse("15:04", s.LocalTime)
	if e != nil || parsed.Format("15:04") != s.LocalTime {
		return ErrOperations
	}
	return nil
}

const scheduleColumns = "id,profile_id,cadence,timezone,local_time,weekday,enabled,revision,created_at,updated_at,last_slot,operation"

func scanSchedule(row scanner) (s Schedule, e error) {
	e = row.Scan(&s.ID, &s.ProfileID, &s.Cadence, &s.Timezone, &s.LocalTime, &s.Weekday, &s.Enabled, &s.Revision, &s.CreatedAt, &s.UpdatedAt, &s.LastSlot, &s.Operation)
	return
}
func (s *Store) CreateSchedule(ctx context.Context, v Schedule, actor string) (Schedule, error) {
	if v.Operation == "" {
		v.Operation = domain.Backup
	}
	if e := ValidSchedule(v); e != nil {
		return v, e
	}
	if v.ID != "" || v.Revision != 0 || v.CreatedAt != "" || v.UpdatedAt != "" || v.LastSlot != "" {
		return v, ErrOperations
	}
	v.ID = uuid.NewString()
	v.Revision = 1
	v.CreatedAt = stamp()
	v.UpdatedAt = v.CreatedAt
	e := s.transaction(ctx, func(tx *sql.Tx) error {
		var count int
		if e := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM schedules").Scan(&count); e != nil {
			return e
		}
		if count >= 1000 {
			return ErrOperations
		}
		if _, e := tx.ExecContext(ctx, "INSERT INTO schedules VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", v.ID, v.ProfileID, v.Cadence, v.Timezone, v.LocalTime, v.Weekday, v.Enabled, v.Revision, v.CreatedAt, v.UpdatedAt, v.LastSlot, v.Operation); e != nil {
			return e
		}
		return insertAudit(ctx, tx, actor, "SCHEDULE_CREATED", v.ID)
	})
	return v, e
}
func (s *Store) Schedule(ctx context.Context, id string) (Schedule, error) {
	return scanSchedule(s.db.QueryRowContext(ctx, "SELECT "+scheduleColumns+" FROM schedules WHERE id=?", id))
}
func (s *Store) ListSchedules(ctx context.Context) ([]Schedule, error) {
	rows, e := s.db.QueryContext(ctx, "SELECT "+scheduleColumns+" FROM schedules ORDER BY id LIMIT 1000")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Schedule{}
	for rows.Next() {
		v, e := scanSchedule(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) UpdateSchedule(ctx context.Context, v Schedule, actor string) (Schedule, error) {
	if v.Operation == "" {
		v.Operation = domain.Backup
	}
	if e := ValidSchedule(v); e != nil {
		return v, e
	}
	if v.Revision < 1 || v.Revision == int64(^uint64(0)>>1) {
		return v, ErrOperations
	}
	v.UpdatedAt = stamp()
	e := s.transaction(ctx, func(tx *sql.Tx) error {
		result, e := tx.ExecContext(ctx, "UPDATE schedules SET operation=?,profile_id=?,cadence=?,timezone=?,local_time=?,weekday=?,enabled=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?", v.Operation, v.ProfileID, v.Cadence, v.Timezone, v.LocalTime, v.Weekday, v.Enabled, v.UpdatedAt, v.ID, v.Revision)
		if e != nil {
			return e
		}
		count, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if count != 1 {
			return ErrConflict
		}
		return insertAudit(ctx, tx, actor, "SCHEDULE_UPDATED", v.ID)
	})
	if e == nil {
		v.Revision++
	}
	return v, e
}

// FireSchedule atomically consumes a deterministic local slot and enqueues at
// most one outstanding profile job. A busy profile leaves its slot unconsumed
// so the next tick coalesces to the latest missed occurrence.
func (s *Store) FireSchedule(ctx context.Context, id string, revision int64, slot string, dueAt, now time.Time, sourceAllowed bool) (job Job, created bool, err error) {
	if len(slot) != 16 || slot[10] != 'T' || dueAt.IsZero() || dueAt.After(now) {
		return job, false, ErrOperations
	}
	if _, e := time.Parse("2006-01-02T15:04", slot); e != nil {
		return job, false, ErrOperations
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		v, e := scanSchedule(tx.QueryRowContext(ctx, "SELECT "+scheduleColumns+" FROM schedules WHERE id=?", id))
		if e != nil {
			return e
		}
		if v.Revision != revision {
			return ErrConflict
		}
		if !v.Enabled || slot <= v.LastSlot {
			return nil
		}
		location, e := time.LoadLocation(v.Timezone)
		if e != nil {
			return e
		}
		if slot[11:] != v.LocalTime || dueAt.In(location).Format("2006-01-02") != slot[:10] || dueAt.Second() != 0 || dueAt.Nanosecond() != 0 {
			return ErrOperations
		}
		if v.Cadence == "weekly" && int(dueAt.In(location).Weekday()) != v.Weekday {
			return ErrOperations
		}
		start, e := time.Parse(time.RFC3339Nano, v.CreatedAt)
		if e != nil {
			return e
		}
		if dueAt.Before(start) {
			return nil
		}
		p, e := scanProfile(tx.QueryRowContext(ctx, "SELECT "+profileColumns+" FROM backup_profiles WHERE id=?", v.ProfileID))
		if e != nil {
			return e
		}
		var eligible int
		if v.Operation == domain.Verify {
			e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM volumes WHERE id=? AND kind='MANAGED' AND header_sha256 IS NOT NULL", p.TargetVolumeID).Scan(&eligible)
			sourceAllowed = true
		} else {
			e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM volumes v JOIN volume_enrollments e ON e.volume_id=v.id WHERE v.id=? AND v.kind='MANAGED' AND v.state='MANAGED' AND e.enabled=1 AND e.revoked_at IS NULL", p.TargetVolumeID).Scan(&eligible)
		}
		if e != nil {
			return e
		}

		if !sourceAllowed || !p.Enabled || eligible != 1 {
			message := "Scheduled backup blocked. Check source configuration, profile, target media and local enrollment."
			if v.Operation == domain.Verify {
				message = "Scheduled verification blocked. Check the enabled profile and its registered managed target tape."
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO notifications(code,message,created_at,dedupe_key) VALUES('ACTION_REQUIRED',?,?,?) ON CONFLICT(dedupe_key) DO NOTHING", message, stamp(), "schedule:"+id+":"+slot); e != nil {
				return e
			}
			if e = insertAudit(ctx, tx, "scheduler", "SCHEDULE_BLOCKED", id); e != nil {
				return e
			}
		} else {
			var queued bool
			job, queued, e = enqueueProfileOperationTx(ctx, tx, p, v.Operation, "schedule:"+id+":"+slot, "", "scheduler")
			if errors.Is(e, ErrOutstanding) {
				return nil
			}
			if e != nil {
				return e
			}
			created = queued
		}
		_, e = tx.ExecContext(ctx, "UPDATE schedules SET last_slot=? WHERE id=? AND revision=?", slot, id, revision)
		return e
	})
	return
}
