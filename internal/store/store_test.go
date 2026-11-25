package store

import (
	"context"
	"database/sql"
	"errors"
	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/domain"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var ctx = context.Background()

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}
func volume(t *testing.T, s *Store) string {
	t.Helper()
	id := uuid.NewString()
	if err := s.CreateVolume(ctx, Volume{ID: id, Kind: "MANAGED", State: domain.BlankCandidate, Label: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	j := job(t, s, domain.Initialize, id)
	j, err := s.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	j = advance(t, s, j, domain.Running)
	if err = s.BeginWriteIntent(ctx, WriteIntent{JobID: j.ID, VolumeID: id, DriveID: "sim"}); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkWriteOpened(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("c", 64)
	if err = s.RecordExpectedHeader(ctx, j.ID, hash, 262144); err != nil {
		t.Fatal(err)
	}
	j = advance(t, s, j, domain.Verifying)
	if err = s.PublishInitializedVolume(ctx, j.ID, Verification{JobID: j.ID, Basis: "readback", ExpectedSHA256: hash, ActualSHA256: hash, ExpectedBytes: 262144, ActualBytes: 262144, IdentityValid: true, FormatValid: true}); err != nil {
		t.Fatal(err)
	}
	return id
}
func job(t *testing.T, s *Store, kind domain.Kind, v string) Job {
	t.Helper()
	j, err := s.Enqueue(ctx, Job{Kind: kind, VolumeID: v})
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func advance(t *testing.T, s *Store, j Job, to domain.State) Job {
	t.Helper()
	j, err := s.Transition(ctx, j.ID, j.Version, to, "test", "")
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func TestSingletonAndMigrationRestart(t *testing.T) {
	s, dir := openTest(t)
	id := volume(t, s)
	if _, err := Open(ctx, dir); !errors.Is(err, ErrOwned) {
		t.Fatalf("second owner: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err = s2.GetVolume(ctx, id); err != nil {
		t.Fatal(err)
	}
	var journal, sync, foreign string
	if err = s2.db.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
		t.Fatal(err)
	}
	s2.db.QueryRow("PRAGMA synchronous").Scan(&sync)
	s2.db.QueryRow("PRAGMA foreign_keys").Scan(&foreign)
	if journal != "wal" || sync != "2" || foreign != "1" {
		t.Fatal(journal, sync, foreign)
	}
}
func TestClaimCASAndImmutableOptions(t *testing.T) {
	s, _ := openTest(t)
	j := job(t, s, domain.Inspect, "")
	job(t, s, domain.Rewind, "")
	var wg sync.WaitGroup
	var mu sync.Mutex
	success := 0
	for range 12 {
		wg.Go(func() {
			_, err := s.Claim(ctx)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				success++
			} else if !errors.Is(err, ErrConflict) {
				t.Errorf("claim: %v", err)
			}
		})
	}
	wg.Wait()
	if success != 1 {
		t.Fatal(success)
	}
	current, err := s.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Transition(ctx, j.ID, 0, domain.Running, "", ""); !errors.Is(err, ErrConflict) {
		t.Fatal("stale CAS accepted", err)
	}
	current = advance(t, s, current, domain.Running)
	advance(t, s, current, domain.Succeeded)
	if _, err = s.db.Exec("UPDATE jobs SET options_json='{}' WHERE id=?", j.ID); err == nil {
		t.Fatal("options mutation accepted")
	}
	events, err := s.Events(ctx, j.ID, 0, 100)
	if err != nil || len(events) != 4 {
		t.Fatal(events, err)
	}
}
func TestDedupeAndConstraints(t *testing.T) {
	s, _ := openTest(t)
	j, err := s.Enqueue(ctx, Job{Kind: domain.Inspect, DedupeKey: "request-1"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Enqueue(ctx, Job{Kind: domain.Inspect, DedupeKey: "request-1"})
	if err != nil || again.ID != j.ID {
		t.Fatal(again, err)
	}
	if _, err = s.Enqueue(ctx, Job{Kind: domain.Rewind, DedupeKey: "request-1"}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if _, err = s.Enqueue(ctx, Job{Kind: domain.Backup, VolumeID: uuid.NewString()}); err == nil {
		t.Fatal("foreign key absent")
	}
	if err = s.StageMembers(ctx, uuid.NewString(), []Member{{Path: "a", Size: -1}}); err == nil {
		t.Fatal("negative size accepted")
	}
	if err = s.StageMembers(ctx, uuid.NewString(), []Member{{Path: "a"}, {Path: "a"}}); err == nil {
		t.Fatal("duplicates accepted")
	}
	var n int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM staged_members").Scan(&n); err != nil || n != 0 {
		t.Fatal("failed batch partially committed", n, err)
	}
}
func TestInterruptedWriteQuarantinesWithoutReplay(t *testing.T) {
	s, dir := openTest(t)
	v := volume(t, s)
	j := job(t, s, domain.Backup, v)
	j, err := s.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	j = advance(t, s, j, domain.Running)
	if err = s.BeginWriteIntent(ctx, WriteIntent{JobID: j.ID, VolumeID: v, DriveID: "sim", DataIndex: 1, CommitIndex: 2}); err != nil {
		t.Fatal(err)
	}
	if err = s.MarkWriteOpened(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Transition(ctx, j.ID, j.Version, domain.Failed, "", ""); !errors.Is(err, ErrEvidence) {
		t.Fatal("uncertain write marked failed", err)
	}
	queued := job(t, s, domain.Inspect, "")
	s.Close()
	restart, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer restart.Close()
	j, err = restart.GetJob(ctx, j.ID)
	if err != nil || j.State != domain.RecoveryRequired {
		t.Fatal(j, err)
	}
	loaded, err := restart.GetVolume(ctx, v)
	if err != nil || loaded.State != domain.Quarantined {
		t.Fatal(loaded, err)
	}
	q, err := restart.GetJob(ctx, queued.ID)
	if err != nil || q.State != domain.Queued {
		t.Fatal(q, err)
	}
}
func TestPublicationRequiresExactReadbackAndIsAtomic(t *testing.T) {
	s, _ := openTest(t)
	v := volume(t, s)
	j := job(t, s, domain.Backup, v)
	j, _ = s.Claim(ctx)
	j = advance(t, s, j, domain.Running)
	if _, err := s.Transition(ctx, j.ID, j.Version, domain.Succeeded, "", ""); !errors.Is(err, ErrEvidence) {
		t.Fatal(err)
	}
	if err := s.BeginWriteIntent(ctx, WriteIntent{JobID: j.ID, VolumeID: v, DriveID: "sim", DataIndex: 1, CommitIndex: 2}); err != nil {
		t.Fatal(err)
	}
	s.MarkWriteOpened(ctx, j.ID)
	sha, commit := strings.Repeat("a", 64), strings.Repeat("b", 64)
	if err := s.RecordExpectedWrite(ctx, j.ID, sha, 262144, commit); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	index := int64(2)
	a := Archive{ID: id, VolumeID: v, JobID: j.ID, Sequence: 1, DataIndex: 1, CommitIndex: &index, StreamBytes: 262144, SHA256: sha, CommitSHA256: commit, Integrity: domain.Verified, IntegrityBasis: "readback", RestoreEligible: true}
	evidence := Verification{ActualCommitSHA256: commit, JobID: j.ID, ArchiveID: id, Basis: "readback", ExpectedSHA256: sha, ActualSHA256: sha, ExpectedBytes: 262144, ActualBytes: 262144, IdentityValid: true, FormatValid: true}
	j = advance(t, s, j, domain.Verifying)
	if err := s.StageMembers(ctx, j.ID, []Member{{Path: "hello", RawPath: "hello", Type: "file", Size: 5}}); err != nil {
		t.Fatal(err)
	}
	bad := evidence
	bad.ActualBytes--
	if err := s.PublishArchive(ctx, j.ID, a, bad); err == nil {
		t.Fatal("bad readback accepted")
	}
	if _, err := s.GetArchive(ctx, id); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	if err := s.PublishArchive(ctx, j.ID, a, evidence); err != nil {
		t.Fatal(err)
	}
	members, err := s.ListMembers(ctx, id, 0, 100)
	if err != nil || len(members) != 1 {
		t.Fatal(members, err)
	}
	finished, err := s.GetJob(ctx, j.ID)
	if err != nil || finished.State != domain.Succeeded {
		t.Fatal(finished, err)
	}
}
func TestConsistentBackupIncludesWAL(t *testing.T) {
	s, _ := openTest(t)
	v := volume(t, s)
	exportDir := t.TempDir()
	os.Chmod(exportDir, 0700)
	dest := filepath.Join(exportDir, "snapshot.db")
	if err := s.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(ctx, dest); err == nil {
		t.Fatal("overwrote backup")
	}
	copyDir := t.TempDir()
	os.Chmod(copyDir, 0700)
	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(copyDir, "catalog.db"), b, 0600); err != nil {
		t.Fatal(err)
	}
	copy, err := Open(ctx, copyDir)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	if _, err = copy.GetVolume(ctx, v); err != nil {
		t.Fatal(err)
	}
}
func TestRejectFutureSchemaAndChangedMigration(t *testing.T) {
	for _, query := range []string{"INSERT INTO schema_migrations VALUES(99,'now','bad')", "UPDATE schema_migrations SET checksum='bad'"} {
		t.Run(query, func(t *testing.T) {
			s, dir := openTest(t)
			if _, err := s.db.Exec(query); err != nil {
				t.Fatal(err)
			}
			s.Close()
			if reopened, err := Open(ctx, dir); err == nil {
				reopened.Close()
				t.Fatal("unsafe schema accepted")
			}
		})
	}
}

func TestIntentBindingAndProtectiveState(t *testing.T) {
	s, _ := openTest(t)
	v := volume(t, s)
	other := volume(t, s)
	j := job(t, s, domain.Backup, v)
	j, _ = s.Claim(ctx)
	j = advance(t, s, j, domain.Running)
	for _, w := range []WriteIntent{{JobID: j.ID, VolumeID: other, DriveID: "sim", DataIndex: 1, CommitIndex: 2}, {JobID: j.ID, VolumeID: v, DriveID: "sim", DataIndex: 2, CommitIndex: 3}, {JobID: j.ID, VolumeID: v, DriveID: "sim", DataIndex: 3, CommitIndex: 4}} {
		if err := s.BeginWriteIntent(ctx, w); err == nil {
			t.Fatal("unsafe intent accepted", w)
		}
	}
	if err := s.SetVolumeState(ctx, v, domain.Sealed); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVolumeState(ctx, v, domain.Managed); err == nil {
		t.Fatal("unseal accepted")
	}
	if err := s.BeginWriteIntent(ctx, WriteIntent{JobID: j.ID, VolumeID: v, DriveID: "sim", DataIndex: 1, CommitIndex: 2}); err == nil {
		t.Fatal("sealed write intent accepted")
	}
}
func TestRecoveryTransitionQuarantinesAtomically(t *testing.T) {
	s, _ := openTest(t)
	v := volume(t, s)
	j := job(t, s, domain.Backup, v)
	j, _ = s.Claim(ctx)
	j = advance(t, s, j, domain.Running)
	if err := s.BeginWriteIntent(ctx, WriteIntent{JobID: j.ID, VolumeID: v, DriveID: "sim", DataIndex: 1, CommitIndex: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkWriteOpened(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkWriteOpened(ctx, j.ID); err == nil {
		t.Fatal("writer open replay allowed")
	}
	advance(t, s, j, domain.RecoveryRequired)
	loaded, err := s.GetVolume(ctx, v)
	if err != nil || loaded.State != domain.Quarantined {
		t.Fatal(loaded, err)
	}
}
func TestInitRejectsMismatchedHeaderReadback(t *testing.T) {
	s, _ := openTest(t)
	id := uuid.NewString()
	if err := s.CreateVolume(ctx, Volume{ID: id, Kind: "MANAGED", State: domain.BlankCandidate}); err != nil {
		t.Fatal(err)
	}
	j := job(t, s, domain.Initialize, id)
	j, _ = s.Claim(ctx)
	j = advance(t, s, j, domain.Running)
	if err := s.BeginWriteIntent(ctx, WriteIntent{JobID: j.ID, VolumeID: id, DriveID: "sim"}); err != nil {
		t.Fatal(err)
	}
	s.MarkWriteOpened(ctx, j.ID)
	hash := strings.Repeat("c", 64)
	if err := s.RecordExpectedHeader(ctx, j.ID, hash, 262144); err != nil {
		t.Fatal(err)
	}
	j = advance(t, s, j, domain.Verifying)
	bad := Verification{JobID: j.ID, Basis: "readback", ExpectedSHA256: strings.Repeat("d", 64), ActualSHA256: strings.Repeat("d", 64), ExpectedBytes: 262144, ActualBytes: 262144, IdentityValid: true, FormatValid: true}
	if err := s.PublishInitializedVolume(ctx, j.ID, bad); err == nil {
		t.Fatal("mismatched label accepted")
	}
	v, _ := s.GetVolume(ctx, id)
	if v.State != domain.BlankCandidate {
		t.Fatal(v.State)
	}
	if _, err := s.Transition(ctx, j.ID, j.Version, domain.Succeeded, "", ""); err == nil {
		t.Fatal("init succeeded without verified registration")
	}
}
func TestRejectSymlinkStateAndPublicDatabase(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	link := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(ctx, link); err == nil {
		s.Close()
		t.Fatal("symlink state accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "catalog.db"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(ctx, dir); err == nil {
		s.Close()
		t.Fatal("public catalog accepted")
	}
}

func TestProtectionChangeBeforeOpenRefusesIntent(t *testing.T) {
	s, _ := openTest(t)
	v := volume(t, s)
	j := job(t, s, domain.Backup, v)
	j, _ = s.Claim(ctx)
	j = advance(t, s, j, domain.Running)
	if err := s.BeginWriteIntent(ctx, WriteIntent{JobID: j.ID, VolumeID: v, DriveID: "sim", DataIndex: 1, CommitIndex: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVolumeState(ctx, v, domain.Sealed); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkWriteOpened(ctx, j.ID); err == nil {
		t.Fatal("sealed after intent but before open accepted")
	}
}
