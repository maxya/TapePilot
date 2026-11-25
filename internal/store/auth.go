package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrSessionLimit = errors.New("SESSION_LIMIT")

type AdminCredential struct {
	Hash    string
	Version int64
}

func (s *Store) AdminCredential(ctx context.Context) (AdminCredential, error) {
	var a AdminCredential
	err := s.db.QueryRowContext(ctx, "SELECT password_hash,version FROM admins WHERE id=1").Scan(&a.Hash, &a.Version)
	return a, err
}

// SetAdminPassword is reserved for the local interactive administrator CLI.
// Reset revokes every session in the same transaction as credential rotation.
func (s *Store) SetAdminPassword(ctx context.Context, hash string, reset bool) error {
	if len(hash) < 32 || len(hash) > 512 {
		return ErrEvidence
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		now := stamp()
		if reset {
			result, err := tx.ExecContext(ctx, "UPDATE admins SET password_hash=?,version=version+1,changed_at=? WHERE id=1", hash, now)
			if err != nil {
				return err
			}
			n, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if n != 1 {
				return ErrNotFound
			}
		} else {
			if _, err := tx.ExecContext(ctx, "INSERT INTO admins VALUES(1,?,1,?,?)", hash, now, now); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM sessions"); err != nil {
			return err
		}
		action := "BOOTSTRAP"
		if reset {
			action = "PASSWORD_RESET"
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO auth_audit_events(ts,action,outcome,request_id) VALUES(?,?, 'SUCCEEDED','local-cli')", now, action)
		return err
	})
}

type Session struct {
	RequestID                                               string
	TokenHash                                               string
	AdminVersion                                            *int64
	CreatedAt, LastSeenAt, IdleExpiresAt, AbsoluteExpiresAt string
}

// CreateSession rotates an optional old session and refuses credential races.
func (s *Store) CreateSession(ctx context.Context, v Session, oldHash string) error {
	if !digest(v.TokenHash) {
		return ErrEvidence
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		now := stamp()
		if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE absolute_expires_at<=? OR idle_expires_at<=?", now, now); err != nil {
			return err
		}
		if v.AdminVersion != nil {
			var version int64
			if err := tx.QueryRowContext(ctx, "SELECT version FROM admins WHERE id=1").Scan(&version); err != nil {
				return err
			}
			if version != *v.AdminVersion {
				return ErrConflict
			}
		}
		var count int
		limit := 128
		condition := "admin_version IS NULL"
		if v.AdminVersion != nil {
			limit = 16
			condition = "admin_version IS NOT NULL"
		}
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE "+condition+" AND token_hash<>?", oldHash).Scan(&count); err != nil {
			return err
		}
		if count >= limit {
			return ErrSessionLimit
		}
		if oldHash != "" {
			if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash=?", oldHash); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO sessions VALUES(?,?,?,?,?,?)", v.TokenHash, v.AdminVersion, v.CreatedAt, v.LastSeenAt, v.IdleExpiresAt, v.AbsoluteExpiresAt); err != nil {
			return err
		}
		if v.AdminVersion != nil {
			if len(v.RequestID) > 64 {
				return ErrEvidence
			}
			_, err := tx.ExecContext(ctx, "INSERT INTO auth_audit_events(ts,action,outcome,request_id) VALUES(?,'LOGIN','SUCCEEDED',?)", now, v.RequestID)
			return err
		}
		return nil
	})
}
func (s *Store) Session(ctx context.Context, hash string, now time.Time, touch bool) (Session, error) {
	var result Session
	if !digest(hash) {
		return result, ErrNotFound
	}
	err := s.transaction(ctx, func(tx *sql.Tx) error {
		timestamp := now.UTC().Format(catalogInstantLayout)
		err := tx.QueryRowContext(ctx, "SELECT token_hash,admin_version,created_at,last_seen_at,idle_expires_at,absolute_expires_at FROM sessions WHERE token_hash=? AND idle_expires_at>? AND absolute_expires_at>? AND (admin_version IS NULL OR admin_version=(SELECT version FROM admins WHERE id=1))", hash, timestamp, timestamp).Scan(&result.TokenHash, &result.AdminVersion, &result.CreatedAt, &result.LastSeenAt, &result.IdleExpiresAt, &result.AbsoluteExpiresAt)
		if err != nil {
			return err
		}
		// Write a touch at most once per minute. Anonymous sessions retain their
		// initial short absolute lifetime regardless of polling.
		if touch && result.AdminVersion != nil {
			last, err := time.Parse(time.RFC3339Nano, result.LastSeenAt)
			if err != nil {
				return err
			}
			if now.Sub(last) >= time.Minute {
				idle := now.Add(30 * time.Minute).UTC().Format(catalogInstantLayout)
				if idle > result.AbsoluteExpiresAt {
					idle = result.AbsoluteExpiresAt
				}
				if _, err = tx.ExecContext(ctx, "UPDATE sessions SET last_seen_at=?,idle_expires_at=? WHERE token_hash=?", timestamp, idle, hash); err != nil {
					return err
				}
				result.LastSeenAt = timestamp
				result.IdleExpiresAt = idle
			}
		}
		return nil
	})
	return result, err
}
func (s *Store) RevokeSession(ctx context.Context, hash string) error {
	return s.transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash=?", hash)
		return err
	})
}
func (s *Store) AuthAudit(ctx context.Context, action, outcome, requestID string) error {
	switch action {
	case "LOGIN", "LOGOUT":
	default:
		return ErrEvidence
	}
	switch outcome {
	case "SUCCEEDED", "FAILED", "RATE_LIMITED":
	default:
		return ErrEvidence
	}
	if len(requestID) > 64 {
		return ErrEvidence
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO auth_audit_events(ts,action,outcome,request_id) VALUES(?,?,?,?)", stamp(), action, outcome, requestID)
		return err
	})
}

func (s *Store) LogoutSession(ctx context.Context, hash, requestID string) error {
	if len(requestID) > 64 {
		return ErrEvidence
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash=?", hash); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO auth_audit_events(ts,action,outcome,request_id) VALUES(?,'LOGOUT','SUCCEEDED',?)", stamp(), requestID)
		return err
	})
}
