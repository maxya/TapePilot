package store

import (
	"context"
	"database/sql"
	"errors"

	"github.com/maxya/TapePilot/internal/domain"
)

// MediaAssessment binds a read-only attempt to the freshly observed managed
// header, under the tape lease. It grants no write authority.
type MediaAssessment struct {
	VolumeID, HeaderSHA256 string
}

func (s *Store) BeginMediaAssessment(ctx context.Context, jobID string, a MediaAssessment) error {
	if !digest(a.HeaderSHA256) {
		return ErrEvidence
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs j JOIN volumes v ON v.id=j.volume_id WHERE j.id=? AND j.kind='VERIFY' AND j.state IN('PREFLIGHT','RUNNING','VERIFYING') AND j.cancel_requested=0 AND v.id=? AND v.kind='MANAGED' AND v.header_sha256=?`, jobID, a.VolumeID, a.HeaderSHA256).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return ErrEvidence
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO media_assessments(job_id,volume_id,header_sha256,created_at) VALUES(?,?,?,?)`, jobID, a.VolumeID, a.HeaderSHA256, stamp())
		return err
	})
}

// CheckpointMediaAssessment stages only bytes matching immutable catalog
// baselines. Current catalog state changes together with the terminal outcome.
func (s *Store) CheckpointMediaAssessment(ctx context.Context, jobID, archiveID string, index int64, actualSHA string, actualBytes int64, actualCommitSHA string) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM media_assessments m JOIN jobs j ON j.id=m.job_id JOIN archives a ON a.volume_id=m.volume_id WHERE m.job_id=? AND m.finalized=0 AND m.complete=0 AND m.invalidated=0 AND m.failed_index IS NULL AND j.state='VERIFYING' AND j.cancel_requested=0 AND a.id=? AND a.data_file_index=? AND a.sha256=? AND a.stream_bytes=? AND a.commit_sha256=? AND a.data_file_index=COALESCE((SELECT MAX(data_file_index)+2 FROM media_assessment_passes WHERE job_id=m.job_id),1)`, jobID, archiveID, index, actualSHA, actualBytes, actualCommitSHA).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return ErrEvidence
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO media_assessment_passes VALUES(?,?,?,?)`, jobID, archiveID, index, stamp())
		return err
	})
}

// FailMediaAssessment distinguishes directly disproved archive bytes from an
// unread remainder. Cancellation and operational read failures are not proof
// of corruption. The supplied index must be the next unchecked pair.
func (s *Store) FailMediaAssessment(ctx context.Context, jobID string, index int64, confirmedCorruption bool) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE media_assessments SET failed_index=?,confirmed_corruption=?,complete=0 WHERE job_id=? AND finalized=0 AND failed_index IS NULL AND ?=COALESCE((SELECT MAX(data_file_index)+2 FROM media_assessment_passes WHERE job_id=?),1)`, index, confirmedCorruption, jobID, index, jobID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err == nil && n != 1 {
			return ErrEvidence
		}
		return err
	})
}

func (s *Store) CompleteMediaAssessment(ctx context.Context, jobID string) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE media_assessments SET complete=1 WHERE job_id=? AND finalized=0 AND invalidated=0 AND failed_index IS NULL AND EXISTS(SELECT 1 FROM jobs WHERE id=? AND state='VERIFYING' AND cancel_requested=0) AND (SELECT COUNT(*) FROM media_assessment_passes WHERE job_id=?)=(SELECT COUNT(*) FROM archives WHERE volume_id=media_assessments.volume_id)`, jobID, jobID, jobID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err == nil && n != 1 {
			return ErrEvidence
		}
		return err
	})
}

// finalizeMediaAssessment runs after resources close, in the same transaction
// as job completion/recovery. Interrupted attempts retain validated prefix
// evidence and conservatively withdraw current integrity for the unread tail.
func finalizeMediaAssessment(ctx context.Context, tx *sql.Tx, j Job, success, cancelled bool) (bool, error) {
	var volumeID, header string
	var complete, finalized, corrupt, invalidated int
	var failed sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT volume_id,header_sha256,complete,finalized,failed_index,confirmed_corruption,invalidated FROM media_assessments WHERE job_id=?`, j.ID).Scan(&volumeID, &header, &complete, &finalized, &failed, &corrupt, &invalidated)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if finalized != 0 {
		return false, ErrConflict
	}
	if j.Kind != domain.Verify || j.VolumeID != volumeID {
		return false, ErrEvidence
	}
	var baseline string
	if err = tx.QueryRowContext(ctx, `SELECT header_sha256 FROM volumes WHERE id=?`, volumeID).Scan(&baseline); err != nil {
		return false, err
	}
	if baseline != header {
		return false, ErrEvidence
	}
	passed := success && complete == 1 && invalidated == 0
	untrusted := invalidated != 0 || (complete == 1 && !success)
	// Historical successful timestamps and baseline digests survive all failures.
	if _, err = tx.ExecContext(ctx, `UPDATE archives SET integrity_state='UNKNOWN',restore_eligible=0 WHERE volume_id=? AND integrity_state!='FAILED' AND (? OR id NOT IN(SELECT archive_id FROM media_assessment_passes WHERE job_id=?))`, volumeID, untrusted, j.ID); err != nil {
		return false, err
	}
	if failed.Valid && corrupt != 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE archives SET integrity_state='FAILED',restore_eligible=0 WHERE volume_id=? AND data_file_index=?`, volumeID, failed.Int64); err != nil {
			return false, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.sha256,a.stream_bytes,a.commit_sha256,p.checked_at FROM archives a JOIN media_assessment_passes p ON p.archive_id=a.id WHERE p.job_id=? AND ?=0 ORDER BY p.data_file_index`, j.ID, untrusted)
	if err != nil {
		return false, err
	}
	type checked struct {
		id, sha, commit, at string
		size                int64
	}
	var entries []checked
	for rows.Next() {
		var e checked
		if err = rows.Scan(&e.id, &e.sha, &e.size, &e.commit, &e.at); err != nil {
			rows.Close()
			return false, err
		}
		entries = append(entries, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if err = insertVerification(ctx, tx, Verification{JobID: j.ID, ArchiveID: e.id, Basis: "CATALOG_READBACK", ExpectedSHA256: e.sha, ActualSHA256: e.sha, ExpectedBytes: e.size, ActualBytes: e.size, ActualCommitSHA256: e.commit, IdentityValid: true, FormatValid: true}); err != nil {
			return false, err
		}
		// A byte readback cannot establish a previously unvalidated extraction graph.
		if _, err = tx.ExecContext(ctx, `UPDATE archives SET integrity_state='VERIFIED',verified_at=? WHERE id=?`, e.at, e.id); err != nil {
			return false, err
		}
	}
	if !passed && (!cancelled || corrupt != 0 || invalidated != 0) {
		if _, err = tx.ExecContext(ctx, `UPDATE volumes SET state='QUARANTINED' WHERE id=? AND state!='SEALED'`, volumeID); err != nil {
			return false, err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE media_assessments SET finalized=1 WHERE job_id=?`, j.ID)
	return passed, err
}

// InvalidateMediaAssessment withdraws staged identity-dependent evidence after
// a header/identity change. It does not assert that any bytes are corrupted.
func (s *Store) InvalidateMediaAssessment(ctx context.Context, jobID string) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE media_assessments SET invalidated=1,complete=0 WHERE job_id=? AND finalized=0", jobID)
		return err
	})
}
