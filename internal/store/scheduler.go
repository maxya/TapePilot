package store

import (
	"context"
	"database/sql"
	"errors"
	"github.com/maxya/TapePilot/internal/domain"
)

var ErrWaitingMedia = errors.New("WAITING_MEDIA")

// RequestCancel records cancellation before the scheduler signals execution.
// Queued and media-wait jobs have no live resources and can finish immediately.
func (s *Store) RequestCancel(ctx context.Context, id string) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		j, err := scanJob(tx.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id=?", id))
		if err != nil {
			return err
		}
		if terminal(j.State) {
			return nil
		}
		to := j.State
		if j.State == domain.Queued || j.State == domain.WaitingMedia {
			to = domain.Cancelled
		}
		if j.State == domain.Running || j.State == domain.Verifying {
			to = domain.CancelRequested
		}
		completed := any(nil)
		active := any(1)
		if to == domain.Cancelled {
			completed = stamp()
			active = nil
		}
		if _, err = tx.ExecContext(ctx, "UPDATE jobs SET cancel_requested=1,state=?,state_version=state_version+1,active_slot=?,completed_at=? WHERE id=?", to, active, completed, id); err != nil {
			return err
		}
		return event(ctx, tx, id, "CANCEL_REQUESTED", "Cancellation requested")
	})
}
func (s *Store) CancellationRequested(ctx context.Context, id string) (bool, error) {
	var cancelled bool
	err := s.db.QueryRowContext(ctx, "SELECT cancel_requested FROM jobs WHERE id=?", id).Scan(&cancelled)
	return cancelled, err
}

// RecheckMedia requeues only the named waiting job. It does not observe devices.
func (s *Store) RecheckMedia(ctx context.Context, id string) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "UPDATE jobs SET media_recheck=1,error_code='' WHERE id=? AND state='WAITING_MEDIA' AND cancel_requested=0", id)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrConflict
		}
		return event(ctx, tx, id, "MEDIA_RECHECK", "Operator requested fresh media preflight")
	})
}

// FinishExecution runs only after executor resources and its tape lease close.
// It never publishes success for the executor.
func (s *Store) FinishExecution(ctx context.Context, id string, runErr error) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		j, err := scanJob(tx.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id=?", id))
		if err != nil {
			return err
		}
		if terminal(j.State) {
			return nil
		}
		var uncertain, cancelled int
		if err = tx.QueryRowContext(ctx, "SELECT cancel_requested FROM jobs WHERE id=?", id).Scan(&cancelled); err != nil {
			return err
		}
		if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(write_may_have_started),0) FROM write_intents WHERE job_id=?", id).Scan(&uncertain); err != nil {
			return err
		}
		to := domain.Failed
		code := "EXECUTION_FAILED"
		if cancelled != 0 || errors.Is(runErr, context.Canceled) {
			to = domain.Cancelled
			code = "CANCELLED"
		}
		if errors.Is(runErr, ErrWaitingMedia) && j.State == domain.Preflight && uncertain == 0 && cancelled == 0 {
			to = domain.WaitingMedia
			code = "WAITING_MEDIA"
		}
		if (j.State == domain.Verifying && runErr != nil && cancelled == 0 && !errors.Is(runErr, context.Canceled)) || uncertain != 0 || (j.Kind == domain.Restore && (j.Phase == "publishing" || j.Phase == "published")) {
			to = domain.RecoveryRequired
			code = "RECOVERY_REQUIRED"
		}
		if runErr == nil {
			code = "MISSING_COMPLETION_EVIDENCE"
		}
		completed := any(stamp())
		if to == domain.WaitingMedia {
			completed = nil
		}
		if _, err = tx.ExecContext(ctx, "UPDATE jobs SET state=?,error_code=?,state_version=state_version+1,active_slot=NULL,completed_at=? WHERE id=?", to, code, completed, id); err != nil {
			return err
		}
		if to == domain.RecoveryRequired {
			if _, err = tx.ExecContext(ctx, "UPDATE volumes SET state='QUARANTINED' WHERE id IN(SELECT volume_id FROM jobs WHERE id=?)", id); err != nil {
				return err
			}
		}
		return event(ctx, tx, id, string(to), "Executor resources closed and result recorded")
	})
}
