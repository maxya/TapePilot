package store

import (
	"context"
	"database/sql"
	"errors"
	"github.com/maxya/TapePilot/internal/domain"
)

type RestorePublication struct {
	SelectedMembers *int   `json:"selected_members"`
	ExpandedMembers *int   `json:"expanded_members"`
	IntegrityBasis  string `json:"integrity_basis"`
	JobID           string `json:"job_id"`
	ArchiveID       string `json:"archive_id"`
	SHA256          string `json:"sha256"`
	Bytes           int64  `json:"bytes"`
	RootID          string `json:"restore_root_id"`
	DestinationSlug string `json:"destination_slug"`
	IntentAt        string `json:"intent_at"`
	CompletedAt     string `json:"completed_at"`
}

// RecordVerifiedSpool is called only after exact spool digest, full TAR scan and
// complete path/link graph validation. Catalog byte integrity alone is insufficient.
func (s *Store) RecordVerifiedSpool(ctx context.Context, job, digestValue string, bytes int64, selected, expanded int) error {
	if !digest(digestValue) || bytes <= 0 || expanded < 0 || selected < expanded {
		return ErrEvidence
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		j, err := scanJob(tx.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id=?", job))
		if err != nil {
			return err
		}
		if j.Kind != domain.Restore || j.State != domain.Running {
			return ErrConflict
		}
		archive, _, _, err := restoreOptions(j.Options)
		if err != nil {
			return err
		}
		var expected, state string
		var expectedBytes int64
		if err = tx.QueryRowContext(ctx, "SELECT sha256,stream_bytes,integrity_state FROM archives WHERE id=?", archive).Scan(&expected, &expectedBytes, &state); err != nil {
			return err
		}
		if expected != digestValue || expectedBytes != bytes || (state != "VERIFIED" && state != "OBSERVED") {
			return ErrEvidence
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO restore_publications(job_id,archive_id,spool_sha256,spool_bytes,selected_members,expanded_members) VALUES(?,?,?,?,?,?)", job, archive, digestValue, bytes, selected, expanded); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "UPDATE archives SET restore_eligible=1 WHERE id=?", archive); err != nil {
			return err
		}
		return event(ctx, tx, job, "SPOOL_VERIFIED", "Exact spool and complete restore graph validated")
	})
}
func (s *Store) BeginRestorePublication(ctx context.Context, job, root, slug string) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		j, err := scanJob(tx.QueryRowContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE id=?", job))
		if err != nil {
			return err
		}
		if j.Kind != domain.Restore || j.State != domain.Running {
			return ErrConflict
		}
		_, expectedRoot, expectedSlug, err := restoreOptions(j.Options)
		if err != nil {
			return err
		}
		if root != expectedRoot || slug != expectedSlug {
			return ErrEvidence
		}
		r, err := tx.ExecContext(ctx, "UPDATE restore_publications SET root_id=?,destination_slug=?,intent_at=? WHERE job_id=? AND intent_at IS NULL", root, slug, stamp(), job)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrEvidence
		}
		if _, err = tx.ExecContext(ctx, "UPDATE jobs SET phase='publishing',state_version=state_version+1 WHERE id=?", job); err != nil {
			return err
		}
		return event(ctx, tx, job, "PUBLICATION_INTENT", "Fresh restore publication intent persisted")
	})
}
func (s *Store) CompleteRestorePublication(ctx context.Context, job string) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs j JOIN restore_publications r ON r.job_id=j.id WHERE j.id=? AND j.kind='RESTORE' AND j.state='RUNNING' AND j.phase='publishing' AND j.cancel_requested=0 AND r.intent_at IS NOT NULL AND r.completed_at IS NULL", job).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return ErrEvidence
		}
		now := stamp()
		if _, err := tx.ExecContext(ctx, "UPDATE restore_publications SET completed_at=? WHERE job_id=?", now, job); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE jobs SET state='SUCCEEDED',phase='published',state_version=state_version+1,active_slot=NULL,completed_at=? WHERE id=?", now, job); err != nil {
			return err
		}
		return event(ctx, tx, job, "SUCCEEDED", "Verified restore destination published")
	})
}
func (s *Store) RestorePublication(ctx context.Context, job string) (RestorePublication, error) {
	var r RestorePublication
	err := s.db.QueryRowContext(ctx, "SELECT r.job_id,r.archive_id,r.spool_sha256,r.spool_bytes,COALESCE(r.root_id,''),COALESCE(r.destination_slug,''),COALESCE(r.intent_at,''),COALESCE(r.completed_at,''),r.selected_members,r.expanded_members,a.integrity_basis FROM restore_publications r JOIN archives a ON a.id=r.archive_id WHERE r.job_id=?", job).Scan(&r.JobID, &r.ArchiveID, &r.SHA256, &r.Bytes, &r.RootID, &r.DestinationSlug, &r.IntentAt, &r.CompletedAt, &r.SelectedMembers, &r.ExpandedMembers, &r.IntegrityBasis)
	return r, err
}

// The old unbound publication marker is closed. Use the verified spool and
// publication intent methods so cancellation and crashes remain recoverable.
func (s *Store) RecordRestorePublication(ctx context.Context, id string) error {
	return errors.New("use durable restore publication intent")
}
