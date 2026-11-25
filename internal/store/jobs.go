package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/domain"
	"strings"
)

type Job struct {
	ID          string          `json:"id"`
	Kind        domain.Kind     `json:"kind"`
	State       domain.State    `json:"state"`
	Phase       string          `json:"phase"`
	VolumeID    string          `json:"volume_id"`
	Options     json.RawMessage `json:"options"`
	DedupeKey   string          `json:"-"`
	Version     int64           `json:"state_version"`
	CreatedAt   string          `json:"created_at"`
	CompletedAt string          `json:"completed_at"`
	ErrorCode   string          `json:"error_code"`
}

const jobColumns = "id,kind,state,phase,COALESCE(volume_id,''),options_json,COALESCE(dedupe_key,''),state_version,created_at,COALESCE(completed_at,''),error_code"

type scanner interface{ Scan(...any) error }

func scanJob(r scanner) (j Job, err error) {
	var options string
	err = r.Scan(&j.ID, &j.Kind, &j.State, &j.Phase, &j.VolumeID, &options, &j.DedupeKey, &j.Version, &j.CreatedAt, &j.CompletedAt, &j.ErrorCode)
	j.Options = json.RawMessage(options)
	return
}
func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}
func prepareJob(j Job) (Job, error) {
	if j.ID == "" {
		j.ID = uuid.NewString()
	}
	if _, err := uuid.Parse(j.ID); err != nil {
		return Job{}, err
	}
	if len(j.Options) == 0 {
		j.Options = json.RawMessage(`{}`)
	}
	if !json.Valid(j.Options) || len(j.Options) > 65536 {
		return Job{}, errors.New("invalid job options")
	}
	j.State = domain.Queued
	j.CreatedAt = stamp()
	return j, nil
}
func enqueueJobTx(ctx context.Context, tx *sql.Tx, j Job) (Job, error) {
	if j.DedupeKey != "" {
		old, err := scanJob(tx.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE dedupe_key=?", j.DedupeKey))
		if err == nil {
			if old.Kind != j.Kind || old.VolumeID != j.VolumeID || string(old.Options) != string(j.Options) {
				return Job{}, ErrConflict
			}
			return old, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return Job{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO jobs(id,kind,state,volume_id,options_json,dedupe_key,created_at) VALUES(?,?,?,?,?,?,?)", j.ID, j.Kind, j.State, nullable(j.VolumeID), string(j.Options), nullable(j.DedupeKey), j.CreatedAt); err != nil {
		return Job{}, err
	}
	if err := event(ctx, tx, j.ID, "QUEUED", "Job queued"); err != nil {
		return Job{}, err
	}
	return j, nil
}
func (s *Store) Enqueue(ctx context.Context, j Job) (Job, error) {
	j, err := prepareJob(j)
	if err != nil {
		return Job{}, err
	}
	err = s.transaction(ctx, func(tx *sql.Tx) error { var err error; j, err = enqueueJobTx(ctx, tx, j); return err })
	return j, err
}

func (s *Store) GetJob(ctx context.Context, id string) (Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id=?", id))
}
func (s *Store) ListJobs(ctx context.Context, limit int) ([]Job, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+jobColumns+" FROM jobs ORDER BY created_at DESC,id DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
func event(ctx context.Context, tx *sql.Tx, id, code, message string) error {
	_, err := tx.ExecContext(ctx, "INSERT INTO job_events(job_id,ts,code,message) VALUES(?,?,?,?)", id, stamp(), code, message)
	return err
}
func (s *Store) Claim(ctx context.Context) (Job, error) {
	var j Job
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE active_slot=1").Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return ErrConflict
		}
		var err error
		j, err = scanJob(tx.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE state='QUEUED' OR (state='WAITING_MEDIA' AND media_recheck=1) ORDER BY created_at,id LIMIT 1"))
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE jobs SET state='PREFLIGHT',state_version=state_version+1,active_slot=1,media_recheck=0 WHERE id=? AND state_version=?", j.ID, j.Version); err != nil {
			return err
		}
		j.State = domain.Preflight
		j.Version++
		return event(ctx, tx, j.ID, "PREFLIGHT", "Scheduler claimed job")
	})
	return j, err
}

// Transition persists a compare-and-swap state update and event atomically.
func (s *Store) Transition(ctx context.Context, id string, version int64, to domain.State, phase, code string) (Job, error) {
	var j Job
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		var err error
		j, err = scanJob(tx.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id=?", id))
		if err != nil {
			return err
		}
		if j.Version != version {
			return ErrConflict
		}
		allowed := false
		for _, edge := range domain.TransitionContract() {
			if edge.From == j.State && edge.To == to {
				allowed = true
			}
		}
		if !allowed {
			return ErrConflict
		}
		var uncertain int
		if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(write_may_have_started),0) FROM write_intents WHERE job_id=?", id).Scan(&uncertain); err != nil {
			return err
		}
		if uncertain != 0 && (to == domain.Failed || to == domain.Cancelled) {
			return ErrEvidence
		}
		if to == domain.Succeeded && (j.Kind == domain.Backup || j.Kind == domain.Initialize || j.Kind == domain.Restore) {
			return ErrEvidence
		}
		if to == domain.Succeeded && j.Kind == domain.Import {
			var staged int
			if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM catalog_attempts WHERE job_id=?", id).Scan(&staged); err != nil {
				return err
			}
			if staged != 0 {
				return ErrEvidence
			}
		}
		if to == domain.Succeeded {
			if err = completion(ctx, tx, j); err != nil {
				return err
			}
		}
		active := any(1)
		completed := any(nil)
		if to == domain.WaitingMedia || terminal(to) {
			active = nil
		}
		if terminal(to) {
			completed = stamp()
		}
		result, err := tx.ExecContext(ctx, "UPDATE jobs SET state=?,phase=?,error_code=?,state_version=state_version+1,active_slot=?,completed_at=? WHERE id=? AND state_version=?", to, phase, code, active, completed, id, version)
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
		j.State = to
		j.Phase = phase
		j.ErrorCode = code
		j.Version++
		if completed != nil {
			j.CompletedAt = completed.(string)
		}
		if to == domain.RecoveryRequired {
			if _, err = tx.ExecContext(ctx, "UPDATE volumes SET state='QUARANTINED' WHERE id IN(SELECT volume_id FROM write_intents WHERE job_id=?)", id); err != nil {
				return err
			}
		}
		if terminal(to) && to != domain.Succeeded {
			if err = cleanStaging(ctx, tx, id); err != nil {
				return err
			}
		}
		return event(ctx, tx, id, string(to), "Job state changed")
	})
	return j, err
}
func terminal(s domain.State) bool {
	return s == domain.Succeeded || s == domain.Failed || s == domain.Cancelled || s == domain.RecoveryRequired
}
func completion(ctx context.Context, tx *sql.Tx, j Job) error {
	if j.Kind == domain.Backup || j.Kind == domain.Initialize || j.Kind == domain.Verify {
		if j.State != domain.Verifying {
			return ErrEvidence
		}
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM verifications WHERE job_id=?", j.ID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrEvidence
		}
		if j.Kind == domain.Backup {
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM archives WHERE job_id=? AND integrity_state='VERIFIED'", j.ID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return ErrEvidence
			}
		}
	}
	if j.Kind == domain.Restore {
		var spool, published int
		if err := tx.QueryRowContext(ctx, "SELECT verified_spool,published FROM completion_evidence WHERE job_id=?", j.ID).Scan(&spool, &published); err != nil || spool != 1 || published != 1 {
			return ErrEvidence
		}
	}
	return nil
}

// Reconcile never resumes active jobs. A durable open possibility quarantines media.
func (s *Store) Reconcile(ctx context.Context) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT id,kind,phase FROM jobs WHERE active_slot=1")
		if err != nil {
			return err
		}
		type active struct{ id, kind, phase string }
		var jobs []active
		for rows.Next() {
			var j active
			if err = rows.Scan(&j.id, &j.kind, &j.phase); err != nil {
				rows.Close()
				return err
			}
			jobs = append(jobs, j)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, j := range jobs {
			var uncertain int
			if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(write_may_have_started),0) FROM write_intents WHERE job_id=?", j.id).Scan(&uncertain); err != nil {
				return err
			}
			current, err := scanJob(tx.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id=?", j.id))
			if err != nil {
				return err
			}
			var assessment int
			if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM media_assessments WHERE job_id=? AND finalized=0", j.id).Scan(&assessment); err != nil {
				return err
			}
			// A process crash cannot prove its read descriptor and lease closed
			// or final identity remained stable. Never promote its staged prefix.
			if _, err = tx.ExecContext(ctx, "UPDATE media_assessments SET invalidated=1,complete=0 WHERE job_id=? AND finalized=0", j.id); err != nil {
				return err
			}
			if _, err = finalizeMediaAssessment(ctx, tx, current, false, false); err != nil {
				return err
			}
			state := domain.Failed
			if assessment != 0 || uncertain != 0 || (j.kind == string(domain.Restore) && strings.Contains(j.phase, "publish")) {
				state = domain.RecoveryRequired
			}
			if _, err = tx.ExecContext(ctx, "UPDATE jobs SET state=?,error_code='INTERRUPTED',state_version=state_version+1,active_slot=NULL,completed_at=? WHERE id=?", state, stamp(), j.id); err != nil {
				return err
			}
			if uncertain != 0 {
				if _, err = tx.ExecContext(ctx, "UPDATE volumes SET state='QUARANTINED' WHERE id IN(SELECT volume_id FROM write_intents WHERE job_id=?)", j.id); err != nil {
					return err
				}
			}
			if err = cleanStaging(ctx, tx, j.id); err != nil {
				return err
			}
			if err = event(ctx, tx, j.id, string(state), "Interrupted job reconciled without replay"); err != nil {
				return err
			}
		}
		return nil
	})
}

type Event struct {
	ID        int64  `json:"id"`
	JobID     string `json:"job_id"`
	Timestamp string `json:"timestamp"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

func (s *Store) Events(ctx context.Context, id string, after int64, limit int) ([]Event, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id,job_id,ts,code,message FROM job_events WHERE job_id=? AND id>? ORDER BY id LIMIT ?", id, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err = rows.Scan(&e.ID, &e.JobID, &e.Timestamp, &e.Code, &e.Message); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
