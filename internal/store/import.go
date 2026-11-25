package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/media"
	"strings"
)

var ErrCollision = errors.New("CATALOG_IDENTITY_COLLISION")

func canonicalMetadata(metadata json.RawMessage) ([]byte, error) {
	if len(metadata) == 0 || len(metadata) > 65536 || !json.Valid(metadata) {
		return nil, ErrEvidence
	}
	var obj map[string]any
	decoder := json.NewDecoder(bytes.NewReader(metadata))
	decoder.UseNumber()
	if err := decoder.Decode(&obj); err != nil || obj == nil {
		return nil, ErrEvidence
	}
	return json.Marshal(obj)
}

// RegisterImportedVolume accepts only already validated read-only media metadata.
// It preserves every existing operator policy and never enables enrollment.
func (s *Store) RegisterImportedVolume(ctx context.Context, job string, v Volume, metadata json.RawMessage) error {
	if !ValidBarcode(v.Barcode) {
		return ErrOperations
	}
	if _, err := uuid.Parse(v.ID); err != nil {
		return err
	}
	if (v.Kind == "MANAGED" && (!digest(v.HeaderSHA256) || v.State != domain.Managed)) || (v.Kind == "LEGACY" && (v.HeaderSHA256 != "" || v.State != domain.LegacyReadOnly)) {
		return ErrEvidence
	}
	if v.Kind == "MANAGED" {
		var label media.Volume
		if err := json.Unmarshal(metadata, &label); err != nil {
			return err
		}
		if err := label.Validate(); err != nil {
			return err
		}
		if label.VolumeID != v.ID {
			return ErrEvidence
		}
	}
	canonical, err := canonicalMetadata(metadata)
	if err != nil {
		return err
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if err := requireImport(ctx, tx, job); err != nil {
			return err
		}
		var kind, header string
		err := tx.QueryRowContext(ctx, "SELECT kind,COALESCE(header_sha256,'') FROM volumes WHERE id=?", v.ID).Scan(&kind, &header)
		if err == nil {
			if kind != v.Kind || header != v.HeaderSHA256 {
				return ErrCollision
			}
		} else if errors.Is(err, sql.ErrNoRows) {
			if v.CreatedAt == "" {
				v.CreatedAt = stamp()
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO volumes(id,kind,label,location,generation,state,header_sha256,created_at,barcode) VALUES(?,?,?,?,?,?,?,?,?)", v.ID, v.Kind, v.Label, v.Location, v.Generation, v.State, nullable(v.HeaderSHA256), v.CreatedAt, v.Barcode); err != nil {
				return err
			}
		} else {
			return err
		}
		return saveMetadata(ctx, tx, v.ID, canonical)
	})
}
func requireImport(ctx context.Context, tx *sql.Tx, job string) error {
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE id=? AND kind='IMPORT' AND state='RUNNING' AND cancel_requested=0", job).Scan(&count); err != nil {
		return err
	}
	if count != 1 {
		return ErrConflict
	}
	return nil
}
func saveMetadata(ctx context.Context, tx *sql.Tx, volume string, metadata []byte) error {
	var existing string
	err := tx.QueryRowContext(ctx, "SELECT metadata_json FROM volume_metadata WHERE volume_id=?", volume).Scan(&existing)
	if err == nil {
		if !bytes.Equal([]byte(existing), metadata) {
			return ErrCollision
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO volume_metadata VALUES(?,?)", volume, string(metadata))
	return err
}
func (s *Store) SaveVolumeMetadata(ctx context.Context, volume string, metadata json.RawMessage) error {
	canonical, err := canonicalMetadata(metadata)
	if err != nil {
		return err
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var kind string
		if err := tx.QueryRowContext(ctx, "SELECT kind FROM volumes WHERE id=?", volume).Scan(&kind); err != nil {
			return err
		}
		if kind == "MANAGED" {
			var label media.Volume
			if err := json.Unmarshal(metadata, &label); err != nil {
				return err
			}
			if err := label.Validate(); err != nil {
				return err
			}
			if label.VolumeID != volume {
				return ErrEvidence
			}
		}
		return saveMetadata(ctx, tx, volume, canonical)
	})
}

func (s *Store) ImportedVolumeMetadata(ctx context.Context, volume string) (json.RawMessage, error) {
	var raw string
	err := s.db.QueryRowContext(ctx, "SELECT metadata_json FROM volume_metadata WHERE volume_id=?", volume).Scan(&raw)
	return json.RawMessage(raw), err
}
func (s *Store) StageImportMembers(ctx context.Context, job, attempt string, members []Member) error {
	if _, err := uuid.Parse(attempt); err != nil {
		return err
	}
	if len(members) > 1000 {
		return ErrEvidence
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if err := requireImport(ctx, tx, job); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO catalog_attempts VALUES(?,?) ON CONFLICT(attempt_id) DO NOTHING", attempt, job); err != nil {
			return err
		}
		var owner string
		if err := tx.QueryRowContext(ctx, "SELECT job_id FROM catalog_attempts WHERE attempt_id=?", attempt).Scan(&owner); err != nil {
			return err
		}
		if owner != job {
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
func (s *Store) PublishImportedArchive(ctx context.Context, job, attempt string, a Archive) error {
	if a.JobID != job || !digest(a.SHA256) || a.StreamBytes <= 0 || a.Sequence < 1 || a.DataIndex < 0 {
		return ErrEvidence
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if err := requireImport(ctx, tx, job); err != nil {
			return err
		}
		var kind string
		if err := tx.QueryRowContext(ctx, "SELECT kind FROM volumes WHERE id=?", a.VolumeID).Scan(&kind); err != nil {
			return err
		}
		switch kind {
		case "MANAGED":
			if a.Integrity != domain.Verified || a.IntegrityBasis != "ON_MEDIA_MANIFEST" || a.CommitIndex == nil || a.Sequence > (1<<62)-1 || a.DataIndex != 2*a.Sequence-1 || *a.CommitIndex != 2*a.Sequence || !digest(a.CommitSHA256) || a.StreamBytes%262144 != 0 {
				return ErrEvidence
			}
		case "LEGACY":
			if a.Integrity != domain.Observed || a.IntegrityBasis != "OBSERVED_NOW" || a.CommitIndex != nil || a.CommitSHA256 != "" || a.VerifiedAt != "" {
				return ErrEvidence
			}
		default:
			return ErrEvidence
		}
		var owner string
		if err := tx.QueryRowContext(ctx, "SELECT job_id FROM catalog_attempts WHERE attempt_id=?", attempt).Scan(&owner); err != nil {
			return err
		}
		if owner != job {
			return ErrConflict
		}
		var oldID, oldSHA, oldCommit, oldVolume string
		var oldData int64
		err := tx.QueryRowContext(ctx, "SELECT id,sha256,COALESCE(commit_sha256,''),volume_id,data_file_index FROM archives WHERE id=? OR (volume_id=? AND data_file_index=?)", a.ID, a.VolumeID, a.DataIndex).Scan(&oldID, &oldSHA, &oldCommit, &oldVolume, &oldData)
		if err == nil {
			if oldID != a.ID || oldSHA != a.SHA256 || oldCommit != a.CommitSHA256 || oldVolume != a.VolumeID || oldData != a.DataIndex {
				return ErrCollision
			}
			// A verified existing pair is immutable. Re-import cannot replace its evidence.
		} else if errors.Is(err, sql.ErrNoRows) {
			if err = insertArchive(ctx, tx, a); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO members(archive_id,path,raw_path,parent_path,type,size,mtime,mode,link_target) SELECT ?,path,raw_path,parent_path,type,size,mtime,mode,link_target FROM staged_members WHERE attempt_id=?", a.ID, attempt); err != nil {
				return err
			}
		} else {
			return err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM staged_members WHERE attempt_id=?", attempt); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM catalog_attempts WHERE attempt_id=?", attempt)
		return err
	})
}
func (s *Store) DiscardImportAttempt(ctx context.Context, job, attempt string) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var owner string
		if err := tx.QueryRowContext(ctx, "SELECT job_id FROM catalog_attempts WHERE attempt_id=?", attempt).Scan(&owner); err != nil {
			return err
		}
		if owner != job {
			return ErrConflict
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM staged_members WHERE attempt_id=?", attempt); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM catalog_attempts WHERE attempt_id=?", attempt)
		return err
	})
}
func cleanStaging(ctx context.Context, tx *sql.Tx, job string) error {
	if _, err := tx.ExecContext(ctx, "DELETE FROM staged_members WHERE attempt_id=? OR attempt_id IN(SELECT attempt_id FROM catalog_attempts WHERE job_id=?)", job, job); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, "DELETE FROM catalog_attempts WHERE job_id=?", job)
	return err
}

// Restore archive identity in options is immutable, as are all job options.
func restoreOptions(raw json.RawMessage) (archive, root, slug string, err error) {
	var opts struct {
		Archive string `json:"archive_id"`
		Root    string `json:"restore_root_id"`
		Slug    string `json:"destination_slug"`
	}
	err = json.Unmarshal(raw, &opts)
	if err != nil {
		return
	}
	if opts.Archive == "" || opts.Root == "" || opts.Slug == "" || len(opts.Slug) > 128 || strings.ContainsAny(opts.Slug, "/\\\x00") || opts.Slug == "." || opts.Slug == ".." {
		err = ErrEvidence
		return
	}
	return opts.Archive, opts.Root, opts.Slug, nil
}
