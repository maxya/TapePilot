package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type MemberQuery struct {
	Text, VolumeID, VolumeLabel, ArchiveID, SourceLabel, JobID string
	ParentPath                                                 *string
	CreatedAfter, CreatedBefore                                string
	AfterPath                                                  string
	AfterID                                                    int64
	Limit                                                      int
}
type CatalogMember struct {
	Member
	ArchiveID      string `json:"archive_id"`
	VolumeID       string `json:"volume_id"`
	VolumeLabel    string `json:"volume_label"`
	DataFileIndex  int64  `json:"data_file_index"`
	Integrity      string `json:"integrity"`
	IntegrityBasis string `json:"integrity_basis"`
}

func pageLimit(limit int) int {
	if limit < 1 {
		return 100
	}
	if limit > 500 {
		return 500
	}
	return limit
}
func instantFilter(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	return catalogInstant(value)
}

// SearchMembers uses literal, case-sensitive substring matching and an opaque
// caller-encoded (path,id) cursor. Every request has a bounded result size.
func (s *Store) SearchMembers(ctx context.Context, q MemberQuery) ([]CatalogMember, error) {
	if len(q.Text) > 1024 || len(q.AfterPath) > 4096 || len(q.VolumeLabel) > 4096 || len(q.SourceLabel) > 4096 {
		return nil, ErrEvidence
	}
	var err error
	q.CreatedAfter, err = instantFilter(q.CreatedAfter)
	if err != nil {
		return nil, err
	}
	q.CreatedBefore, err = instantFilter(q.CreatedBefore)
	if err != nil {
		return nil, err
	}

	clauses := []string{"(m.path>? OR (m.path=? AND m.id>?))"}
	args := []any{q.AfterPath, q.AfterPath, q.AfterID}
	for _, f := range []struct {
		condition string
		value     string
	}{{"instr(m.path,?)>0", q.Text}, {"a.volume_id=?", q.VolumeID}, {"instr(v.label,?)>0", q.VolumeLabel}, {"a.id=?", q.ArchiveID}, {"instr(a.source_label,?)>0", q.SourceLabel}, {"a.job_id=?", q.JobID}, {"a.created_at>=?", q.CreatedAfter}, {"a.created_at<=?", q.CreatedBefore}} {
		if f.value != "" {
			clauses = append(clauses, f.condition)
			args = append(args, f.value)
		}
	}
	if q.ParentPath != nil {
		if len(*q.ParentPath) > 4096 {
			return nil, ErrEvidence
		}
		clauses = append(clauses, "m.parent_path=?")
		args = append(args, *q.ParentPath)
	}
	args = append(args, pageLimit(q.Limit))
	rows, err := s.db.QueryContext(ctx, "SELECT m.id,m.path,m.raw_path,m.parent_path,m.type,m.size,m.mtime,m.mode,m.link_target,a.id,a.volume_id,v.label,a.data_file_index,a.integrity_state,a.integrity_basis FROM members m JOIN archives a ON a.id=m.archive_id JOIN volumes v ON v.id=a.volume_id WHERE "+strings.Join(clauses, " AND ")+" ORDER BY m.path COLLATE BINARY,m.id LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CatalogMember{}
	for rows.Next() {
		var m CatalogMember
		if err = rows.Scan(&m.ID, &m.Path, &m.RawPath, &m.ParentPath, &m.Type, &m.Size, &m.Mtime, &m.Mode, &m.LinkTarget, &m.ArchiveID, &m.VolumeID, &m.VolumeLabel, &m.DataFileIndex, &m.Integrity, &m.IntegrityBasis); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

type ArchiveQuery struct {
	VolumeID, AfterCreatedAt, AfterID string
	Limit                             int
}

const archiveColumns = "id,volume_id,COALESCE(job_id,''),sequence,data_file_index,commit_file_index,source_label,created_at,stream_bytes,sha256,COALESCE(commit_sha256,''),integrity_state,integrity_basis,COALESCE(verified_at,''),restore_eligible"

func scanArchive(row scanner) (a Archive, err error) {
	err = row.Scan(&a.ID, &a.VolumeID, &a.JobID, &a.Sequence, &a.DataIndex, &a.CommitIndex, &a.SourceLabel, &a.CreatedAt, &a.StreamBytes, &a.SHA256, &a.CommitSHA256, &a.Integrity, &a.IntegrityBasis, &a.VerifiedAt, &a.RestoreEligible)
	return
}
func (s *Store) ListArchives(ctx context.Context, q ArchiveQuery) ([]Archive, error) {
	normalized, err := instantFilter(q.AfterCreatedAt)
	if err != nil {
		return nil, err
	}
	q.AfterCreatedAt = normalized
	clause := "(created_at>? OR (created_at=? AND id>?))"
	args := []any{q.AfterCreatedAt, q.AfterCreatedAt, q.AfterID}
	if q.VolumeID != "" {
		clause += " AND volume_id=?"
		args = append(args, q.VolumeID)
	}
	args = append(args, pageLimit(q.Limit))
	rows, err := s.db.QueryContext(ctx, "SELECT "+archiveColumns+" FROM archives WHERE "+clause+" ORDER BY created_at,id LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Archive{}
	for rows.Next() {
		a, err := scanArchive(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// readSnapshot opens a query-only WAL reader so a slow export cannot monopolize
// the single writer connection. At most four snapshots are live per Store.
func (s *Store) readSnapshot(ctx context.Context, fn func(*sql.Tx) error) error {
	select {
	case s.readSlots <- struct{}{}:
		defer func() { <-s.readSlots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	db, err := sql.Open("sqlite", s.readDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

var ErrExportExpired = errors.New("EXPORT_EXPIRED")
