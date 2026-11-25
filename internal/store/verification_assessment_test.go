package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/domain"
)

func assessmentFixture(t *testing.T) (*Store, Job, string, []Archive) {
	t.Helper()
	s, _ := openTest(t)
	v := volume(t, s)
	// Synthetic baseline models already published archives, independently of a
	// scanner. This lets transaction, restart and resource-close failures be tested.
	owner := job(t, s, domain.Inspect, "")
	owner, err := s.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	owner = advance(t, s, owner, domain.Running)
	advance(t, s, owner, domain.Succeeded)
	var archives []Archive
	for i := int64(1); i <= 2; i++ {
		a := Archive{ID: uuid.NewString(), VolumeID: v, JobID: owner.ID, Sequence: i, DataIndex: 2*i - 1, SHA256: strings.Repeat(fmt.Sprint(i), 64), CommitSHA256: strings.Repeat("a", 64), StreamBytes: 262144, Integrity: domain.Verified, IntegrityBasis: "WRITE_READBACK", VerifiedAt: "2026-01-01T00:00:00.000000000Z", RestoreEligible: true}
		if _, err = s.db.Exec(`INSERT INTO archives(id,volume_id,job_id,sequence,data_file_index,commit_file_index,source_label,created_at,stream_bytes,sha256,commit_sha256,integrity_state,integrity_basis,verified_at,restore_eligible) VALUES(?,?,?,?,?,?,'fixture',?,?,?,?,?,?,?,1)`, a.ID, v, owner.ID, i, a.DataIndex, a.DataIndex+1, stamp(), a.StreamBytes, a.SHA256, a.CommitSHA256, a.Integrity, a.IntegrityBasis, a.VerifiedAt); err != nil {
			t.Fatal(err)
		}
		archives = append(archives, a)
	}
	j := job(t, s, domain.Verify, v)
	j, err = s.Claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BeginMediaAssessment(ctx, j.ID, MediaAssessment{VolumeID: v, HeaderSHA256: strings.Repeat("c", 64)}); err != nil {
		t.Fatal(err)
	}
	j = advance(t, s, j, domain.Running)
	j = advance(t, s, j, domain.Verifying)
	return s, j, v, archives
}
func checkpoint(t *testing.T, s *Store, j Job, a Archive) {
	t.Helper()
	if err := s.CheckpointMediaAssessment(ctx, j.ID, a.ID, a.DataIndex, a.SHA256, a.StreamBytes, a.CommitSHA256); err != nil {
		t.Fatal(err)
	}
}
func TestMediaAssessmentTerminalAtomicityAndHistoricalEvidence(t *testing.T) {
	for _, test := range []struct {
		name                     string
		cancel, corrupt, restart bool
		want                     domain.Integrity
		state                    domain.State
	}{
		{name: "confirmed second", corrupt: true, want: domain.IntegrityFailed, state: domain.RecoveryRequired},
		{name: "operational read", want: domain.IntegrityUnknown, state: domain.RecoveryRequired},
		{name: "cancelled unread", cancel: true, want: domain.IntegrityUnknown, state: domain.Cancelled},
		{name: "restart unread", restart: true, want: domain.IntegrityUnknown, state: domain.RecoveryRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, j, v, aa := assessmentFixture(t)
			checkpoint(t, s, j, aa[0])
			if err := s.FailMediaAssessment(ctx, j.ID, 3, test.corrupt); err != nil {
				t.Fatal(err)
			}
			before, _ := s.GetArchive(ctx, aa[1].ID)
			if before.Integrity != domain.Verified {
				t.Fatal("stage prematurely changed catalog", before)
			}
			if test.cancel {
				if err := s.RequestCancel(ctx, j.ID); err != nil {
					t.Fatal(err)
				}
			}
			var err error
			if test.restart {
				err = s.Reconcile(ctx)
			} else {
				cause := errors.New("read failed")
				if test.cancel {
					cause = context.Canceled
				}
				err = s.FinishExecution(ctx, j.ID, cause)
			}
			if err != nil {
				t.Fatal(err)
			}
			current, _ := s.GetJob(ctx, j.ID)
			if current.State != test.state {
				t.Fatal(current)
			}
			prefix, _ := s.GetArchive(ctx, aa[0].ID)
			affected, _ := s.GetArchive(ctx, aa[1].ID)
			wantPrefix := domain.Verified
			if test.restart {
				wantPrefix = domain.IntegrityUnknown
			}
			if prefix.Integrity != wantPrefix || affected.Integrity != test.want || affected.RestoreEligible || affected.VerifiedAt != aa[1].VerifiedAt || affected.SHA256 != aa[1].SHA256 || affected.CommitSHA256 != aa[1].CommitSHA256 || affected.IntegrityBasis != aa[1].IntegrityBasis {
				t.Fatal(prefix, affected)
			}
			loaded, _ := s.GetVolume(ctx, v)
			if !test.cancel && loaded.State != domain.Quarantined {
				t.Fatal(loaded)
			}
			if test.cancel && loaded.State != domain.Managed {
				t.Fatal("cancel alone quarantined media", loaded)
			}
		})
	}
}
func TestMediaAssessmentLateCloseAndIdentityInvalidateEveryStage(t *testing.T) {
	for _, identity := range []bool{false, true} {
		t.Run(fmt.Sprint(identity), func(t *testing.T) {
			s, j, _, aa := assessmentFixture(t)
			for _, a := range aa {
				checkpoint(t, s, j, a)
			}
			if identity {
				if err := s.InvalidateMediaAssessment(ctx, j.ID); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := s.CompleteMediaAssessment(ctx, j.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.FinishExecution(ctx, j.ID, errors.New("final read/lease close uncertain")); err != nil {
				t.Fatal(err)
			}
			for _, a := range aa {
				got, _ := s.GetArchive(ctx, a.ID)
				if got.Integrity != domain.IntegrityUnknown || got.RestoreEligible || got.VerifiedAt != a.VerifiedAt {
					t.Fatal(got)
				}
			}
		})
	}
}
func TestMediaAssessmentReverifyPreservesFailedSuffixAndSeal(t *testing.T) {
	s, j, v, aa := assessmentFixture(t)
	checkpoint(t, s, j, aa[0])
	if err := s.FailMediaAssessment(ctx, j.ID, 3, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVolumeState(ctx, v, domain.Sealed); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishExecution(ctx, j.ID, ErrEvidence); err != nil {
		t.Fatal(err)
	}
	next := func() Job {
		j := job(t, s, domain.Verify, v)
		j, err := s.Claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.BeginMediaAssessment(ctx, j.ID, MediaAssessment{VolumeID: v, HeaderSHA256: strings.Repeat("c", 64)}); err != nil {
			t.Fatal(err)
		}
		j = advance(t, s, j, domain.Running)
		return advance(t, s, j, domain.Verifying)
	}
	j = next()
	if err := s.FinishExecution(ctx, j.ID, context.Canceled); err != nil {
		t.Fatal(err)
	}
	failed, _ := s.GetArchive(ctx, aa[1].ID)
	if failed.Integrity != domain.IntegrityFailed {
		t.Fatal("unread overwrote known failure", failed)
	}
	j = next()
	for _, a := range aa {
		checkpoint(t, s, j, a)
	}
	if err := s.CompleteMediaAssessment(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishExecution(ctx, j.ID, nil); err != nil {
		t.Fatal(err)
	}
	current, _ := s.GetJob(ctx, j.ID)
	if current.State != domain.Succeeded {
		t.Fatal(current)
	}
	for _, a := range aa {
		got, _ := s.GetArchive(ctx, a.ID)
		if got.Integrity != domain.Verified || got.IntegrityBasis != "WRITE_READBACK" || got.SHA256 != a.SHA256 {
			t.Fatal(got)
		}
	}
	loaded, _ := s.GetVolume(ctx, v)
	if loaded.State != domain.Sealed {
		t.Fatal("reverify unsealed media", loaded)
	}
}
func TestMediaAssessmentRejectsWrongIdentityAndBaseline(t *testing.T) {
	s, j, v, aa := assessmentFixture(t)
	if err := s.CheckpointMediaAssessment(ctx, j.ID, aa[0].ID, 1, strings.Repeat("f", 64), aa[0].StreamBytes, aa[0].CommitSHA256); !errors.Is(err, ErrEvidence) {
		t.Fatal(err)
	}
	if err := s.BeginMediaAssessment(ctx, j.ID, MediaAssessment{VolumeID: v, HeaderSHA256: strings.Repeat("f", 64)}); !errors.Is(err, ErrEvidence) {
		t.Fatal(err)
	}
	if err := s.CompleteMediaAssessment(ctx, j.ID); !errors.Is(err, ErrEvidence) {
		t.Fatal("incomplete assessment completed", err)
	}
}

func TestMediaAssessmentCatalogAndTerminalOutcomeRollbackTogether(t *testing.T) {
	s, j, _, aa := assessmentFixture(t)
	checkpoint(t, s, j, aa[0])
	if err := s.FailMediaAssessment(ctx, j.ID, 3, true); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_assessment_finish BEFORE UPDATE OF state ON jobs WHEN NEW.id='` + j.ID + `' AND NEW.state='RECOVERY_REQUIRED' BEGIN SELECT RAISE(ABORT,'synthetic terminal database failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishExecution(ctx, j.ID, ErrEvidence); err == nil {
		t.Fatal("synthetic database error swallowed")
	}
	for _, a := range aa {
		got, _ := s.GetArchive(ctx, a.ID)
		if got.Integrity != domain.Verified || got.VerifiedAt != a.VerifiedAt || !got.RestoreEligible {
			t.Fatal("catalog changed without terminal result", got)
		}
	}
	var finalized int
	if err := s.db.QueryRow("SELECT finalized FROM media_assessments WHERE job_id=?", j.ID).Scan(&finalized); err != nil || finalized != 0 {
		t.Fatal(finalized, err)
	}
	if _, err := s.db.Exec("DROP TRIGGER reject_assessment_finish"); err != nil {
		t.Fatal(err)
	}
	if err := s.FinishExecution(ctx, j.ID, ErrEvidence); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetArchive(ctx, aa[1].ID)
	if got.Integrity != domain.IntegrityFailed {
		t.Fatal(got)
	}
}
