package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/google/uuid"
	"time"
)

type HTTPPlan struct {
	ID, Kind, ProfileID, VolumeID string
	Revision                      int64
	Options, Preview              json.RawMessage
	ExpiresAt                     string
}

func (s *Store) SaveHTTPPlan(ctx context.Context, p HTTPPlan) error {
	if _, err := uuid.Parse(p.ID); err != nil || len(p.Options) > 65536 || len(p.Preview) > 1<<20 {
		return ErrEvidence
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		now := stamp()
		if _, err := tx.ExecContext(ctx, "DELETE FROM http_plans WHERE expires_at<=?", now); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM http_plans").Scan(&count); err != nil {
			return err
		}
		if count >= 128 {
			return ErrConflict
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO http_plans VALUES(?,?,?,?,?,?,?,?,?)", p.ID, p.Kind, p.ProfileID, p.Revision, p.VolumeID, string(p.Options), string(p.Preview), now, p.ExpiresAt)
		return err
	})
}
func (s *Store) HTTPPlan(ctx context.Context, id string) (p HTTPPlan, err error) {
	var options, preview string
	err = s.db.QueryRowContext(ctx, "SELECT id,kind,profile_id,revision,volume_id,options_json,preview_json,expires_at FROM http_plans WHERE id=? AND expires_at>?", id, time.Now().UTC().Format(catalogInstantLayout)).Scan(&p.ID, &p.Kind, &p.ProfileID, &p.Revision, &p.VolumeID, &options, &preview, &p.ExpiresAt)
	p.Options = json.RawMessage(options)
	p.Preview = json.RawMessage(preview)
	return
}
