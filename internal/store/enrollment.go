package store

import (
	"context"
	"database/sql"
	"errors"
	"github.com/maxya/TapePilot/internal/domain"
)

var ErrNotEnrolled = errors.New("WRITE_NOT_ARMED")

type Enrollment struct {
	VolumeID, DriveID, Profile, OperatorRef string
}

// EnrollVolume is a local operator action. HTTP handlers must not expose it.
func (s *Store) EnrollVolume(ctx context.Context, e Enrollment) error {
	if e.DriveID == "" || e.Profile == "" || e.OperatorRef == "" || len(e.OperatorRef) > 256 {
		return ErrNotEnrolled
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		var state domain.MediaState
		var kind string
		if err := tx.QueryRowContext(ctx, "SELECT state,kind FROM volumes WHERE id=?", e.VolumeID).Scan(&state, &kind); err != nil {
			return err
		}
		if state != domain.Managed || kind != "MANAGED" {
			return ErrNotEnrolled
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO volume_enrollments(volume_id,drive_id,profile,enabled,enrolled_at,operator_ref) VALUES(?,?,?,1,?,?) ON CONFLICT(volume_id) DO UPDATE SET drive_id=excluded.drive_id,profile=excluded.profile,enabled=1,enrolled_at=excluded.enrolled_at,revoked_at=NULL,operator_ref=excluded.operator_ref", e.VolumeID, e.DriveID, e.Profile, stamp(), e.OperatorRef); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO enrollment_events(volume_id,drive_id,profile,action,operator_ref,created_at) VALUES(?,?,?,'ENROLL',?,?)", e.VolumeID, e.DriveID, e.Profile, e.OperatorRef, stamp())
		return err
	})
}
func (s *Store) RevokeEnrollment(ctx context.Context, volume, operatorRef string) error {
	if operatorRef == "" || len(operatorRef) > 256 {
		return ErrNotEnrolled
	}
	return s.transaction(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "UPDATE volume_enrollments SET enabled=0,revoked_at=?,operator_ref=? WHERE volume_id=?", stamp(), operatorRef, volume)
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
		_, err = tx.ExecContext(ctx, "INSERT INTO enrollment_events(volume_id,drive_id,profile,action,operator_ref,created_at) SELECT volume_id,drive_id,profile,'REVOKE',?,? FROM volume_enrollments WHERE volume_id=?", operatorRef, stamp(), volume)
		return err
	})
}
func (s *Store) CheckEnrollment(ctx context.Context, volume, drive, profile string) error {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM volume_enrollments e JOIN volumes v ON v.id=e.volume_id WHERE e.volume_id=? AND e.drive_id=? AND e.profile=? AND e.enabled=1 AND e.revoked_at IS NULL AND v.kind='MANAGED' AND v.state='MANAGED'", volume, drive, profile).Scan(&n)
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrNotEnrolled
	}
	return nil
}
