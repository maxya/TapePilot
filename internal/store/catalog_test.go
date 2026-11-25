package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/media"
	"github.com/maxya/TapePilot/internal/tape"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func importFixture(t *testing.T, s *Store) (Job, Volume, Archive) {
	t.Helper()
	j := job(t, s, domain.Import, "")
	j, err := s.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	j = advance(t, s, j, domain.Running)
	v := Volume{ID: uuid.NewString(), Kind: "LEGACY", Label: "legacy", State: domain.LegacyReadOnly}
	if err = s.RegisterImportedVolume(ctx, j.ID, v, json.RawMessage(`{"source_id":"fixture","profile":"plain","root_identity":"synthetic"}`)); err != nil {
		t.Fatal(err)
	}
	attempt := uuid.NewString()
	if err = s.StageImportMembers(ctx, j.ID, attempt, []Member{{Path: "a%_\nname", RawPath: "./a%_\nname", Type: "file", Size: 3}, {Path: "directory/child", RawPath: "./directory/child", ParentPath: "directory", Type: "file", Size: 5}}); err != nil {
		t.Fatal(err)
	}
	a := Archive{ID: uuid.NewString(), VolumeID: v.ID, JobID: j.ID, Sequence: 1, DataIndex: 0, SourceLabel: "fixture source", StreamBytes: 10240, SHA256: strings.Repeat("a", 64), Integrity: domain.Observed, IntegrityBasis: "OBSERVED_NOW"}
	if err = s.PublishImportedArchive(ctx, j.ID, attempt, a); err != nil {
		t.Fatal(err)
	}
	return j, v, a
}
func TestOfflineSearchCursorAndObservedImport(t *testing.T) {
	s, _ := openTest(t)
	j, v, a := importFixture(t, s)
	loaded, err := s.GetArchive(ctx, a.ID)
	if err != nil || loaded.VerifiedAt != "" || loaded.Integrity != domain.Observed {
		t.Fatal(loaded, err)
	}
	rows, err := s.SearchMembers(ctx, MemberQuery{VolumeID: v.ID, Text: "%_", Limit: 5000})
	if err != nil || len(rows) != 1 || rows[0].RawPath != "./a%_\nname" {
		t.Fatal(rows, err)
	}
	first, err := s.SearchMembers(ctx, MemberQuery{ArchiveID: a.ID, Limit: 1})
	if err != nil || len(first) != 1 {
		t.Fatal(first, err)
	}
	next, err := s.SearchMembers(ctx, MemberQuery{ArchiveID: a.ID, Limit: 1, AfterPath: first[0].Path, AfterID: first[0].ID})
	if err != nil || len(next) != 1 || next[0].ID == first[0].ID {
		t.Fatal(next, err)
	}
	parent := "directory"
	browse, err := s.SearchMembers(ctx, MemberQuery{ArchiveID: a.ID, ParentPath: &parent})
	if err != nil || len(browse) != 1 || browse[0].Path != "directory/child" {
		t.Fatal(browse, err)
	}
	byLabel, err := s.SearchMembers(ctx, MemberQuery{VolumeLabel: "egac", SourceLabel: "fixture", JobID: j.ID})
	if err != nil || len(byLabel) != 2 || byLabel[0].DataFileIndex != 0 || byLabel[0].VolumeLabel != "legacy" {
		t.Fatal(byLabel, err)
	}
	missingLabel, err := s.SearchMembers(ctx, MemberQuery{VolumeLabel: "absent"})
	if err != nil || len(missingLabel) != 0 {
		t.Fatal(missingLabel, err)
	}
	legacyDescriptor, err := s.LegacyDescriptor(ctx, v.ID)
	if err != nil || len(legacyDescriptor.Archives) != 1 || legacyDescriptor.Archives[0].IntegrityBasis != "OBSERVED_NOW" {
		t.Fatal(legacyDescriptor, err)
	}

	archives, err := s.ListArchives(ctx, ArchiveQuery{VolumeID: v.ID})
	if err != nil || len(archives) != 1 {
		t.Fatal(archives, err)
	}
	if err = s.CheckEnrollment(ctx, v.ID, "sim", "profile"); err == nil {
		t.Fatal("legacy import enrolled")
	}
	advance(t, s, j, domain.Succeeded)
	var out bytes.Buffer
	if err = s.WriteCatalogNDJSON(ctx, v.ID, &out); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&out)
	count := 0
	for {
		var row CatalogMember
		err = decoder.Decode(&row)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
	}
	if count != 2 {
		t.Fatal(count)
	}
}
func TestImportCollisionPreservesPriorCatalogAndProtection(t *testing.T) {
	s, _ := openTest(t)
	j, v, a := importFixture(t, s)
	if err := s.SetVolumeState(ctx, v.ID, domain.Sealed); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterImportedVolume(ctx, j.ID, v, json.RawMessage(`{"root_identity":"synthetic","profile":"plain","source_id":"fixture"}`)); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetVolume(ctx, v.ID)
	if got.State != domain.Sealed {
		t.Fatal(got)
	}
	if err := s.RegisterImportedVolume(ctx, j.ID, v, json.RawMessage(`{"root_identity":"changed","profile":"plain","source_id":"fixture"}`)); !errors.Is(err, ErrCollision) {
		t.Fatal(err)
	}
	attempt := uuid.NewString()
	s.StageImportMembers(ctx, j.ID, attempt, []Member{{Path: "new", RawPath: "new", Type: "file"}})
	bad := a
	bad.SHA256 = strings.Repeat("b", 64)
	if err := s.PublishImportedArchive(ctx, j.ID, attempt, bad); !errors.Is(err, ErrCollision) {
		t.Fatal(err)
	}
	members, err := s.ListMembers(ctx, a.ID, 0, 500)
	if err != nil || len(members) != 2 {
		t.Fatal(members, err)
	}
	if err = s.FinishExecution(ctx, j.ID, errors.New("collision")); err != nil {
		t.Fatal(err)
	}
	var staged int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM staged_members").Scan(&staged); err != nil || staged != 0 {
		t.Fatal(staged, err)
	}
}
func TestDurableRestorePublicationAndCrashOutcome(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprint(complete), func(t *testing.T) {
			s, _ := openTest(t)
			importJob, v, a := importFixture(t, s)
			advance(t, s, importJob, domain.Succeeded)
			options, _ := json.Marshal(map[string]any{"archive_id": a.ID, "restore_root_id": "restore", "destination_slug": "fresh", "all": true})
			j, err := s.Enqueue(ctx, Job{Kind: domain.Restore, VolumeID: v.ID, Options: options})
			if err != nil {
				t.Fatal(err)
			}
			j, err = s.Claim(ctx)
			if err != nil {
				t.Fatal(err)
			}
			j = advance(t, s, j, domain.Running)
			if err = s.RecordVerifiedSpool(ctx, j.ID, strings.Repeat("b", 64), a.StreamBytes, 2, 1); err == nil {
				t.Fatal("bad spool accepted")
			}
			if err = s.CompleteRestorePublication(ctx, j.ID); err == nil {
				t.Fatal("missing publication intent accepted")
			}
			if err = s.RecordVerifiedSpool(ctx, j.ID, a.SHA256, a.StreamBytes, 2, 1); err != nil {
				t.Fatal(err)
			}
			publication, err := s.RestorePublication(ctx, j.ID)
			if err != nil || publication.SelectedMembers == nil || *publication.SelectedMembers != 2 || publication.ExpandedMembers == nil || *publication.ExpandedMembers != 1 || publication.IntegrityBasis != a.IntegrityBasis {
				t.Fatal("restore summary", publication, err)
			}
			if err = s.BeginRestorePublication(ctx, j.ID, "other", "fresh"); err == nil {
				t.Fatal("wrong root accepted")
			}
			if err = s.BeginRestorePublication(ctx, j.ID, "restore", "fresh"); err != nil {
				t.Fatal(err)
			}
			if complete {
				if err = s.CompleteRestorePublication(ctx, j.ID); err != nil {
					t.Fatal(err)
				}
				got, _ := s.GetJob(ctx, j.ID)
				if got.State != domain.Succeeded {
					t.Fatal(got)
				}
			} else {
				if err = s.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
				got, _ := s.GetJob(ctx, j.ID)
				if got.State != domain.RecoveryRequired {
					t.Fatal(got)
				}
			}
		})
	}
}
func TestPrivateExpiringExportsAndRealMetadataDescriptor(t *testing.T) {
	s, _ := openTest(t)
	id := volume(t, s)
	var label media.Volume
	b, err := os.ReadFile("../../schemas/fixtures/volume.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &label); err != nil {
		t.Fatal(err)
	}
	label.VolumeID = id
	raw, _ := json.Marshal(label)
	if err = s.SaveVolumeMetadata(ctx, id, raw); err != nil {
		t.Fatal(err)
	}
	d, err := s.Descriptor(ctx, id)
	if err != nil || d.VolumeID != id || d.Profile.RecordBytes != tape.RecordBytes || d.Label != label.LabelAtInitialization {
		t.Fatal(d, err)
	}
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	export, err := s.CreateExport(ctx, dir, "descriptor", id, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f, record, err := s.OpenExport(ctx, dir, export.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if record.Bytes <= 0 || record.SHA256 == "" {
		t.Fatal(record)
	}
	if _, err = s.db.Exec("UPDATE exports SET expires_at='2000-01-01T00:00:00.000000000Z' WHERE id=?", export.ID); err != nil {
		t.Fatal(err)
	}
	if f, _, err = s.OpenExport(ctx, dir, export.ID); !errors.Is(err, ErrExportExpired) {
		if f != nil {
			f.Close()
		}
		t.Fatal(err)
	}
	if n, err := s.PurgeExpiredExports(ctx, dir); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if _, err = os.Stat(dir + "/" + export.Filename); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}
func BenchmarkCatalogMillionMembers(b *testing.B) {
	ctx := context.Background()
	dir := b.TempDir()
	os.Chmod(dir, 0700)
	s, err := Open(ctx, dir)
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	j, err := s.Enqueue(ctx, Job{Kind: domain.Import})
	if err != nil {
		b.Fatal(err)
	}
	j, err = s.Claim(ctx)
	if err != nil {
		b.Fatal(err)
	}
	j, err = s.Transition(ctx, j.ID, j.Version, domain.Running, "indexing", "")
	if err != nil {
		b.Fatal(err)
	}
	volume := uuid.NewString()
	if err = s.RegisterImportedVolume(ctx, j.ID, Volume{ID: volume, Kind: "LEGACY", Label: "million-member-synthetic", State: domain.LegacyReadOnly}, json.RawMessage(`{"source_id":"synthetic","profile":"plain","root_identity":"benchmark"}`)); err != nil {
		b.Fatal(err)
	}
	attempt := uuid.NewString()
	started := time.Now()
	for start := 0; start < 1000000; start += 1000 {
		members := make([]Member, 1000)
		for i := range members {
			n := start + i
			parent := fmt.Sprintf("group-%03d", n/10000)
			name := fmt.Sprintf("%s/file-%09d.txt", parent, n)
			members[i] = Member{Path: name, RawPath: name, ParentPath: parent, Type: "file", Size: 1}
		}
		if err = s.StageImportMembers(ctx, j.ID, attempt, members); err != nil {
			b.Fatal(err)
		}
	}
	archive := uuid.NewString()
	if err = s.PublishImportedArchive(ctx, j.ID, attempt, Archive{ID: archive, VolumeID: volume, JobID: j.ID, Sequence: 1, DataIndex: 0, StreamBytes: 262144, SHA256: strings.Repeat("a", 64), Integrity: domain.Observed, IntegrityBasis: "OBSERVED_NOW"}); err != nil {
		b.Fatal(err)
	}
	elapsed := time.Since(started).Seconds()
	b.ResetTimer()
	for range b.N {
		rows, err := s.SearchMembers(ctx, MemberQuery{ArchiveID: archive, Text: "file-000999", Limit: 100})
		if err != nil || len(rows) != 100 {
			b.Fatal(len(rows), err)
		}
	}
	b.StopTimer()
	b.ReportMetric(elapsed, "load_seconds")
	b.ReportMetric(1000000, "members")
}

func TestInterruptedImportCleansStagingPreservesCommittedPair(t *testing.T) {
	s, dir := openTest(t)
	j, _, a := importFixture(t, s)
	attempt := uuid.NewString()
	if err := s.StageImportMembers(ctx, j.ID, attempt, []Member{{Path: "unfinished", RawPath: "unfinished", Type: "file"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err = restarted.GetArchive(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = restarted.db.QueryRow("SELECT COUNT(*) FROM staged_members").Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

type blockedWriter struct {
	started chan struct{}
	release chan struct{}
	once    bool
}

func (w *blockedWriter) Write(p []byte) (int, error) {
	if !w.once {
		w.once = true
		close(w.started)
		<-w.release
	}
	return len(p), nil
}
func TestCatalogSnapshotAllowsWriterToPersistEvidence(t *testing.T) {
	s, _ := openTest(t)
	_, v, _ := importFixture(t, s)
	writer := &blockedWriter{started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- s.WriteCatalogNDJSON(ctx, v.ID, writer) }()
	select {
	case <-writer.started:
	case <-time.After(5 * time.Second):
		t.Fatal("export did not start")
	}
	queryCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	_, err := s.Enqueue(queryCtx, Job{Kind: domain.Inspect})
	close(writer.release)
	if err != nil {
		t.Fatal("snapshot blocked writer", err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
