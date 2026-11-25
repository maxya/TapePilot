package store

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/domain"
)

func instantFixture(t *testing.T, s *Store) (string, []Archive) {
	t.Helper()
	volume := uuid.NewString()
	if err := s.CreateVolume(ctx, Volume{ID: volume, Kind: "LEGACY", State: domain.LegacyReadOnly}); err != nil {
		t.Fatal(err)
	}
	instants := []string{
		"2026-11-24T12:00:00.100000000Z",
		"2026-11-24T12:00:00.1Z",
		"2026-11-24T12:00:00.100001Z",
		"2026-11-25T00:00:00Z",
		"2026-11-24T12:00:00Z",
		"2026-11-23T23:59:59.999999999Z",
	}
	archives := make([]Archive, len(instants))
	for i, instant := range instants {
		a := Archive{ID: fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1), VolumeID: volume, Sequence: int64(i + 1), DataIndex: int64(i), CreatedAt: instant, StreamBytes: 512, SHA256: strings.Repeat("a", 64), Integrity: domain.Observed, IntegrityBasis: "OBSERVED_NOW"}
		if err := s.transaction(ctx, func(tx *sql.Tx) error {
			if err := insertArchive(ctx, tx, a); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, "INSERT INTO members(archive_id,path,raw_path,parent_path,type,size,mtime,mode,link_target) VALUES(?,?,?,'','file',1,'',384,'')", a.ID, fmt.Sprintf("file-%d", i), fmt.Sprintf("file-%d", i))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		archives[i] = a
	}
	return volume, archives
}

func TestArchiveCursorOrdersExactInstantsAcrossFractionWidths(t *testing.T) {
	s, _ := openTest(t)
	volume, archives := instantFixture(t, s)
	want := []string{archives[5].ID, archives[4].ID, archives[0].ID, archives[1].ID, archives[2].ID, archives[3].ID}
	q := ArchiveQuery{VolumeID: volume, Limit: 1}
	for i, id := range want {
		page, err := s.ListArchives(ctx, q)
		if err != nil || len(page) != 1 || page[0].ID != id {
			t.Fatalf("page %d got %#v, error %v", i, page, err)
		}
		canonical, err := catalogInstant(page[0].CreatedAt)
		if err != nil || page[0].CreatedAt != canonical {
			t.Fatal("noncanonical new creation instant", page[0].CreatedAt, err)
		}
		q.AfterCreatedAt = page[0].CreatedAt
		q.AfterID = page[0].ID
	}
	if page, err := s.ListArchives(ctx, q); err != nil || len(page) != 0 {
		t.Fatal(page, err)
	}
	tied, err := s.ListArchives(ctx, ArchiveQuery{VolumeID: volume, AfterCreatedAt: "2026-11-24T12:00:00.1Z", AfterID: archives[0].ID, Limit: 1})
	if err != nil || len(tied) != 1 || tied[0].ID != archives[1].ID {
		t.Fatal("fraction-width cursor skipped tied timestamp", tied, err)
	}
}

func TestMemberDateBoundsNormalizeFractionsAndWholeSeconds(t *testing.T) {
	s, _ := openTest(t)
	volume, archives := instantFixture(t, s)
	for _, bounds := range []struct {
		after, before string
		want          []string
	}{
		{"2026-11-24T12:00:00Z", "2026-11-24T12:00:00Z", []string{archives[4].ID}},
		{"2026-11-24T12:00:00.1Z", "2026-11-24T12:00:00.100001Z", []string{archives[0].ID, archives[1].ID, archives[2].ID}},
		{"2026-11-24T00:00:00Z", "2026-11-24T23:59:59.999999999Z", []string{archives[0].ID, archives[1].ID, archives[2].ID, archives[4].ID}},
	} {
		page, err := s.SearchMembers(ctx, MemberQuery{VolumeID: volume, CreatedAfter: bounds.after, CreatedBefore: bounds.before})
		if err != nil || len(page) != len(bounds.want) {
			t.Fatal(bounds, page, err)
		}
		got := map[string]bool{}
		for _, m := range page {
			got[m.ArchiveID] = true
		}
		for _, id := range bounds.want {
			if !got[id] {
				t.Fatal("date bound omitted archive", id)
			}
		}
	}
	for _, bad := range []string{"2026-11-24T12:00:00+00:00", "yesterday", "2026-11-24", "2026-11-24T12:00:00.1000000001Z"} {
		if _, err := s.SearchMembers(ctx, MemberQuery{CreatedAfter: bad}); err == nil {
			t.Fatal("invalid bound accepted", bad)
		}
	}
}

func TestExistingArchiveInstantsNormalizeInMigrationWithoutPrecisionLoss(t *testing.T) {
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
	if _, err = db.Exec("CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL,checksum TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		script, err := migrations.ReadFile("migrations/" + entries[i].Name())
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(string(script)); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec("INSERT INTO schema_migrations VALUES(?,?,?)", i+1, stamp(), fmt.Sprintf("%x", sha256.Sum256(script))); err != nil {
			t.Fatal(err)
		}
	}
	volume := uuid.NewString()
	if _, err = db.Exec("INSERT INTO volumes VALUES(?,'LEGACY','synthetic','','','LEGACY_READ_ONLY',NULL,?)", volume, stamp()); err != nil {
		t.Fatal(err)
	}
	raw := []string{"2026-11-24T12:00:00Z", "2026-11-24T12:00:00.1Z", "2026-11-24T12:00:00.100001Z", "2026-11-25T00:00:00.999999999Z"}
	for i, instant := range raw {
		if _, err = db.Exec("INSERT INTO archives(id,volume_id,sequence,data_file_index,source_label,created_at,stream_bytes,sha256,integrity_state,integrity_basis,restore_eligible) VALUES(?,?,?,?,?,?,512,?,'OBSERVED','OBSERVED_NOW',0)", fmt.Sprintf("existing-%d", i), volume, i+1, i, "synthetic", instant, strings.Repeat("a", 64)); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i, instant := range raw {
		a, err := s.GetArchive(ctx, fmt.Sprintf("existing-%d", i))
		want, _ := catalogInstant(instant)
		if err != nil || a.CreatedAt != want {
			t.Fatal(a.CreatedAt, want, err)
		}
	}
	snapshots, _ := filepath.Glob(filepath.Join(dir, "pre-migration-v4-*.db"))
	if len(snapshots) != 1 {
		t.Fatal(snapshots)
	}
}
