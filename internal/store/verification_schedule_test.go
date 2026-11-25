package store

import (
	"errors"
	"github.com/maxya/TapePilot/internal/domain"
	"testing"
	"time"
)

func TestVerifyScheduleIndependentOfSourceAndEnrollment(t *testing.T) {
	s, _ := openTest(t)
	p := profile(t, s)
	scheduled, err := s.CreateSchedule(ctx, Schedule{ProfileID: p.ID, Operation: domain.Verify, Cadence: "daily", Timezone: "UTC", LocalTime: "12:00", Enabled: true}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(48 * time.Hour)
	due := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	if due.After(now) {
		due = due.Add(-24 * time.Hour)
	}
	slot := due.Format("2006-01-02T15:04")
	if err = s.SetVolumeState(ctx, p.TargetVolumeID, domain.Sealed); err != nil {
		t.Fatal(err)
	}
	j, created, err := s.FireSchedule(ctx, scheduled.ID, scheduled.Revision, slot, due, now, false)
	if err != nil || !created || j.Kind != domain.Verify || j.VolumeID != p.TargetVolumeID || string(j.Options) != "{}" {
		t.Fatal(j, created, err)
	}
	again, created, err := s.FireSchedule(ctx, scheduled.ID, scheduled.Revision, slot, due, now, false)
	if err != nil || created || again.ID != "" {
		t.Fatal("slot replay", again, created, err)
	}
	if _, err = s.EnqueueProfile(ctx, p.ID, p.Revision, "admin"); !errors.Is(err, ErrOutstanding) {
		t.Fatal("verify did not occupy shared profile slot", err)
	}
	if err = s.RequestCancel(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	changed := scheduled
	changed.Operation = domain.Backup
	updated, err := s.UpdateSchedule(ctx, changed, "admin")
	if err != nil || updated.Operation != domain.Backup {
		t.Fatal(updated, err)
	}
	if _, err = s.UpdateSchedule(ctx, changed, "admin"); !errors.Is(err, ErrConflict) {
		t.Fatal("stale schedule update accepted", err)
	}
}
func TestScheduleOperationDefaultAndInvalidKinds(t *testing.T) {
	s, _ := openTest(t)
	p := profile(t, s)
	v, err := s.CreateSchedule(ctx, Schedule{ProfileID: p.ID, Cadence: "weekly", Timezone: "UTC", LocalTime: "12:00", Weekday: 1}, "admin")
	if err != nil || v.Operation != domain.Backup {
		t.Fatal(v, err)
	}
	loaded, err := s.Schedule(ctx, v.ID)
	if err != nil || loaded.Operation != domain.Backup {
		t.Fatal(loaded, err)
	}
	v.ID = ""
	v.Revision = 0
	v.CreatedAt = ""
	v.UpdatedAt = ""
	v.Operation = domain.Initialize
	if _, err = s.CreateSchedule(ctx, v, "admin"); !errors.Is(err, ErrOperations) {
		t.Fatal("unsafe scheduled operation", err)
	}
}
