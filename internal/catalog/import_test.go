package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/legacy"
	"github.com/maxya/TapePilot/internal/media"
	"github.com/maxya/TapePilot/internal/store"
	"github.com/maxya/TapePilot/internal/tape"
	"github.com/maxya/TapePilot/internal/tape/sim"
	"github.com/maxya/TapePilot/internal/volume"
)

var ctx = context.Background()

func openStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	s, e := store.Open(ctx, dir)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func importJob(t *testing.T, s *store.Store, opts ImportOptions) store.Job {
	t.Helper()
	raw, e := json.Marshal(opts)
	if e != nil {
		t.Fatal(e)
	}
	_, e = s.Enqueue(ctx, store.Job{Kind: domain.Import, Options: raw})
	if e != nil {
		t.Fatal(e)
	}
	j, e := s.Claim(ctx)
	if e != nil {
		t.Fatal(e)
	}
	return j
}
func hash(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func rawArchive(t *testing.T, payload string, legacy bool) []byte {
	t.Helper()
	root := t.TempDir()
	if e := os.WriteFile(filepath.Join(root, "file"), []byte(payload), 0600); e != nil {
		t.Fatal(e)
	}
	args := []string{"--create", "--format=pax", "--file=-", "--directory=" + root}
	if legacy {
		args = append(args, ".")
	} else {
		args = append(args, "--blocking-factor=512", "--", "file")
	}
	command := exec.Command("/usr/bin/tar", args...)
	command.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin"}
	raw, e := command.Output()
	if e != nil {
		t.Fatal(e)
	}
	return raw
}
func write(t *testing.T, l *sim.Lease, p tape.WritePermit, b []byte) {
	t.Helper()
	w, e := l.OpenWriter(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	for offset := 0; offset < len(b); offset += tape.RecordBytes {
		if e = w.WriteRecord(ctx, b[offset:offset+tape.RecordBytes]); e != nil {
			t.Fatal(e)
		}
	}
	if e = w.CloseAndFlush(); e != nil {
		t.Fatal(e)
	}
}
func managedFixture(t *testing.T) (*sim.Drive, string, media.Volume, []media.Commit) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "drive")
	drive, e := sim.Open(root)
	if e != nil {
		t.Fatal(e)
	}
	if e = drive.Create("lab", 20*tape.RecordBytes, false); e != nil {
		t.Fatal(e)
	}
	if e = drive.Insert("lab"); e != nil {
		t.Fatal(e)
	}
	lease, e := drive.Acquire(ctx)
	if e != nil {
		t.Fatal(e)
	}
	defer lease.Close()
	var label media.Volume
	raw, e := os.ReadFile("../../schemas/fixtures/volume.json")
	if e != nil {
		t.Fatal(e)
	}
	json.Unmarshal(raw, &label)
	header, e := media.EncodeVolume(label)
	if e != nil {
		t.Fatal(e)
	}
	permit, e := lease.AuthorizeInitialization(ctx, "test-init", true)
	if e != nil {
		t.Fatal(e)
	}
	write(t, lease, permit, header)
	data := rawArchive(t, "rebuild payload", false)
	var template media.Commit
	raw, e = os.ReadFile("../../schemas/fixtures/commit.json")
	if e != nil {
		t.Fatal(e)
	}
	json.Unmarshal(raw, &template)
	var commits []media.Commit
	for sequence := int64(1); sequence <= 2; sequence++ {
		scanned, e := volume.Scan(ctx, lease, volume.Visitor{})
		if e != nil {
			t.Fatal(e)
		}
		if e = lease.PositionAppend(ctx, *scanned.Proof); e != nil {
			t.Fatal(e)
		}
		intent := fmt.Sprintf("test-backup-%d", sequence)
		permit, e = lease.AuthorizeAppend(ctx, intent)
		if e != nil {
			t.Fatal(e)
		}
		write(t, lease, permit, data)
		commit := template
		commit.Sequence = sequence
		commit.DataFileIndex = 2*sequence - 1
		commit.CommitFileIndex = 2 * sequence
		commit.VolumeID = label.VolumeID
		commit.VolumeHeaderSHA256 = hash(header)
		commit.DataSHA256 = hash(data)
		commit.DataStreamBytes = int64(len(data))
		commit.DataRecordCount = int64(len(data)) / tape.RecordBytes
		if sequence > 1 {
			commit.ArchiveID = "6fa69f68-9d52-4542-9614-b19f1074ef50"
			commit.JobID = "2f2d805c-e530-4f68-ab29-5cb4c8f63320"
			previous := scanned.LastCommit.SHA256
			commit.PreviousCommitSHA256 = &previous
		}
		raw, e = media.EncodeCommit(commit)
		if e != nil {
			t.Fatal(e)
		}
		permit, e = lease.AuthorizeCommit(ctx, intent)
		if e != nil {
			t.Fatal(e)
		}
		write(t, lease, permit, raw)
		commits = append(commits, commit)
	}
	return drive, root, label, commits
}
func TestManagedRebuildAndIdempotentPolicyPreservation(t *testing.T) {
	drive, root, label, commits := managedFixture(t)
	s := openStore(t)
	importer := Importer{Store: s, Drive: drive}
	before, e := os.ReadFile(filepath.Join(root, "lab", "000001.tar"))
	if e != nil {
		t.Fatal(e)
	}
	j := importJob(t, s, ImportOptions{Mode: "managed", ExpectedVolumeID: label.VolumeID})
	if e = importer.Execute(ctx, j); e != nil {
		t.Fatal(e)
	}
	status, e := s.GetJob(ctx, j.ID)
	if e != nil || status.State != domain.Succeeded {
		t.Fatal(status, e)
	}
	if count, e := s.ArchiveCount(ctx, label.VolumeID); e != nil || count != 2 {
		t.Fatal(count, e)
	}
	for _, c := range commits {
		a, e := s.GetArchive(ctx, c.ArchiveID)
		if e != nil || a.SHA256 != c.DataSHA256 || a.Integrity != domain.Verified || a.IntegrityBasis != "ON_MEDIA_MANIFEST" || a.RestoreEligible {
			t.Fatal(a, e)
		}
		members, e := s.ListMembers(ctx, a.ID, 0, 500)
		if e != nil || len(members) != 1 || members[0].Path != "file" {
			t.Fatal(members, e)
		}
	}
	if !errors.Is(s.CheckEnrollment(ctx, label.VolumeID, "drive", "simulator"), store.ErrNotEnrolled) {
		t.Fatal("rebuild enrolled volume")
	}
	if e = s.EnrollVolume(ctx, store.Enrollment{VolumeID: label.VolumeID, DriveID: "drive", Profile: "simulator", OperatorRef: "test operator"}); e != nil {
		t.Fatal(e)
	}
	if e = s.RevokeEnrollment(ctx, label.VolumeID, "test revoke"); e != nil {
		t.Fatal(e)
	}
	if e = s.SetVolumeState(ctx, label.VolumeID, domain.Sealed); e != nil {
		t.Fatal(e)
	}
	j = importJob(t, s, ImportOptions{Mode: "managed"})
	if e = importer.Execute(ctx, j); e != nil {
		t.Fatal(e)
	}
	v, e := s.GetVolume(ctx, label.VolumeID)
	if e != nil || v.State != domain.Sealed {
		t.Fatal("seal lost", e)
	}
	if !errors.Is(s.CheckEnrollment(ctx, label.VolumeID, "drive", "simulator"), store.ErrNotEnrolled) {
		t.Fatal("revocation lost")
	}
	after, e := os.ReadFile(filepath.Join(root, "lab", "000001.tar"))
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("read-only rebuild altered archive")
	}
}
func TestManagedCorruptTailKeepsEarlierPair(t *testing.T) {
	drive, root, label, commits := managedFixture(t)
	file, e := os.OpenFile(filepath.Join(root, "lab", "000003.tar"), os.O_RDWR, 0)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = file.WriteAt([]byte{0xff}, 512); e != nil {
		t.Fatal(e)
	}
	file.Close()
	s := openStore(t)
	importer := Importer{Store: s, Drive: drive}
	j := importJob(t, s, ImportOptions{Mode: "managed"})
	e = importer.Execute(ctx, j)
	if e == nil {
		t.Fatal("damaged tail accepted")
	}
	if finish := s.FinishExecution(ctx, j.ID, e); finish != nil {
		t.Fatal(finish)
	}
	a, e := s.GetArchive(ctx, commits[0].ArchiveID)
	if e != nil || a.Integrity != domain.Verified {
		t.Fatal("earlier pair lost", e)
	}
	if _, e = s.GetArchive(ctx, commits[1].ArchiveID); !errors.Is(e, store.ErrNotFound) {
		t.Fatal("damaged pair published")
	}
	v, e := s.GetVolume(ctx, label.VolumeID)
	if e != nil || v.State != domain.Quarantined {
		t.Fatal("tail not quarantined", v, e)
	}
}
func TestLegacyObservedImportAndCollision(t *testing.T) {
	root := t.TempDir()
	raw := rawArchive(t, "legacy evidence", true)
	if e := os.WriteFile(filepath.Join(root, "000000.tar"), raw, 0400); e != nil {
		t.Fatal(e)
	}
	roots := []legacy.Root{{ID: "old", Path: root}}
	binding, e := legacy.Identify(ctx, roots, "old", legacy.SimulatorProfile, true)
	if e != nil {
		t.Fatal(e)
	}
	metadata, _ := json.Marshal(binding)
	id := uuid.NewSHA1(uuid.NameSpaceOID, metadata).String()
	s := openStore(t)
	importer := Importer{Store: s, LegacyRoots: roots}
	opts := ImportOptions{Mode: "legacy", SourceID: "old", Profile: legacy.SimulatorProfile, Label: "Old archive", Confirmed: true}
	for n := 0; n < 2; n++ {
		j := importJob(t, s, opts)
		if e = importer.Execute(ctx, j); e != nil {
			t.Fatal(e)
		}
	}
	count, e := s.ArchiveCount(ctx, id)
	if e != nil || count != 1 {
		t.Fatal(count, e)
	}
	archives, e := s.ListArchives(ctx, store.ArchiveQuery{VolumeID: id, Limit: 10})
	if e != nil || len(archives) != 1 {
		t.Fatal(archives, e)
	}
	archive := archives[0]
	if archive.Integrity != domain.Observed || archive.VerifiedAt != "" || archive.IntegrityBasis != "OBSERVED_NOW" || archive.RestoreEligible || archive.SHA256 != hash(raw) {
		t.Fatalf("false legacy verification %#v", archive)
	}
	members, e := s.ListMembers(ctx, archive.ID, 0, 500)
	if e != nil || len(members) != 2 || members[0].RawPath != "./" || members[1].RawPath != "./file" || members[1].Path != "file" {
		t.Fatal(members, e)
	}
	out, e := exec.Command("/usr/bin/tar", "--extract", "--to-stdout", "--file="+filepath.Join(root, "000000.tar"), "--", "./file").Output()
	if e != nil || string(out) != "legacy evidence" {
		t.Fatal("independent legacy recovery failed", e)
	}
	if e = os.Chmod(filepath.Join(root, "000000.tar"), 0600); e != nil {
		t.Fatal(e)
	}
	changed := rawArchive(t, "different legacy evidence", true)
	if e = os.WriteFile(filepath.Join(root, "000000.tar"), changed, 0600); e != nil {
		t.Fatal(e)
	}
	j := importJob(t, s, opts)
	e = importer.Execute(ctx, j)
	if !errors.Is(e, store.ErrCollision) {
		t.Fatal("legacy observation silently replaced", e)
	}
	old, e := s.GetArchive(ctx, archive.ID)
	if e != nil || old.SHA256 != archive.SHA256 {
		t.Fatal("original observation changed")
	}
}

func TestImportIdentityAndProfileGuards(t *testing.T) {
	drive, root, _, _ := managedFixture(t)
	s := openStore(t)
	importer := Importer{Store: s, Drive: drive}
	before, e := os.ReadFile(filepath.Join(root, "lab", "000000.tar"))
	if e != nil {
		t.Fatal(e)
	}
	j := importJob(t, s, ImportOptions{Mode: "managed", ExpectedVolumeID: "11111111-1111-4111-8111-111111111111"})
	e = importer.Execute(ctx, j)
	if !errors.Is(e, ErrPolicy) {
		t.Fatal("wrong volume identity accepted", e)
	}
	if e = s.FinishExecution(ctx, j.ID, e); e != nil {
		t.Fatal(e)
	}
	volumes, e := s.ListVolumes(ctx, 10)
	if e != nil || len(volumes) != 0 {
		t.Fatal("mismatching header registered", volumes, e)
	}
	j = importJob(t, s, ImportOptions{Mode: "legacy", SourceID: "old", Profile: legacy.SimulatorProfile, Label: "legacy", Confirmed: false})
	if e = importer.Execute(ctx, j); !errors.Is(e, ErrPolicy) {
		t.Fatal("legacy confirmation bypassed", e)
	}
	after, e := os.ReadFile(filepath.Join(root, "lab", "000000.tar"))
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("guard path changed media")
	}
}

func TestMalformedFirstLegacyFileIsNotClassified(t *testing.T) {
	for _, kind := range []string{"malformed", "compressed"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			raw := bytes.Repeat([]byte("x"), 2048)
			if kind == "compressed" {
				raw = []byte{0x1f, 0x8b, 8, 0}
				raw = append(raw, make([]byte, 2044)...)
			}
			if e := os.WriteFile(filepath.Join(root, "000000.tar"), raw, 0400); e != nil {
				t.Fatal(e)
			}
			s := openStore(t)
			importer := Importer{Store: s, LegacyRoots: []legacy.Root{{ID: "old", Path: root}}}
			job := importJob(t, s, ImportOptions{Mode: "legacy", SourceID: "old", Profile: legacy.SimulatorProfile, Label: "Unknown media", Confirmed: true})
			if e := importer.Execute(ctx, job); e == nil {
				t.Fatal("invalid first archive accepted")
			}
			volumes, e := s.ListVolumes(ctx, 10)
			if e != nil || len(volumes) != 0 {
				t.Fatalf("invalid source classified as legacy: %#v %v", volumes, e)
			}
		})
	}
}
func TestDamagedLegacyTailPreservesObservedPrefixAndQuarantines(t *testing.T) {
	root := t.TempDir()
	first := rawArchive(t, "retained legacy prefix", true)
	if e := os.WriteFile(filepath.Join(root, "000000.tar"), first, 0400); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "000001.tar"), bytes.Repeat([]byte("x"), 2048), 0400); e != nil {
		t.Fatal(e)
	}
	s := openStore(t)
	roots := []legacy.Root{{ID: "old", Path: root}}
	importer := Importer{Store: s, LegacyRoots: roots}
	job := importJob(t, s, ImportOptions{Mode: "legacy", SourceID: "old", Profile: legacy.SimulatorProfile, Label: "Partial media", Confirmed: true})
	failure := importer.Execute(ctx, job)
	if failure == nil {
		t.Fatal("damaged tail accepted")
	}
	if e := s.FinishExecution(ctx, job.ID, failure); e != nil {
		t.Fatal(e)
	}
	volumes, e := s.ListVolumes(ctx, 10)
	if e != nil || len(volumes) != 1 || volumes[0].Kind != "LEGACY" || volumes[0].State != domain.Quarantined {
		t.Fatalf("invalid partial classification: %#v %v", volumes, e)
	}
	archives, e := s.ListArchives(ctx, store.ArchiveQuery{VolumeID: volumes[0].ID, Limit: 10})
	if e != nil || len(archives) != 1 || archives[0].Integrity != domain.Observed || archives[0].SHA256 != hash(first) {
		t.Fatalf("observed prefix lost: %#v %v", archives, e)
	}
	after, e := os.ReadFile(filepath.Join(root, "000000.tar"))
	if e != nil || !bytes.Equal(after, first) {
		t.Fatal("prefix bytes changed")
	}
}

func TestManagedReimportDowngradesKnownCorruptArchive(t *testing.T) {
	drive, root, label, commits := managedFixture(t)
	s := openStore(t)
	importer := Importer{Store: s, Drive: drive}
	j := importJob(t, s, ImportOptions{Mode: "managed"})
	if err := importer.Execute(ctx, j); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "lab", "000003.tar")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[512] ^= 0xff
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	j = importJob(t, s, ImportOptions{Mode: "managed", ExpectedVolumeID: label.VolumeID})
	if err = importer.Execute(ctx, j); err == nil {
		t.Fatal("corrupt archive accepted")
	}
	if err = s.FinishExecution(ctx, j.ID, err); err != nil {
		t.Fatal(err)
	}
	for index, c := range commits {
		a, err := s.GetArchive(ctx, c.ArchiveID)
		want := domain.Verified
		if index == 1 {
			want = domain.IntegrityFailed
		}
		if err != nil || a.Integrity != want || a.VerifiedAt == "" || (index == 1 && a.RestoreEligible) {
			t.Fatal(a, err)
		}
	}
}
