package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/media"
	"math"
	"strings"
)

type Volume struct {
	Barcode      string            `json:"barcode,omitempty"`
	ID           string            `json:"id"`
	Kind         string            `json:"kind"`
	Label        string            `json:"label"`
	Location     string            `json:"location"`
	Generation   string            `json:"generation"`
	State        domain.MediaState `json:"state"`
	HeaderSHA256 string            `json:"header_sha256"`
	CreatedAt    string            `json:"created_at"`
}

func (s *Store) CreateVolume(ctx context.Context, v Volume) error {
	if !ValidBarcode(v.Barcode) {
		return ErrOperations
	}
	if _, err := uuid.Parse(v.ID); err != nil {
		return err
	}
	if (v.Kind == "MANAGED" && v.State != domain.BlankCandidate && v.State != domain.Unclassified && v.State != domain.Unknown) || (v.Kind == "LEGACY" && v.State != domain.LegacyReadOnly) || v.State == domain.Managed || v.HeaderSHA256 != "" {
		return ErrEvidence
	}
	if v.CreatedAt == "" {
		v.CreatedAt = stamp()
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO volumes(id,kind,label,location,generation,state,header_sha256,created_at,barcode) VALUES(?,?,?,?,?,?,?,?,?)", v.ID, v.Kind, v.Label, v.Location, v.Generation, v.State, nullable(v.HeaderSHA256), v.CreatedAt, v.Barcode)
		return err
	})
}
func (s *Store) GetVolume(ctx context.Context, id string) (v Volume, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT id,kind,label,location,generation,state,COALESCE(header_sha256,''),created_at,barcode FROM volumes WHERE id=?", id).Scan(&v.ID, &v.Kind, &v.Label, &v.Location, &v.Generation, &v.State, &v.HeaderSHA256, &v.CreatedAt, &v.Barcode)
	return
}

// SetVolumeState permits only protective changes. Clearing protection needs a
// separately audited read-only investigation and is deliberately unavailable.
func (s *Store) SetVolumeState(ctx context.Context, id string, state domain.MediaState) error {
	if state != domain.Sealed && state != domain.Quarantined {
		return ErrEvidence
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, "UPDATE volumes SET state=? WHERE id=?", state, id)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if n != 1 {
			return ErrNotFound
		}
		return err
	})
}

type WriteIntent struct {
	ID, JobID, VolumeID, DriveID, PreviousCommitSHA256, Phase string
	DataIndex, CommitIndex                                    int64
}

func (s *Store) BeginWriteIntent(ctx context.Context, w WriteIntent) error {
	if w.ID == "" {
		w.ID = uuid.NewString()
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var state domain.State
		var kind domain.Kind
		var volume string
		var media domain.MediaState
		var volumeKind string
		if err := tx.QueryRowContext(ctx, "SELECT j.state,j.kind,COALESCE(j.volume_id,''),v.state,v.kind FROM jobs j JOIN volumes v ON v.id=j.volume_id WHERE j.id=?", w.JobID).Scan(&state, &kind, &volume, &media, &volumeKind); err != nil {
			return err
		}
		if state != domain.Running || volume != w.VolumeID || volumeKind != "MANAGED" || w.DriveID == "" {
			return ErrConflict
		}
		switch kind {
		case domain.Backup:
			if media != domain.Managed || w.DataIndex < 1 || w.DataIndex%2 != 1 || w.DataIndex == math.MaxInt64 || w.CommitIndex != w.DataIndex+1 {
				return ErrEvidence
			}
			if (w.DataIndex == 1 && w.PreviousCommitSHA256 != "") || (w.DataIndex > 1 && !digest(w.PreviousCommitSHA256)) {
				return ErrEvidence
			}
		case domain.Initialize:
			if media != domain.BlankCandidate || w.DataIndex != 0 || w.CommitIndex != 0 || w.PreviousCommitSHA256 != "" {
				return ErrEvidence
			}
		default:
			return ErrEvidence
		}

		_, err := tx.ExecContext(ctx, "INSERT INTO write_intents(id,job_id,volume_id,drive_id,expected_data_index,expected_commit_index,previous_commit_sha256,phase,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)", w.ID, w.JobID, w.VolumeID, w.DriveID, w.DataIndex, w.CommitIndex, nullable(w.PreviousCommitSHA256), w.Phase, stamp(), stamp())
		return err
	})
}

// MarkWriteOpened must complete before any adapter writer can be opened.
func (s *Store) MarkWriteOpened(ctx context.Context, job string) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, "UPDATE write_intents SET write_may_have_started=1,updated_at=? WHERE job_id=? AND write_may_have_started=0 AND EXISTS(SELECT 1 FROM jobs j JOIN volumes v ON v.id=j.volume_id WHERE j.id=job_id AND j.state='RUNNING' AND ((j.kind='BACKUP' AND v.state='MANAGED') OR (j.kind='INITIALIZE' AND v.state='BLANK_CANDIDATE')))", stamp(), job)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if n != 1 {
			return ErrNotFound
		}
		return err
	})
}
func (s *Store) RecordExpectedWrite(ctx context.Context, job, dataDigest string, bytes int64, commitDigest string) error {
	if !digest(dataDigest) || !digest(commitDigest) || bytes <= 0 || bytes%262144 != 0 {
		return ErrEvidence
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, "UPDATE write_intents SET expected_data_sha256=?,expected_data_bytes=?,expected_commit_sha256=?,updated_at=? WHERE job_id=? AND expected_data_sha256 IS NULL AND write_may_have_started=1 AND EXISTS(SELECT 1 FROM jobs WHERE id=job_id AND state='RUNNING')", dataDigest, bytes, commitDigest, stamp(), job)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if n != 1 {
			return ErrConflict
		}
		return err
	})
}

type Member struct {
	ID         int64  `json:"id"`
	Path       string `json:"path"`
	RawPath    string `json:"raw_path"`
	ParentPath string `json:"parent_path"`
	Type       string `json:"type"`
	Size       int64  `json:"size"`
	Mtime      string `json:"mtime"`
	Mode       int64  `json:"mode"`
	LinkTarget string `json:"link_target"`
}

func (s *Store) StageMembers(ctx context.Context, attempt string, members []Member) error {
	if _, err := uuid.Parse(attempt); err != nil {
		return err
	}
	if len(members) > 1000 {
		return errors.New("staging batch exceeds 1000 members")
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE id=? AND state='VERIFYING'", attempt).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return ErrConflict
		}
		stmt, err := tx.PrepareContext(ctx, "INSERT INTO staged_members VALUES(?,?,?,?,?,?,?,?,?)")
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, m := range members {
			if _, err = stmt.ExecContext(ctx, attempt, m.Path, m.RawPath, m.ParentPath, m.Type, m.Size, m.Mtime, m.Mode, m.LinkTarget); err != nil {
				return err
			}
		}
		return nil
	})
}

type Archive struct {
	ID              string           `json:"id"`
	VolumeID        string           `json:"volume_id"`
	JobID           string           `json:"job_id"`
	Sequence        int64            `json:"sequence"`
	DataIndex       int64            `json:"data_file_index"`
	CommitIndex     *int64           `json:"commit_file_index"`
	SourceLabel     string           `json:"source_label"`
	CreatedAt       string           `json:"created_at"`
	StreamBytes     int64            `json:"stream_bytes"`
	SHA256          string           `json:"sha256"`
	CommitSHA256    string           `json:"commit_sha256"`
	Integrity       domain.Integrity `json:"integrity"`
	IntegrityBasis  string           `json:"integrity_basis"`
	VerifiedAt      string           `json:"verified_at"`
	RestoreEligible bool             `json:"restore_eligible"`
}
type Verification struct {
	ID, JobID, ArchiveID, Basis, ExpectedSHA256, ActualSHA256 string
	ActualCommitSHA256                                        string
	Metadata                                                  json.RawMessage
	ExpectedBytes, ActualBytes                                int64
	IdentityValid, FormatValid                                bool
}

func digest(s string) bool {
	if len(s) != 64 {
		return false
	}
	return strings.Trim(s, "0123456789abcdef") == ""
}
func validVerification(v Verification) bool {
	return digest(v.ExpectedSHA256) && v.ExpectedSHA256 == v.ActualSHA256 && v.ExpectedBytes == v.ActualBytes && v.ExpectedBytes > 0 && v.IdentityValid && v.FormatValid
}
func insertVerification(ctx context.Context, tx *sql.Tx, v Verification) error {
	if !validVerification(v) {
		return ErrEvidence
	}
	if v.ID == "" {
		v.ID = uuid.NewString()
	}
	_, err := tx.ExecContext(ctx, "INSERT INTO verifications VALUES(?,?,?,?,?,?,?,?,?,?,?,?)", v.ID, v.JobID, nullable(v.ArchiveID), v.Basis, v.ExpectedSHA256, v.ActualSHA256, nullable(v.ActualCommitSHA256), v.ExpectedBytes, v.ActualBytes, v.IdentityValid, v.FormatValid, stamp())
	return err
}

// RecordVerification persists a read-only verification result. Managed writes
// publish evidence only through PublishArchive or PublishInitializedVolume.
func (s *Store) RecordVerification(ctx context.Context, v Verification) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var kind domain.Kind
		var state domain.State
		if err := tx.QueryRowContext(ctx, "SELECT kind,state FROM jobs WHERE id=?", v.JobID).Scan(&kind, &state); err != nil {
			return err
		}
		if kind != domain.Verify || state != domain.Verifying {
			return ErrConflict
		}
		return insertVerification(ctx, tx, v)
	})
}

// PublishArchive promotes only independently scanned members and matching readback evidence.
func (s *Store) PublishArchive(ctx context.Context, attempt string, a Archive, v Verification) error {
	if attempt != a.JobID || a.CommitIndex == nil || a.Sequence < 1 || a.Sequence > math.MaxInt64/2 || a.DataIndex != 2*a.Sequence-1 || *a.CommitIndex != 2*a.Sequence || a.ID != v.ArchiveID || a.JobID != v.JobID || a.SHA256 != v.ActualSHA256 || a.StreamBytes != v.ActualBytes || a.Integrity != domain.Verified || !digest(a.CommitSHA256) {
		return ErrEvidence
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var data, commit string
		var bytes, dataIndex, commitIndex int64
		var volume string
		var kind domain.Kind
		var state domain.State
		if err := tx.QueryRowContext(ctx, "SELECT kind,state FROM jobs WHERE id=?", a.JobID).Scan(&kind, &state); err != nil {
			return err
		}
		if kind != domain.Backup || state != domain.Verifying {
			return ErrConflict
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM staged_members WHERE attempt_id=?", attempt).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return ErrEvidence
		}
		if err := tx.QueryRowContext(ctx, "SELECT expected_data_sha256,expected_data_bytes,expected_commit_sha256,volume_id,expected_data_index,expected_commit_index FROM write_intents WHERE job_id=? AND write_may_have_started=1", a.JobID).Scan(&data, &bytes, &commit, &volume, &dataIndex, &commitIndex); err != nil {
			return err
		}
		if volume != a.VolumeID || dataIndex != a.DataIndex || commitIndex != *a.CommitIndex || data != a.SHA256 || bytes != a.StreamBytes || commit != a.CommitSHA256 || commit != v.ActualCommitSHA256 {
			return ErrEvidence
		}
		if err := insertArchive(ctx, tx, a); err != nil {
			return err
		}
		if err := insertVerification(ctx, tx, v); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO members(archive_id,path,raw_path,parent_path,type,size,mtime,mode,link_target) SELECT ?,path,raw_path,parent_path,type,size,mtime,mode,link_target FROM staged_members WHERE attempt_id=?", a.ID, attempt); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM staged_members WHERE attempt_id=?", attempt); err != nil {
			return err
		}
		return succeed(ctx, tx, a.JobID)
	})
}
func insertArchive(ctx context.Context, tx *sql.Tx, a Archive) error {
	if a.CreatedAt == "" {
		a.CreatedAt = stamp()
	}
	normalized, err := catalogInstant(a.CreatedAt)
	if err != nil {
		return err
	}
	a.CreatedAt = normalized
	if a.VerifiedAt == "" && a.Integrity == domain.Verified {
		a.VerifiedAt = stamp()
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO archives VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)", a.ID, a.VolumeID, nullable(a.JobID), a.Sequence, a.DataIndex, a.CommitIndex, a.SourceLabel, a.CreatedAt, a.StreamBytes, a.SHA256, nullable(a.CommitSHA256), a.Integrity, a.IntegrityBasis, nullable(a.VerifiedAt), a.RestoreEligible)
	return err
}
func (s *Store) GetArchive(ctx context.Context, id string) (a Archive, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT id,volume_id,COALESCE(job_id,''),sequence,data_file_index,commit_file_index,source_label,created_at,stream_bytes,sha256,COALESCE(commit_sha256,''),integrity_state,integrity_basis,COALESCE(verified_at,''),restore_eligible FROM archives WHERE id=?", id).Scan(&a.ID, &a.VolumeID, &a.JobID, &a.Sequence, &a.DataIndex, &a.CommitIndex, &a.SourceLabel, &a.CreatedAt, &a.StreamBytes, &a.SHA256, &a.CommitSHA256, &a.Integrity, &a.IntegrityBasis, &a.VerifiedAt, &a.RestoreEligible)
	return
}
func (s *Store) ListMembers(ctx context.Context, archive string, after int64, limit int) ([]Member, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id,path,raw_path,parent_path,type,size,mtime,mode,link_target FROM members WHERE archive_id=? AND id>? ORDER BY id LIMIT ?", archive, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		if err = rows.Scan(&m.ID, &m.Path, &m.RawPath, &m.ParentPath, &m.Type, &m.Size, &m.Mtime, &m.Mode, &m.LinkTarget); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// RecordExpectedHeader binds the expected immutable label before readback.
func (s *Store) RecordExpectedHeader(ctx context.Context, job, digestValue string, bytes int64) error {
	if !digest(digestValue) || bytes <= 0 || bytes > 1048576 || bytes%262144 != 0 {
		return ErrEvidence
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, "UPDATE write_intents SET expected_data_sha256=?,expected_data_bytes=?,updated_at=? WHERE job_id=? AND expected_data_sha256 IS NULL AND write_may_have_started=1 AND EXISTS(SELECT 1 FROM jobs WHERE id=job_id AND kind='INITIALIZE' AND state='RUNNING')", digestValue, bytes, stamp(), job)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if n != 1 {
			return ErrConflict
		}
		return err
	})
}
func (s *Store) PublishInitializedVolume(ctx context.Context, job string, v Verification) error {
	if job != v.JobID || v.ArchiveID != "" || !validVerification(v) {
		return ErrEvidence
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var expected, volume string
		var bytes int64
		if err := tx.QueryRowContext(ctx, "SELECT w.expected_data_sha256,w.expected_data_bytes,w.volume_id FROM write_intents w JOIN jobs j ON j.id=w.job_id JOIN volumes v ON v.id=w.volume_id WHERE j.id=? AND j.kind='INITIALIZE' AND j.state='VERIFYING' AND w.write_may_have_started=1 AND v.state='BLANK_CANDIDATE'", job).Scan(&expected, &bytes, &volume); err != nil {
			return err
		}
		if expected != v.ExpectedSHA256 || bytes != v.ExpectedBytes {
			return ErrEvidence
		}
		if err := insertVerification(ctx, tx, v); err != nil {
			return err
		}
		if len(v.Metadata) > 0 {
			var label media.Volume
			if err := json.Unmarshal(v.Metadata, &label); err != nil {
				return err
			}
			if err := label.Validate(); err != nil {
				return err
			}
			if label.VolumeID != volume {
				return ErrEvidence
			}
			canonical, err := canonicalMetadata(v.Metadata)
			if err != nil {
				return err
			}
			if err = saveMetadata(ctx, tx, volume, canonical); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE volumes SET state='MANAGED',header_sha256=? WHERE id=?", expected, volume); err != nil {
			return err
		}
		return succeed(ctx, tx, job)
	})
}
func succeed(ctx context.Context, tx *sql.Tx, job string) error {
	if _, err := tx.ExecContext(ctx, "UPDATE jobs SET state='SUCCEEDED',state_version=state_version+1,active_slot=NULL,completed_at=? WHERE id=? AND state='VERIFYING'", stamp(), job); err != nil {
		return err
	}
	return event(ctx, tx, job, "SUCCEEDED", "Readback and durable publication completed")
}

func (s *Store) ListVolumes(ctx context.Context, limit int) ([]Volume, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id,kind,label,location,generation,state,COALESCE(header_sha256,''),created_at,barcode FROM volumes ORDER BY created_at,id LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Volume{}
	for rows.Next() {
		var v Volume
		if err = rows.Scan(&v.ID, &v.Kind, &v.Label, &v.Location, &v.Generation, &v.State, &v.HeaderSHA256, &v.CreatedAt, &v.Barcode); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) ArchiveCount(ctx context.Context, volume string) (int64, error) {
	var count int64
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM archives WHERE volume_id=?", volume).Scan(&count)
	return count, err
}
