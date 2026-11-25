package store

import (
	"context"
	"database/sql"
	"strconv"
	"time"
)

type OutboxMessage struct {
	ID          int64
	JobID, Code string
	Attempts    int
}

// ConfigureSMTP synchronizes immutable host configuration before job execution.
// Disabled installations never accumulate a queue. Turning SMTP off suppresses
// pending deliveries so later opt-in cannot replay an old backlog.
func (s *Store) ConfigureSMTP(ctx context.Context, enabled bool) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var previous bool
		if e := tx.QueryRowContext(ctx, "SELECT enabled FROM smtp_preferences WHERE id=1").Scan(&previous); e != nil {
			return e
		}
		if previous == enabled {
			return nil
		}
		if _, e := tx.ExecContext(ctx, "UPDATE smtp_preferences SET enabled=? WHERE id=1", enabled); e != nil {
			return e
		}
		if !enabled {
			if _, e := tx.ExecContext(ctx, "UPDATE notification_outbox SET permanent_failure=1,last_error_code='SMTP_DISABLED' WHERE delivered_at IS NULL AND permanent_failure=0"); e != nil {
				return e
			}
		}
		action := "SMTP_DISABLED"
		if enabled {
			action = "SMTP_ENABLED"
		}
		return insertAudit(ctx, tx, "system", action, "settings")
	})
}

// ClaimOutbox reserves one delivery attempt durably before any network I/O.
// A crash leaves a bounded retry after the reservation interval.
func (s *Store) ClaimOutbox(ctx context.Context, now time.Time) (message OutboxMessage, err error) {
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		e := tx.QueryRowContext(ctx, "SELECT id,job_id,code,attempts FROM notification_outbox WHERE delivered_at IS NULL AND permanent_failure=0 AND attempts<5 AND next_attempt_at<=? AND (SELECT enabled FROM smtp_preferences WHERE id=1)=1 ORDER BY id LIMIT 1", now.UTC().Format("2006-01-02T15:04:05.000000000Z")).Scan(&message.ID, &message.JobID, &message.Code, &message.Attempts)
		if e != nil {
			return e
		}
		message.Attempts++
		_, e = tx.ExecContext(ctx, "UPDATE notification_outbox SET attempts=attempts+1,next_attempt_at=? WHERE id=?", now.UTC().Add(5*time.Minute).Format("2006-01-02T15:04:05.000000000Z"), message.ID)
		return e
	})
	return
}

// FinishOutbox records a terse error classification, never server text or secrets.
func (s *Store) FinishOutbox(ctx context.Context, message OutboxMessage, accepted, permanent bool, code string, next time.Time) error {
	if message.ID < 1 || message.Attempts < 1 || message.Attempts > 5 || !auditAction.MatchString(code) {
		return ErrOperations
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		delivered := any(nil)
		if accepted {
			delivered = stamp()
		}
		result, e := tx.ExecContext(ctx, "UPDATE notification_outbox SET delivered_at=?,permanent_failure=?,last_error_code=?,next_attempt_at=? WHERE id=? AND attempts=? AND job_id=? AND code=? AND delivered_at IS NULL AND permanent_failure=0", delivered, permanent, code, next.UTC().Format("2006-01-02T15:04:05.000000000Z"), message.ID, message.Attempts, message.JobID, message.Code)
		if e != nil {
			return e
		}
		n, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrConflict
		}
		if !accepted && (permanent || message.Attempts == 5) {
			_, e = tx.ExecContext(ctx, "INSERT INTO notifications(job_id,code,message,created_at,dedupe_key) VALUES(?,'SMTP_DELIVERY_FAILED','Email delivery failed. Review the configured relay and credentials.',?,?) ON CONFLICT(dedupe_key) DO NOTHING", message.JobID, stamp(), "smtp-failure:"+strconv.FormatInt(message.ID, 10))
			if e != nil {
				return e
			}
		}
		return insertAudit(ctx, tx, "system", code, message.JobID)
	})
}
