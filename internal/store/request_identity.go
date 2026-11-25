package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/maxya/TapePilot/internal/domain"
	"strings"
)

// RequestIdentity describes the normalized operator request before it resolves
// to execution options. The actor and route scope keys independently.
type RequestIdentity struct {
	Actor, Route, Key string
	Request           json.RawMessage
}

func (i RequestIdentity) hash() (string, error) {
	if i.Actor != "admin" || len(i.Route) == 0 || len(i.Route) > 512 || strings.ContainsAny(i.Route, "\x00\r\n") || len(i.Key) == 0 || len(i.Key) > 128 {
		return "", ErrOperations
	}
	canonical, err := canonicalMetadata(i.Request)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
func (i RequestIdentity) dedupe() string {
	sum := sha256.Sum256([]byte(i.Actor + "\x00" + i.Route + "\x00" + i.Key))
	return "http-request:" + hex.EncodeToString(sum[:])
}
func replayRequestTx(ctx context.Context, tx *sql.Tx, i RequestIdentity, hash string) (Job, bool, error) {
	var previous, raw string
	err := tx.QueryRowContext(ctx, "SELECT request_sha256,result_json FROM http_request_results WHERE actor=? AND route=? AND request_key=?", i.Actor, i.Route, i.Key).Scan(&previous, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	if previous != hash {
		return Job{}, false, ErrConflict
	}
	var result Job
	if err = json.Unmarshal([]byte(raw), &result); err != nil {
		return Job{}, false, err
	}
	return result, true, nil
}
func saveRequestTx(ctx context.Context, tx *sql.Tx, i RequestIdentity, hash string, j Job) error {
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO http_request_results VALUES(?,?,?,?,?,?,?)", i.Actor, i.Route, i.Key, hash, j.ID, string(raw), stamp())
	return err
}

// Earlier development builds stored execution-derived keys without original
// plan IDs. Their ambiguous requests cannot be reconstructed safely. Refuse
// reuse of those keys instead of silently creating a second operation.
func legacyRequestConflictTx(ctx context.Context, tx *sql.Tx, i RequestIdentity, profileID string) error {
	keys := []string{"http:" + i.Key}
	if profileID != "" {
		sum := sha256.Sum256([]byte(i.Actor + "\x00" + i.Key))
		keys = append(keys, "manual-profile:"+profileID+":"+hex.EncodeToString(sum[:]))
	}
	for _, key := range keys {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM jobs WHERE dedupe_key=?", key).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return ErrConflict
		}
	}
	return nil
}

// EnqueueHTTPRequest creates the job, audit and immutable replay result in one
// transaction. A repeated key cannot silently adopt changed execution options.
func (s *Store) EnqueueHTTPRequest(ctx context.Context, i RequestIdentity, j Job) (Job, error) {
	hash, err := i.hash()
	if err != nil {
		return Job{}, err
	}
	var result Job
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		existing, found, err := replayRequestTx(ctx, tx, i, hash)
		if err != nil {
			return err
		}
		if found {
			result = existing
			return nil
		}
		if err = legacyRequestConflictTx(ctx, tx, i, ""); err != nil {
			return err
		}

		j.DedupeKey = i.dedupe()
		j, err = prepareJob(j)
		if err != nil {
			return err
		}
		result, err = enqueueJobTx(ctx, tx, j)
		if err != nil {
			return err
		}
		if err = insertAudit(ctx, tx, i.Actor, "JOB_REQUEST", string(j.Kind)); err != nil {
			return err
		}
		return saveRequestTx(ctx, tx, i, hash, result)
	})
	return result, err
}

// EnqueueHTTPPlan binds the actual plan ID and request revision to its result
// before plan lookup. An accepted request therefore replays after plan expiry,
// deletion or profile edits without starting another job.
func (s *Store) EnqueueHTTPPlan(ctx context.Context, i RequestIdentity, planID, profileID string, revision int64) (Job, error) {
	expected, err := json.Marshal(struct {
		PlanID   string `json:"plan_id"`
		Revision int64  `json:"revision"`
	}{planID, revision})
	if err != nil {
		return Job{}, err
	}
	original, err := canonicalMetadata(i.Request)
	if err != nil {
		return Job{}, err
	}
	normalized, err := canonicalMetadata(expected)
	if err != nil {
		return Job{}, err
	}
	if !bytes.Equal(original, normalized) {
		return Job{}, ErrOperations
	}
	hash, err := i.hash()
	if err != nil {
		return Job{}, err
	}
	var result Job
	err = s.transaction(ctx, func(tx *sql.Tx) error {
		existing, found, err := replayRequestTx(ctx, tx, i, hash)
		if err != nil {
			return err
		}
		if found {
			result = existing
			return nil
		}
		if err = legacyRequestConflictTx(ctx, tx, i, profileID); err != nil {
			return err
		}

		var p HTTPPlan
		var options string
		if err = tx.QueryRowContext(ctx, "SELECT id,kind,profile_id,revision,volume_id,options_json FROM http_plans WHERE id=? AND expires_at>?", planID, stamp()).Scan(&p.ID, &p.Kind, &p.ProfileID, &p.Revision, &p.VolumeID, &options); err != nil {
			return err
		}
		p.Options = json.RawMessage(options)
		if profileID != "" {
			if p.Kind != "BACKUP" || p.ProfileID != profileID || p.Revision != revision {
				return ErrConflict
			}
			var planDigest string
			if err = json.Unmarshal(p.Options, &planDigest); err != nil || !digest(planDigest) {
				return ErrEvidence
			}
			profile, err := scanProfile(tx.QueryRowContext(ctx, "SELECT "+profileColumns+" FROM backup_profiles WHERE id=?", profileID))
			if err != nil {
				return err
			}
			if profile.Revision != revision {
				return ErrConflict
			}
			result, _, err = enqueueProfileTx(ctx, tx, profile, i.dedupe(), planDigest, i.Actor)
			if err != nil {
				return err
			}
		} else {
			if p.Kind != "RESTORE" {
				return ErrConflict
			}
			j, err := prepareJob(Job{Kind: domain.Restore, VolumeID: p.VolumeID, Options: p.Options, DedupeKey: i.dedupe()})
			if err != nil {
				return err
			}
			result, err = enqueueJobTx(ctx, tx, j)
			if err != nil {
				return err
			}
			if err = insertAudit(ctx, tx, i.Actor, "JOB_REQUEST", string(j.Kind)); err != nil {
				return err
			}
		}
		return saveRequestTx(ctx, tx, i, hash, result)
	})
	return result, err
}
