package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/jobs"
	"github.com/maxya/TapePilot/internal/media"
	"github.com/maxya/TapePilot/internal/source"
	"github.com/maxya/TapePilot/internal/store"
	"github.com/maxya/TapePilot/internal/tape"
	"github.com/maxya/TapePilot/internal/tape/sim"
	"github.com/maxya/TapePilot/internal/volume"
)

var ctx = context.Background()

type fixture struct {
	engine                                 *Engine
	store                                  *store.Store
	drive                                  *sim.Drive
	root, sourceRoot, volumeID, generation string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root := t.TempDir()
	s, err := store.Open(ctx, filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	d, err := sim.Open(filepath.Join(root, "tape"))
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Create("cartridge", 64*tape.RecordBytes, false); err != nil {
		t.Fatal(err)
	}
	if err = d.Insert("cartridge"); err != nil {
		t.Fatal(err)
	}
	l, err := d.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status, err := l.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	if err = s.CreateVolume(ctx, store.Volume{ID: id, Kind: "MANAGED", Label: "Synthetic", Generation: "SIMULATED", State: domain.BlankCandidate}); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(root, "source")
	if err = os.MkdirAll(filepath.Join(src, "data"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"normal.txt", "line\nbreak.txt", "-leading.txt", "猫.txt"} {
		if err = os.WriteFile(filepath.Join(src, "data", name), []byte("synthetic:"+name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	spool := filepath.Join(root, "spool")
	if err = os.Mkdir(spool, 0700); err != nil {
		t.Fatal(err)
	}
	return &fixture{engine: &Engine{Store: s, Drive: d, Sources: []source.Root{{ID: "source", Path: src}}, Spool: spool}, store: s, drive: d, root: root, sourceRoot: src, volumeID: id, generation: status.Generation}
}
func (f *fixture) job(t *testing.T, kind domain.Kind, opts any) store.Job {
	t.Helper()
	raw, err := json.Marshal(opts)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := f.store.Enqueue(ctx, store.Job{Kind: kind, VolumeID: f.volumeID, Options: raw})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := f.store.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != queued.ID {
		t.Fatal("wrong claimed job")
	}
	return claimed
}
func (f *fixture) initialize(t *testing.T) {
	t.Helper()
	j := f.job(t, domain.Initialize, InitOptions{CartridgeID: "cartridge", Generation: f.generation, Confirmed: true})
	f.engine.InitializeArmed = true
	f.engine.InitializeJobID = j.ID
	if err := f.engine.Execute(ctx, j); err != nil {
		t.Fatal(err)
	}
	f.engine.InitializeArmed = false
	f.engine.InitializeJobID = ""
	got, err := f.store.GetJob(ctx, j.ID)
	if err != nil || got.State != domain.Succeeded {
		t.Fatal(got, err)
	}
	l, err := f.drive.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	obs, err := l.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	if err = f.store.EnrollVolume(ctx, store.Enrollment{VolumeID: f.volumeID, DriveID: obs.Drive, Profile: obs.Profile, OperatorRef: "test operator"}); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) backup(t *testing.T) store.Job {
	t.Helper()
	return f.job(t, domain.Backup, BackupOptions{SourceID: "source", Selection: "data", SourceLabel: "Synthetic"})
}
func (f *fixture) finishFailure(t *testing.T, j store.Job) store.Job {
	t.Helper()
	current, err := f.store.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The production scheduler performs the same conservative reconciliation
	// after executor failure; this test also exercises durable restart behavior.
	if err = f.store.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	current, err = f.store.GetJob(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	return current
}
func TestInitializationTwoAppendsAndIndependentRecovery(t *testing.T) {
	f := newFixture(t)
	f.initialize(t)
	first, err := os.ReadFile(filepath.Join(f.root, "tape", "cartridge", "000000.tar"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		j := f.backup(t)
		if err = f.engine.Execute(ctx, j); err != nil {
			t.Fatal(err)
		}
		got, err := f.store.GetJob(ctx, j.ID)
		if err != nil || got.State != domain.Succeeded {
			t.Fatal(got, err)
		}
	}
	after, err := os.ReadFile(filepath.Join(f.root, "tape", "cartridge", "000000.tar"))
	if err != nil || !bytes.Equal(first, after) {
		t.Fatal("header changed", err)
	}
	l, err := f.drive.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pairs := 0
	scanned, err := volume.Scan(ctx, l, volume.Visitor{Pair: func(pair volume.Pair) error {
		pairs++
		archive, err := f.store.GetArchive(ctx, pair.Commit.ArchiveID)
		if err != nil {
			return err
		}
		members, err := f.store.ListMembers(ctx, archive.ID, 0, 500)
		if err != nil {
			return err
		}
		if len(members) != 5 {
			return errors.New("catalog did not come from actual members")
		}
		return nil
	}})
	if err != nil || pairs != 2 || scanned.Proof == nil {
		t.Fatal(scanned, err)
	}
	l.Close()
	// Ordinary tar must recover both payload files with the catalog closed.
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{"000001.tar", "000003.tar"} {
		archive := filepath.Join(f.root, "tape", "cartridge", index)
		out := filepath.Join(f.root, "recover-"+index)
		if err = os.Mkdir(out, 0700); err != nil {
			t.Fatal(err)
		}
		command := exec.Command("/usr/bin/tar", "--extract", "--file="+archive, "--directory="+out, "--no-same-owner", "--no-same-permissions")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatal(err, string(output))
		}
		for _, name := range []string{"normal.txt", "line\nbreak.txt", "-leading.txt", "猫.txt"} {
			got, err := os.ReadFile(filepath.Join(out, "data", name))
			if err != nil || string(got) != "synthetic:"+name {
				t.Fatal("manual restore mismatch", err)
			}
		}
	}
}
func TestInitializationNeedsJobAndGenerationBoundLocalAuthority(t *testing.T) {
	for _, caseName := range []string{"unarmed", "other-job", "wrong-generation", "wrong-cartridge", "unconfirmed"} {
		t.Run(caseName, func(t *testing.T) {
			f := newFixture(t)
			opts := InitOptions{CartridgeID: "cartridge", Generation: f.generation, Confirmed: true}
			if caseName == "wrong-generation" {
				opts.Generation = "stale"
			}
			if caseName == "wrong-cartridge" {
				opts.CartridgeID = "other"
			}
			if caseName == "unconfirmed" {
				opts.Confirmed = false
			}
			j := f.job(t, domain.Initialize, opts)
			f.engine.InitializeArmed = caseName != "unarmed"
			f.engine.InitializeJobID = j.ID
			if caseName == "other-job" {
				f.engine.InitializeJobID = uuid.NewString()
			}
			if err := f.engine.Execute(ctx, j); err == nil {
				t.Fatal("unauthorized initialization")
			}
			entries, err := os.ReadDir(filepath.Join(f.root, "tape", "cartridge"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatal("unauthorized write opened")
			}
		})
	}
}
func TestBackupFaultMilestonesNeverPublish(t *testing.T) {
	for _, phase := range []string{"before_intent", "after_intent", "before_data_open", "after_data_open", "after_data_close", "before_commit", "before_commit_open", "after_commit_open", "after_commit_close", "before_readback", "before_publish"} {
		t.Run(phase, func(t *testing.T) {
			f := newFixture(t)
			f.initialize(t)
			j := f.backup(t)
			triggered := false
			f.engine.testHook = func(name string) error {
				if name == phase {
					triggered = true
					return errors.New("injected milestone")
				}
				return nil
			}
			if err := f.engine.Execute(ctx, j); err == nil {
				t.Fatal("fault ignored")
			}
			if !triggered {
				t.Fatal("milestone not reached")
			}
			finished := f.finishFailure(t, j)
			expected := domain.RecoveryRequired
			if phase == "before_intent" || phase == "after_intent" {
				expected = domain.Failed
			}
			if finished.State != expected {
				t.Fatalf("got %s want %s", finished.State, expected)
			}
			if expected == domain.RecoveryRequired {
				v, err := f.store.GetVolume(ctx, f.volumeID)
				if err != nil || v.State != domain.Quarantined {
					t.Fatal(v, err)
				}
			}
		})
	}
}
func TestBackupReadbackCorruptionNeverSucceeds(t *testing.T) {
	f := newFixture(t)
	f.initialize(t)
	j := f.backup(t)
	f.engine.testHook = func(phase string) error {
		if phase != "before_readback" {
			return nil
		}
		name := filepath.Join(f.root, "tape", "cartridge", "000001.tar")
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		data[512] ^= 1
		return os.WriteFile(name, data, 0600)
	}
	if err := f.engine.Execute(ctx, j); err == nil {
		t.Fatal("corrupt tape verified")
	}
	if got := f.finishFailure(t, j); got.State != domain.RecoveryRequired {
		t.Fatal(got)
	}
}
func TestBackupRecordAndCloseFaults(t *testing.T) {
	for _, fault := range []sim.Faults{{FailWriteRecord: 1}, {ShortWriteRecord: 1}, {FailClose: true}} {
		t.Run(strings.ReplaceAll(strings.TrimSpace(string(mustJSON(t, fault))), "/", "_"), func(t *testing.T) {
			f := newFixture(t)
			f.initialize(t)
			if err := f.drive.SetFaults(fault); err != nil {
				t.Fatal(err)
			}
			j := f.backup(t)
			if err := f.engine.Execute(ctx, j); err == nil {
				t.Fatal("fault ignored")
			}
			if got := f.finishFailure(t, j); got.State != domain.RecoveryRequired {
				t.Fatal(got)
			}
		})
	}
}
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestOptionsAreStrict(t *testing.T) {
	for _, raw := range []string{`{"source_id":"x","source_id":"y"}`, `{"root":"/etc"}`, `{} {}`, `null`} {
		var options BackupOptions
		if decodeOptions([]byte(raw), &options) == nil {
			t.Fatal("unsafe options", raw)
		}
	}
}
func TestNoEnrollmentOrSealedVolumeNeverOpensWriter(t *testing.T) {
	for _, sealed := range []bool{false, true} {
		f := newFixture(t)
		f.initialize(t)
		if sealed {
			if err := f.store.SetVolumeState(ctx, f.volumeID, domain.Sealed); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := f.store.RevokeEnrollment(ctx, f.volumeID, "test operator"); err != nil {
				t.Fatal(err)
			}
		}
		j := f.backup(t)
		if err := f.engine.Execute(ctx, j); err == nil {
			t.Fatal("unarmed backup")
		}
		entries, err := os.ReadDir(filepath.Join(f.root, "tape", "cartridge"))
		if err != nil || len(entries) != 2 {
			t.Fatal("unauthorized data open", err)
		}
	}
}
func TestExpectedDigestIsRawArchiveIncludingPadding(t *testing.T) {
	f := newFixture(t)
	f.initialize(t)
	j := f.backup(t)
	if err := f.engine.Execute(ctx, j); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(f.root, "tape", "cartridge", "000001.tar"))
	if err != nil {
		t.Fatal(err)
	}
	commitRaw, err := os.ReadFile(filepath.Join(f.root, "tape", "cartridge", "000002.tar"))
	if err != nil {
		t.Fatal(err)
	}
	commit, _, err := media.ReadCommit(bytes.NewReader(commitRaw))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if commit.DataSHA256 != hex.EncodeToString(sum[:]) || commit.DataStreamBytes != int64(len(raw)) {
		t.Fatal("wrong raw archive digest")
	}
	reader := tar.NewReader(bytes.NewReader(raw))
	count := 0
	for {
		_, err = reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != 5 {
		t.Fatal(count)
	}
}
func TestCommitCloseAndEndOfMediaNeverPublish(t *testing.T) {
	commitIndex := uint64(2)
	for _, fault := range []sim.Faults{{FailCloseAtIndex: &commitIndex}, {EndOfMediaRecord: 1}} {
		f := newFixture(t)
		f.initialize(t)
		if err := f.drive.SetFaults(fault); err != nil {
			t.Fatal(err)
		}
		j := f.backup(t)
		if err := f.engine.Execute(ctx, j); err == nil {
			t.Fatal("uncertain tape operation passed")
		}
		if got := f.finishFailure(t, j); got.State != domain.RecoveryRequired {
			t.Fatal(got)
		}
	}
}
func TestEnrollmentRevocationBetweenDataAndCommit(t *testing.T) {
	f := newFixture(t)
	f.initialize(t)
	j := f.backup(t)
	f.engine.testHook = func(phase string) error {
		if phase == "before_commit" {
			return f.store.RevokeEnrollment(ctx, f.volumeID, "operator")
		}
		return nil
	}
	if err := f.engine.Execute(ctx, j); err == nil {
		t.Fatal("revoked commit opened")
	}
	entries, err := os.ReadDir(filepath.Join(f.root, "tape", "cartridge"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "000002.tar" {
			t.Fatal("commit written after revocation")
		}
	}
	if got := f.finishFailure(t, j); got.State != domain.RecoveryRequired {
		t.Fatal(got)
	}
}
func TestCancellationAtPublicationIsAtomic(t *testing.T) {
	f := newFixture(t)
	f.initialize(t)
	j := f.backup(t)
	f.engine.testHook = func(phase string) error {
		if phase == "before_publish" {
			return f.store.RequestCancel(ctx, j.ID)
		}
		return nil
	}
	if err := f.engine.Execute(ctx, j); err == nil {
		t.Fatal("publication raced past cancellation")
	}
	l, err := f.drive.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	scanned, err := volume.Scan(ctx, l, volume.Visitor{Pair: func(p volume.Pair) error {
		if _, err := f.store.GetArchive(ctx, p.Commit.ArchiveID); err == nil {
			return errors.New("cancelled job published archive")
		}
		return nil
	}})
	if err != nil || scanned.Pairs != 1 {
		t.Fatal(scanned, err)
	}
	l.Close()
	if err = f.store.FinishExecution(ctx, j.ID, context.Canceled); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.GetJob(ctx, j.ID)
	if err != nil || got.State != domain.RecoveryRequired {
		t.Fatal(got, err)
	}
}
func TestWholeSourceSelectionAndInvalidMetadataBeforeWriterOpen(t *testing.T) {
	f := newFixture(t)
	f.initialize(t)
	j := f.job(t, domain.Backup, BackupOptions{SourceID: "source"})
	if err := f.engine.Execute(ctx, j); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(f.root, "tape", "cartridge", "000002.tar"))
	if err != nil {
		t.Fatal(err)
	}
	commit, _, err := media.ReadCommit(bytes.NewReader(raw))
	if err != nil || commit.SourceSelection != "." {
		t.Fatal(commit, err)
	}
	j = f.job(t, domain.Backup, BackupOptions{SourceID: "source", SourceLabel: strings.Repeat("a", 4097)})
	if err = f.engine.Execute(ctx, j); err == nil {
		t.Fatal("invalid source metadata accepted")
	}
	if _, err = os.Stat(filepath.Join(f.root, "tape", "cartridge", "000003.tar")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("invalid metadata opened writer", err)
	}
}
func TestInitializationFaultMilestonesNeverRegister(t *testing.T) {
	for _, phase := range []string{"before_intent", "after_intent", "before_header_open", "after_header_open", "after_header_close", "before_readback", "before_publish"} {
		t.Run(phase, func(t *testing.T) {
			f := newFixture(t)
			j := f.job(t, domain.Initialize, InitOptions{CartridgeID: "cartridge", Generation: f.generation, Confirmed: true})
			f.engine.InitializeArmed = true
			f.engine.InitializeJobID = j.ID
			reached := false
			f.engine.testHook = func(name string) error {
				if name == phase {
					reached = true
					return errors.New("initialization milestone failure")
				}
				return nil
			}
			if err := f.engine.Execute(ctx, j); err == nil || !reached {
				t.Fatal("initialization fault not exercised", err)
			}
			finished := f.finishFailure(t, j)
			expected := domain.RecoveryRequired
			if phase == "before_intent" || phase == "after_intent" {
				expected = domain.Failed
			}
			if finished.State != expected {
				t.Fatalf("got %s want %s", finished.State, expected)
			}
			v, err := f.store.GetVolume(ctx, f.volumeID)
			if err != nil {
				t.Fatal(err)
			}
			if v.State == domain.Managed || v.HeaderSHA256 != "" {
				t.Fatal("failed init registered", v)
			}
			if expected == domain.RecoveryRequired && v.State != domain.Quarantined {
				t.Fatal("uncertain initialization did not quarantine")
			}
		})
	}
}
func TestPublicationSQLFailureLeavesCompletePairRecoverable(t *testing.T) {
	f := newFixture(t)
	f.initialize(t)
	first := f.backup(t)
	if err := f.engine.Execute(ctx, first); err != nil {
		t.Fatal(err)
	}
	j := f.backup(t)
	// The only second SQLite writer in this test installs a synthetic failure
	// while the application is synchronously paused before its transaction.
	db, err := sql.Open("sqlite", filepath.Join(f.root, "state", "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	f.engine.testHook = func(phase string) error {
		if phase == "before_publish" {
			_, err := db.ExecContext(ctx, `CREATE TRIGGER test_publication_failure BEFORE INSERT ON archives BEGIN SELECT RAISE(ABORT,'synthetic publication failure'); END`)
			return err
		}
		return nil
	}
	if err = f.engine.Execute(ctx, j); err == nil || !strings.Contains(err.Error(), "synthetic publication failure") {
		t.Fatal("real transaction failure not exercised", err)
	}
	if _, err = db.ExecContext(ctx, "DROP TRIGGER test_publication_failure"); err != nil {
		t.Fatal(err)
	}
	l, err := f.drive.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	scanned, err := volume.Scan(ctx, l, volume.Visitor{Pair: func(pair volume.Pair) error {
		checked++
		a, err := f.store.GetArchive(ctx, pair.Commit.ArchiveID)
		if pair.Commit.JobID == first.ID {
			if err != nil || a.SHA256 != pair.Data.SHA256 {
				return errors.New("earlier committed archive changed")
			}
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return errors.New("failed publication leaked archive row")
		}
		var count int
		if err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM verifications WHERE job_id=?", j.ID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return errors.New("failed publication leaked verification")
		}
		return nil
	}})
	if err != nil || checked != 2 || scanned.Proof == nil {
		t.Fatal("complete physical pair not independently recoverable", scanned, err)
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	if got := f.finishFailure(t, j); got.State != domain.RecoveryRequired {
		t.Fatal(got)
	}
}
func TestPersistedProgressDistinguishesWriteFromReadback(t *testing.T) {
	f := newFixture(t)
	f.initialize(t)
	j := f.backup(t)
	if err := f.engine.Execute(ctx, j); err != nil {
		t.Fatal(err)
	}
	p, err := f.store.Progress(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.AcceptedBytes != 2*tape.RecordBytes || p.ReadbackBytes != 3*tape.RecordBytes || p.EstimatedPercent != nil {
		t.Fatalf("incorrect persisted transfer evidence: %#v", p)
	}
	j = f.backup(t)
	f.engine.testHook = func(phase string) error {
		if phase == "before_readback" {
			return errors.New("stop before verification")
		}
		return nil
	}
	if err = f.engine.Execute(ctx, j); err == nil {
		t.Fatal("fault ignored")
	}
	p, err = f.store.Progress(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.AcceptedBytes != 2*tape.RecordBytes || p.ReadbackBytes != 0 {
		t.Fatalf("write acceptance presented as readback: %#v", p)
	}
}
func TestWaitingMediaRecheckPreservesProgressWithoutResumingWrite(t *testing.T) {
	f := newFixture(t)
	f.initialize(t)
	l, err := f.drive.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Unload(ctx); err != nil {
		t.Fatal(err)
	}
	if err = l.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(BackupOptions{SourceID: "source", Selection: "data"})
	if err != nil {
		t.Fatal(err)
	}
	j, err := f.store.Enqueue(ctx, store.Job{Kind: domain.Backup, VolumeID: f.volumeID, Options: raw})
	if err != nil {
		t.Fatal(err)
	}
	scheduler := jobs.New(f.store, f.engine)
	if ran, err := scheduler.RunOne(ctx); err != nil || !ran {
		t.Fatal(ran, err)
	}
	waiting, err := f.store.GetJob(ctx, j.ID)
	if err != nil || waiting.State != domain.WaitingMedia {
		t.Fatal(waiting, err)
	}
	before, err := f.store.Progress(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.drive.Insert("cartridge"); err != nil {
		t.Fatal(err)
	}
	if err = scheduler.Recheck(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if ran, err := scheduler.RunOne(ctx); err != nil || !ran {
		t.Fatal(ran, err)
	}
	done, err := f.store.GetJob(ctx, j.ID)
	if err != nil || done.State != domain.Succeeded {
		t.Fatal(done, err)
	}
	after, err := f.store.Progress(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !before.StartedAt.Equal(after.StartedAt) || before.AcceptedBytes != 0 || after.AcceptedBytes != 2*tape.RecordBytes {
		t.Fatal("invalid waiting-media telemetry", before, after)
	}
}
