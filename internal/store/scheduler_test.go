package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/domain"
	"os"
	"path/filepath"
	"testing"
)

func TestEnrollmentRevocationAndProtection(t *testing.T) {
	s, dir := openTest(t)
	v := volume(t, s)
	if err := s.CheckEnrollment(ctx, v, "sim", "profile"); !errors.Is(err, ErrNotEnrolled) {
		t.Fatal(err)
	}
	if err := s.EnrollVolume(ctx, Enrollment{VolumeID: v, DriveID: "sim", Profile: "profile", OperatorRef: "local-fixture-operator"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckEnrollment(ctx, v, "wrong-drive", "profile"); !errors.Is(err, ErrNotEnrolled) {
		t.Fatal(err)
	}
	if err := s.CheckEnrollment(ctx, v, "sim", "changed-profile"); !errors.Is(err, ErrNotEnrolled) {
		t.Fatal(err)
	}
	if err := s.CheckEnrollment(ctx, v, "sim", "profile"); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeEnrollment(ctx, v, "operator-revocation"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	restart, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer restart.Close()
	if err = restart.CheckEnrollment(ctx, v, "sim", "profile"); !errors.Is(err, ErrNotEnrolled) {
		t.Fatal(err)
	}
	if err = restart.EnrollVolume(ctx, Enrollment{VolumeID: v, DriveID: "sim", Profile: "profile", OperatorRef: "local"}); err != nil {
		t.Fatal(err)
	}
	if err = restart.SetVolumeState(ctx, v, domain.Sealed); err != nil {
		t.Fatal(err)
	}
	if err = restart.CheckEnrollment(ctx, v, "sim", "profile"); !errors.Is(err, ErrNotEnrolled) {
		t.Fatal(err)
	}
	legacy := uuid.NewString()
	if err = restart.CreateVolume(ctx, Volume{ID: legacy, Kind: "LEGACY", State: domain.LegacyReadOnly}); err != nil {
		t.Fatal(err)
	}
	if err = restart.EnrollVolume(ctx, Enrollment{VolumeID: legacy, DriveID: "sim", Profile: "profile", OperatorRef: "local"}); !errors.Is(err, ErrNotEnrolled) {
		t.Fatal(err)
	}
}
func TestUpgradeTakesConsistentPreMigrationSnapshot(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	path := filepath.Join(dir, "catalog.db")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	script, err := migrations.ReadFile("migrations/001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL,checksum TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(string(script)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO schema_migrations VALUES(1,?,?)", stamp(), fmt.Sprintf("%x", sha256.Sum256(script))); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	if _, err = db.Exec("INSERT INTO volumes VALUES(?,'MANAGED','old','','','BLANK_CANDIDATE',NULL,?)", id, stamp()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	upgraded, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if _, err = upgraded.GetVolume(ctx, id); err != nil {
		t.Fatal(err)
	}
	snapshots, err := filepath.Glob(filepath.Join(dir, "pre-migration-v1-*.db"))
	if err != nil || len(snapshots) != 1 {
		t.Fatal(snapshots, err)
	}
	snapshot, err := sql.Open("sqlite", snapshots[0]+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	var count int
	if err = snapshot.QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if err = snapshot.QueryRow("SELECT COUNT(*) FROM volumes WHERE id=?", id).Scan(&count); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if _, err = snapshot.Exec("SELECT cancel_requested FROM jobs"); err == nil {
		t.Fatal("snapshot includes post-upgrade schema")
	}
}
