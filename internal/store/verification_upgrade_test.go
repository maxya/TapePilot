package store

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maxya/TapePilot/internal/domain"
)

func TestVersion13UpgradeWithdrawsOnlyHistoricallyUnverifiedFailureScope(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "catalog.db")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec("CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL,checksum TEXT NOT NULL)")
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 13; i++ {
		script, err := migrations.ReadFile("migrations/" + entries[i].Name())
		if err != nil {
			t.Fatal(err)
		}
		exec(string(script))
		exec("INSERT INTO schema_migrations VALUES(?,?,?)", i+1, stamp(), fmt.Sprintf("%x", sha256.Sum256(script)))
	}
	before := "2026-11-24T00:00:00Z"
	failedAt := "2026-11-24T00:00:00.1Z"
	after := "2026-11-24T00:00:00.100001Z"
	hash := strings.Repeat("a", 64)
	for _, v := range []string{"identified", "wrong-loaded"} {
		exec("INSERT INTO volumes(id,kind,label,state,header_sha256,created_at) VALUES(?,'MANAGED','fixture','MANAGED',?,?)", v, hash, before)
	}
	addJob := func(id, kind, state, phase, volume, at string) {
		exec("INSERT INTO jobs(id,kind,state,phase,volume_id,options_json,created_at,completed_at) VALUES(?,?,?,?,?,'{}',?,?)", id, kind, state, phase, volume, before, at)
	}
	addJob("failed-readback", "VERIFY", "RECOVERY_REQUIRED", "readback", "identified", failedAt)
	addJob("wrong-media-preflight", "VERIFY", "RECOVERY_REQUIRED", "preflight", "wrong-loaded", failedAt)
	addJob("later-success", "VERIFY", "SUCCEEDED", "complete", "identified", after)
	cases := []struct {
		id, volume, verified string
		want                 domain.Integrity
	}{
		{"valid-prefix", "identified", before, domain.Verified},
		{"stale", "identified", before, domain.IntegrityUnknown},
		{"later-readback", "identified", before, domain.Verified},
		{"later-import", "identified", after, domain.Verified},
		{"new-backup", "identified", after, domain.Verified},
		{"wrong-loaded-expected", "wrong-loaded", before, domain.Verified},
	}
	for i, c := range cases {
		exec(`INSERT INTO archives(id,volume_id,sequence,data_file_index,commit_file_index,source_label,created_at,stream_bytes,sha256,commit_sha256,integrity_state,integrity_basis,verified_at,restore_eligible) VALUES(?,?,?,?,?,'fixture',?,262144,?,?,'VERIFIED','WRITE_READBACK',?,1)`, c.id, c.volume, i+1, 2*i+1, 2*i+2, before, hash, hash, c.verified)
	}
	for _, e := range []struct{ archive, job, at string }{{"valid-prefix", "failed-readback", "2026-11-24T00:00:00.09Z"}, {"later-readback", "later-success", after}} {
		exec("INSERT INTO verifications VALUES(?,?,?,'CATALOG_READBACK',?,?,?,262144,262144,1,1,?)", e.archive, e.job, e.archive, hash, hash, hash, e.at)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, c := range cases {
		a, err := s.GetArchive(ctx, c.id)
		if err != nil || a.Integrity != c.want || a.VerifiedAt != c.verified || a.SHA256 != hash || a.IntegrityBasis != "WRITE_READBACK" {
			t.Fatal(c, a, err)
		}
		if c.want == domain.IntegrityUnknown && a.RestoreEligible {
			t.Fatal("stale archive remained eligible", a)
		}
	}
	identified, _ := s.GetVolume(ctx, "identified")
	wrong, _ := s.GetVolume(ctx, "wrong-loaded")
	if identified.State != domain.Quarantined || wrong.State != domain.Managed {
		t.Fatal(identified, wrong)
	}
}

func TestMediaAssessmentRestartNeverPromotesUnclosedPrefix(t *testing.T) {
	s, j, _, aa := assessmentFixture(t)
	if _, err := s.db.Exec("UPDATE archives SET integrity_state='UNKNOWN',restore_eligible=0 WHERE id=?", aa[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("UPDATE archives SET integrity_state='FAILED',restore_eligible=0 WHERE id=?", aa[1].ID); err != nil {
		t.Fatal(err)
	}
	checkpoint(t, s, j, aa[0])
	if err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	prefix, _ := s.GetArchive(ctx, aa[0].ID)
	failed, _ := s.GetArchive(ctx, aa[1].ID)
	if prefix.Integrity != domain.IntegrityUnknown || failed.Integrity != domain.IntegrityFailed || prefix.VerifiedAt != aa[0].VerifiedAt || failed.VerifiedAt != aa[1].VerifiedAt {
		t.Fatal(prefix, failed)
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM verifications WHERE job_id=?", j.ID).Scan(&n); err != nil || n != 0 {
		t.Fatal("unclosed checkpoint created successful history", n, err)
	}
}
