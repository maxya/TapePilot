package schedule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/engine"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/maxya/TapePilot/internal/app"
	"github.com/maxya/TapePilot/internal/config"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/media"
	"github.com/maxya/TapePilot/internal/store"
)

func scheduledFixture(t *testing.T) (*app.Runtime, store.Schedule, string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	db, e := store.Open(ctx, dir)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { db.Close() })
	var label media.Volume
	raw, e := os.ReadFile("../../schemas/fixtures/volume.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &label); e != nil {
		t.Fatal(e)
	}
	header, e := media.EncodeVolume(label)
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(header)
	job, e := db.Enqueue(ctx, store.Job{Kind: domain.Import})
	if e != nil {
		t.Fatal(e)
	}
	job, e = db.Claim(ctx)
	if e != nil {
		t.Fatal(e)
	}
	job, e = db.Transition(ctx, job.ID, job.Version, domain.Running, "header_import", "")
	if e != nil {
		t.Fatal(e)
	}
	if e = db.RegisterImportedVolume(ctx, job.ID, store.Volume{ID: label.VolumeID, Kind: "MANAGED", Label: label.LabelAtInitialization, State: domain.Managed, HeaderSHA256: hex.EncodeToString(sum[:])}, raw); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Transition(ctx, job.ID, job.Version, domain.Succeeded, "header_complete", ""); e != nil {
		t.Fatal(e)
	}
	if e = db.EnrollVolume(ctx, store.Enrollment{VolumeID: label.VolumeID, DriveID: "synthetic-drive", Profile: "simulator", OperatorRef: "synthetic operator"}); e != nil {
		t.Fatal(e)
	}
	profile, e := db.CreateBackupProfile(ctx, store.BackupProfile{Name: "Scheduled fixture", SourceID: "synthetic", TargetVolumeID: label.VolumeID, Enabled: true}, "admin")
	if e != nil {
		t.Fatal(e)
	}
	scheduled, e := db.CreateSchedule(ctx, store.Schedule{ProfileID: profile.ID, Cadence: "daily", Timezone: "UTC", LocalTime: "12:00", Enabled: true}, "admin")
	if e != nil {
		t.Fatal(e)
	}
	return &app.Runtime{Store: db, Settings: config.Settings{Sources: map[string]string{"synthetic": t.TempDir()}}}, scheduled, dir
}
func TestTickConcurrentIdempotencyAndRestart(t *testing.T) {
	ctx := context.Background()
	runtime, scheduled, dir := scheduledFixture(t)
	now := time.Now().UTC().Add(48 * time.Hour)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wakes := 0
	wake := func() { mu.Lock(); wakes++; mu.Unlock() }
	for n := 0; n < 6; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := Tick(ctx, runtime, now, wake); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if wakes != 1 {
		t.Fatal("scheduled slot duplicated", wakes)
	}
	jobs, e := runtime.Store.ListJobs(ctx, 20)
	if e != nil {
		t.Fatal(e)
	}
	queued := 0
	var firstID string
	for _, j := range jobs {
		if j.Kind == domain.Backup && j.State == domain.Queued {
			queued++
			firstID = j.ID
		}
	}
	if queued != 1 {
		t.Fatal("unexpected queued profiles", queued)
	}
	if e = runtime.Store.Close(); e != nil {
		t.Fatal(e)
	}
	reopened, e := store.Open(ctx, dir)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	runtime.Store = reopened
	if e = Tick(ctx, runtime, now, wake); e != nil {
		t.Fatal(e)
	}
	if wakes != 1 {
		t.Fatal("restart replayed same slot")
	}
	if e = runtime.Store.RequestCancel(ctx, firstID); e != nil {
		t.Fatal(e)
	}
	if e = Tick(ctx, runtime, now.Add(8*24*time.Hour), wake); e != nil {
		t.Fatal(e)
	}
	if wakes != 2 {
		t.Fatal("missed slots did not coalesce to one latest job", wakes)
	}
	actual, e := runtime.Store.Schedule(ctx, scheduled.ID)
	if e != nil || actual.LastSlot == "" {
		t.Fatal("slot not durable", e)
	}
}
func TestTickMissingSourceCreatesActionableInbox(t *testing.T) {
	ctx := context.Background()
	runtime, _, _ := scheduledFixture(t)
	runtime.Settings.Sources = map[string]string{}
	if e := Tick(ctx, runtime, time.Now().UTC().Add(48*time.Hour), nil); e != nil {
		t.Fatal(e)
	}
	jobs, e := runtime.Store.ListJobs(ctx, 20)
	if e != nil {
		t.Fatal(e)
	}
	for _, j := range jobs {
		if j.Kind == domain.Backup {
			t.Fatal("missing source enqueued")
		}
	}
	notifications, e := runtime.Store.ListNotifications(ctx, 0, 200)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, n := range notifications {
		if n.Code == "ACTION_REQUIRED" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing source has no actionable event")
	}
}

func TestScheduledVerifyWithoutSourceOrEnrollmentPerformsOnlyReadback(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	c := config.Default()
	c.StateDir = dir
	sourceRoot := t.TempDir()
	if err := os.WriteFile(sourceRoot+"/archive-me.txt", []byte("synthetic scheduled readback"), 0600); err != nil {
		t.Fatal(err)
	}
	c.Sources = map[string]string{"historical-source": sourceRoot}
	c.SpoolReserveBytes = 0
	runtime, err := app.Open(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Store.Close()
	if err = runtime.Drive.Create("verify-fixture", 64*262144, false); err != nil {
		t.Fatal(err)
	}
	if err = runtime.Insert("verify-fixture"); err != nil {
		t.Fatal(err)
	}
	lease, err := runtime.Drive.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before, err := lease.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
	volume := uuid.NewString()
	if err = runtime.Store.CreateVolume(ctx, store.Volume{ID: volume, Kind: "MANAGED", Label: "Synthetic verify target", Generation: "SIMULATED", State: domain.BlankCandidate}); err != nil {
		t.Fatal(err)
	}
	options, _ := json.Marshal(engine.InitOptions{CartridgeID: "verify-fixture", Generation: before.Generation, Confirmed: true})
	initJob, err := runtime.Store.Enqueue(ctx, store.Job{Kind: domain.Initialize, VolumeID: volume, Options: options})
	if err != nil {
		t.Fatal(err)
	}
	services := runtime.Services()
	services.Engine.InitializeArmed = true
	services.Engine.InitializeJobID = initJob.ID
	if processed, err := services.Scheduler.RunOne(ctx); err != nil || !processed {
		t.Fatal(processed, err)
	}
	actual, err := runtime.Store.GetJob(ctx, initJob.ID)
	if err != nil || actual.State != domain.Succeeded {
		t.Fatal(actual, err)
	}
	services.Engine.InitializeArmed = false
	services.Engine.InitializeJobID = ""
	lease, err = runtime.Drive.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	observed, err := lease.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
	if err = runtime.Store.EnrollVolume(ctx, store.Enrollment{VolumeID: volume, DriveID: observed.Drive, Profile: observed.Profile, OperatorRef: "synthetic test operator"}); err != nil {
		t.Fatal(err)
	}
	backupOptions, _ := json.Marshal(engine.BackupOptions{SourceID: "historical-source", SourceLabel: "Synthetic historical source"})
	backup, err := runtime.Store.Enqueue(ctx, store.Job{Kind: domain.Backup, VolumeID: volume, Options: backupOptions})
	if err != nil {
		t.Fatal(err)
	}
	if processed, err := services.Scheduler.RunOne(ctx); err != nil || !processed {
		t.Fatal(processed, err)
	}
	completedBackup, err := runtime.Store.GetJob(ctx, backup.ID)
	if err != nil || completedBackup.State != domain.Succeeded {
		t.Fatal(completedBackup, err)
	}
	if err = runtime.Store.RevokeEnrollment(ctx, volume, "synthetic test revocation"); err != nil {
		t.Fatal(err)
	}
	runtime.Settings.Sources = map[string]string{}
	runtime.Sources = nil
	if err = os.Remove(sourceRoot + "/archive-me.txt"); err != nil {
		t.Fatal(err)
	}

	lease, err = runtime.Drive.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	initialized, err := lease.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
	p, err := runtime.Store.CreateBackupProfile(ctx, store.BackupProfile{Name: "Verify historical target", SourceID: "historical-source", TargetVolumeID: volume, Enabled: true}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err = runtime.Store.CheckEnrollment(ctx, volume, "any-drive", "any-profile"); !errors.Is(err, store.ErrNotEnrolled) {
		t.Fatal("test target unexpectedly enrolled", err)
	}
	scheduled, err := runtime.Store.CreateSchedule(ctx, store.Schedule{ProfileID: p.ID, Operation: domain.Verify, Cadence: "daily", Timezone: "UTC", LocalTime: "12:00", Enabled: true}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(48 * time.Hour)
	if err = Tick(ctx, runtime, now, nil); err != nil {
		t.Fatal(err)
	}
	if processed, err := services.Scheduler.RunOne(ctx); err != nil || !processed {
		t.Fatal(processed, err)
	}
	all, err := runtime.Store.ListJobs(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	var verification store.Job
	for _, j := range all {
		if j.Kind == domain.Verify {
			verification = j
		}
	}
	if verification.ID == "" || verification.State != domain.Succeeded || verification.VolumeID != volume {
		t.Fatal(verification)
	}
	lease, err = runtime.Drive.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	after, err := lease.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lease.Close()
	if after.UsedBytes != initialized.UsedBytes || after.CompletedFiles != initialized.CompletedFiles || after.Uncertain {
		t.Fatal("verification changed media", initialized, after)
	}
	if err = Tick(ctx, runtime, now, nil); err != nil {
		t.Fatal(err)
	}
	loaded, err := runtime.Store.Schedule(ctx, scheduled.ID)
	if err != nil || loaded.LastSlot == "" {
		t.Fatal(loaded, err)
	}
	repeated, err := runtime.Store.ListJobs(ctx, 20)
	if err != nil || len(repeated) != len(all) {
		t.Fatal("verification slot replayed", repeated, err)
	}
}
