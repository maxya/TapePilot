package store

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func profile(t *testing.T, s *Store) BackupProfile {
	t.Helper()
	target := volume(t, s)
	p, e := s.CreateBackupProfile(ctx, BackupProfile{Name: "Synthetic backup", SourceID: "synthetic", RelativeSelection: "data", Exclusions: []string{"data/cache"}, TargetVolumeID: target, Enabled: true}, "admin")
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func TestProfilesRevisionAndAtomicManualQueue(t *testing.T) {
	s, _ := openTest(t)
	p := profile(t, s)
	stale := p
	p.Name = "Updated profile"
	p, e := s.UpdateBackupProfile(ctx, p, "admin")
	if e != nil || p.Revision != 2 {
		t.Fatal(p, e)
	}
	if _, e = s.UpdateBackupProfile(ctx, stale, "admin"); !errors.Is(e, ErrConflict) {
		t.Fatal("stale revision accepted")
	}
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	first, e := s.EnqueueManualProfile(ctx, p.ID, p.Revision, "request-key", digest, "admin")
	if e != nil {
		t.Fatal(e)
	}
	repeated, e := s.EnqueueManualProfile(ctx, p.ID, p.Revision, "request-key", digest, "admin")
	if e != nil || repeated.ID != first.ID {
		t.Fatal("request duplicated", e)
	}
	if _, e = s.EnqueueManualProfile(ctx, p.ID, p.Revision, "request-key", "", "admin"); !errors.Is(e, ErrConflict) {
		t.Fatal("idempotency payload collision accepted")
	}
	var raw map[string]json.RawMessage
	if e = json.Unmarshal(first.Options, &raw); e != nil {
		t.Fatal(e)
	}
	if string(raw["plan_digest"]) != "\""+digest+"\"" || first.VolumeID != p.TargetVolumeID {
		t.Fatal("queue snapshot changed")
	}
	if e = s.RequestCancel(ctx, first.ID); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	queued := 0
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := s.EnqueueProfile(ctx, p.ID, p.Revision, "admin")
			if e == nil {
				mu.Lock()
				queued++
				mu.Unlock()
			} else if !errors.Is(e, ErrOutstanding) {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if queued != 1 {
		t.Fatal("multiple outstanding profile jobs", queued)
	}
}
func TestScheduleSlotsCoalesceAndBlockedActionsNotify(t *testing.T) {
	s, _ := openTest(t)
	p := profile(t, s)
	if e := s.EnrollVolume(ctx, Enrollment{VolumeID: p.TargetVolumeID, DriveID: "simulator-drive", Profile: "simulator", OperatorRef: "test"}); e != nil {
		t.Fatal(e)
	}
	schedule, e := s.CreateSchedule(ctx, Schedule{ProfileID: p.ID, Cadence: "daily", Timezone: "UTC", LocalTime: time.Now().UTC().Add(47 * time.Hour).Format("15:04"), Enabled: true}, "admin")
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC().Add(48 * time.Hour)
	due := now.Add(-time.Hour).Truncate(time.Minute)
	slot := due.Format("2006-01-02T15:04")
	first, created, e := s.FireSchedule(ctx, schedule.ID, schedule.Revision, slot, due, now, true)
	if e != nil || !created {
		t.Fatal(first, created, e)
	}
	if _, created, e = s.FireSchedule(ctx, schedule.ID, schedule.Revision, slot, due, now, true); e != nil || created {
		t.Fatal("same slot duplicated", e)
	}
	later := due.Add(24 * time.Hour)
	laterKey := later.Format("2006-01-02T15:04")
	if _, created, e = s.FireSchedule(ctx, schedule.ID, schedule.Revision, laterKey, later, later.Add(time.Hour), true); e != nil || created {
		t.Fatal("outstanding profile duplicated", e)
	}
	pending, e := s.Schedule(ctx, schedule.ID)
	if e != nil || pending.LastSlot != slot {
		t.Fatal("busy slot consumed instead of coalesced", e)
	}
	if e = s.RequestCancel(ctx, first.ID); e != nil {
		t.Fatal(e)
	}
	second, created, e := s.FireSchedule(ctx, schedule.ID, schedule.Revision, laterKey, later, later.Add(time.Hour), true)
	if e != nil || !created || second.ID == first.ID {
		t.Fatal("missed slot lost", e)
	}
	if e = s.RequestCancel(ctx, second.ID); e != nil {
		t.Fatal(e)
	}
	if e = s.RevokeEnrollment(ctx, p.TargetVolumeID, "test revocation"); e != nil {
		t.Fatal(e)
	}
	blocked := later.Add(24 * time.Hour)
	if _, created, e = s.FireSchedule(ctx, schedule.ID, schedule.Revision, blocked.Format("2006-01-02T15:04"), blocked, blocked.Add(time.Hour), true); e != nil || created {
		t.Fatal("revoked volume scheduled", e)
	}
	inbox, e := s.ListNotifications(ctx, 0, 200)
	if e != nil {
		t.Fatal(e)
	}
	var notification int64
	for _, n := range inbox {
		if n.Code == "ACTION_REQUIRED" {
			notification = n.ID
		}
	}
	if notification == 0 {
		t.Fatal("blocked schedule has no action notification")
	}
	if e = s.ReadNotification(ctx, notification); e != nil {
		t.Fatal(e)
	}
	inbox, e = s.ListNotifications(ctx, notification-1, 1)
	if e != nil || inbox[0].ReadAt == nil {
		t.Fatal("inbox read status not stored", e)
	}
	audit, e := s.ListAudit(ctx, 0, 200)
	if e != nil || len(audit) == 0 {
		t.Fatal("audit missing", e)
	}
}
func TestAuditAppendOnlyAndNoUnsafeValues(t *testing.T) {
	s, _ := openTest(t)
	if e := s.Audit(ctx, "LOGIN_SUCCEEDED", "session"); e != nil {
		t.Fatal(e)
	}
	if e := s.Audit(ctx, "SOME_ACTION", "/secret/source/name"); !errors.Is(e, ErrOperations) {
		t.Fatal("path accepted into audit")
	}
	if _, e := s.db.ExecContext(ctx, "DELETE FROM audit_events"); e == nil {
		t.Fatal("audit deletion allowed")
	}
	if e := s.AuthAudit(ctx, "LOGIN", "FAILED", uuid.NewString()); e != nil {
		t.Fatal(e)
	}
	audit, e := s.ListAudit(ctx, 0, 200)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, a := range audit {
		if a.Action == "AUTH_LOGIN_FAILED" {
			found = true
		}
	}
	if !found {
		t.Fatal("authentication audit not visible")
	}
	target := volume(t, s)
	invalid := BackupProfile{Name: "bad", SourceID: "synthetic", RelativeSelection: "../escape", TargetVolumeID: target}
	if _, e = s.CreateBackupProfile(ctx, invalid, "admin"); e == nil {
		t.Fatal("unsafe selection accepted")
	}
	if _, e = s.CreateBackupProfile(ctx, BackupProfile{Name: "bad", SourceID: "synthetic", TargetVolumeID: target, Enabled: true, Revision: 3}, "admin"); e == nil {
		t.Fatal("client set create revision")
	}
}
