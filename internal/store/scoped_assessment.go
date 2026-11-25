package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/maxya/TapePilot/internal/domain"
)

// A restore or managed re-import can disprove a catalog entry without running
// a standalone verify job. Bind this evidence to the observed on-media header,
// immutable job and physical file. Never use a private spool error as evidence.
type scopedAssessment struct {
	jobID, volumeID, header string
	index                   int64
	suffix                  bool
	cause                   error
}

func (e *scopedAssessment) Error() string { return e.cause.Error() }
func (e *scopedAssessment) Unwrap() error { return e.cause }

// ManagedReadMismatch attaches an already observed raw-media mismatch to its job.
// suffix applies to a managed import whose scan stopped at the first bad pair.
func ManagedReadMismatch(jobID, volumeID, header string, dataIndex int64, suffix bool, cause error) error {
	if cause == nil {
		cause = ErrEvidence
	}
	return &scopedAssessment{jobID: jobID, volumeID: volumeID, header: header, index: dataIndex, suffix: suffix, cause: cause}
}

func applyScopedAssessment(ctx context.Context, tx *sql.Tx, j Job, runErr error) error {
	var e *scopedAssessment
	if !errors.As(runErr, &e) {
		return nil
	}
	if e.jobID != j.ID || !digest(e.header) || e.index < 1 || e.index%2 != 1 || (j.VolumeID != "" && j.VolumeID != e.volumeID) {
		return ErrEvidence
	}
	var header string
	if err := tx.QueryRowContext(ctx, "SELECT header_sha256 FROM volumes WHERE id=? AND kind='MANAGED'", e.volumeID).Scan(&header); err != nil {
		return err
	}
	if header != e.header {
		return ErrEvidence
	}
	switch j.Kind {
	case domain.Restore:
		archive, _, _, err := restoreOptions(j.Options)
		if err != nil {
			return err
		}
		var index int64
		if err := tx.QueryRowContext(ctx, "SELECT data_file_index FROM archives WHERE id=? AND volume_id=?", archive, e.volumeID).Scan(&index); err != nil {
			return err
		}
		if e.suffix || index != e.index {
			return ErrEvidence
		}
	case domain.Import:
		var opts struct {
			Mode           string
			ExpectedVolume string `json:"expected_volume_id"`
		}
		if json.Unmarshal(j.Options, &opts) != nil || opts.Mode != "managed" || (opts.ExpectedVolume != "" && opts.ExpectedVolume != e.volumeID) {
			return ErrEvidence
		}
	default:
		return ErrEvidence
	}
	if _, err := tx.ExecContext(ctx, "UPDATE archives SET integrity_state='FAILED',restore_eligible=0 WHERE volume_id=? AND data_file_index=?", e.volumeID, e.index); err != nil {
		return err
	}
	if e.suffix {
		if _, err := tx.ExecContext(ctx, "UPDATE archives SET integrity_state='UNKNOWN',restore_eligible=0 WHERE volume_id=? AND data_file_index>? AND integrity_state!='FAILED'", e.volumeID, e.index); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE volumes SET state='QUARANTINED' WHERE id=? AND state!='SEALED'", e.volumeID); err != nil {
		return err
	}
	return event(ctx, tx, j.ID, "MEDIA_READ_MISMATCH", "Identified media differs from catalog evidence")
}
