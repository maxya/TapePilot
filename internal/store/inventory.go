package store

import (
	"context"
	"database/sql"
	"github.com/maxya/TapePilot/internal/domain"
	"strings"
	"unicode/utf8"
)

// ValidBarcode treats a manual barcode as optional display metadata. Identity
// and write authorization always use the observed managed UUID and header.
func ValidBarcode(value string) bool {
	return len(value) <= 128 && utf8.ValidString(value) && !strings.ContainsAny(value, "\x00\r\n")
}
func (s *Store) EditInventoryMetadata(ctx context.Context, id, oldLabel, oldLocation, oldBarcode, label, location, barcode string) error {
	if !plainLabel(label) || !utf8.ValidString(location) || len(location) > 256 || strings.ContainsAny(location, "\x00\r\n") || !ValidBarcode(barcode) {
		return ErrOperations
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, "UPDATE volumes SET label=?,location=?,barcode=? WHERE id=? AND label=? AND location=? AND barcode=?", label, location, barcode, id, oldLabel, oldLocation, oldBarcode)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrConflict
		}
		return insertAudit(ctx, tx, "admin", "VOLUME_UPDATED", id)
	})
}

func (s *Store) EditInventory(ctx context.Context, id, oldLabel, oldLocation, label, location string) error {
	if !plainLabel(label) || !utf8.ValidString(location) || len(location) > 256 || strings.ContainsAny(location, "\x00\r\n") {
		return ErrOperations
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		res, e := tx.ExecContext(ctx, "UPDATE volumes SET label=?,location=? WHERE id=? AND label=? AND location=?", label, location, id, oldLabel, oldLocation)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrConflict
		}
		return insertAudit(ctx, tx, "admin", "VOLUME_UPDATED", id)
	})
}
func (s *Store) FilterJobs(ctx context.Context, state domain.State, limit int) ([]Job, error) {
	if state == "" {
		return s.ListJobs(ctx, limit)
	}
	switch state {
	case domain.Queued, domain.Preflight, domain.WaitingMedia, domain.Running, domain.Verifying, domain.Succeeded, domain.Failed, domain.CancelRequested, domain.Cancelled, domain.RecoveryRequired:
	default:
		return nil, ErrOperations
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+jobColumns+" FROM jobs WHERE state=? ORDER BY created_at DESC,id DESC LIMIT ?", state, pageLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Job{}
	for rows.Next() {
		j, e := scanJob(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, j)
	}
	return out, rows.Err()
}
