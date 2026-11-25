package jobs

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/store"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

type executorFunc func(context.Context, store.Job) error

func (f executorFunc) Execute(ctx context.Context, j store.Job) error { return f(ctx, j) }
func testStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	s, err := store.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func enqueue(t *testing.T, s *store.Store, kind domain.Kind, volume string) store.Job {
	t.Helper()
	j, err := s.Enqueue(context.Background(), store.Job{Kind: kind, VolumeID: volume})
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func get(t *testing.T, s *store.Store, id string) store.Job {
	t.Helper()
	j, err := s.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return j
}
func wait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}
func TestMediaWaitDoesNotBlockEligibleJob(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	first := enqueue(t, s, domain.Inspect, "")
	second := enqueue(t, s, domain.Inspect, "")
	var loaded atomic.Bool
	executor := executorFunc(func(ctx context.Context, j store.Job) error {
		if j.ID == first.ID && !loaded.Load() {
			return store.ErrWaitingMedia
		}
		j, err := s.Transition(ctx, j.ID, j.Version, domain.Running, "inspect", "")
		if err != nil {
			return err
		}
		_, err = s.Transition(ctx, j.ID, j.Version, domain.Succeeded, "inspect", "")
		return err
	})
	scheduler := New(s, executor)
	for range 2 {
		if ran, err := scheduler.RunOne(ctx); err != nil || !ran {
			t.Fatal(ran, err)
		}
	}
	if j := get(t, s, first.ID); j.State != domain.WaitingMedia {
		t.Fatal(j)
	}
	if j := get(t, s, second.ID); j.State != domain.Succeeded {
		t.Fatal(j)
	}
	if ran, err := scheduler.RunOne(ctx); err != nil || ran {
		t.Fatal("waiting job repeated", ran, err)
	}
	loaded.Store(true)
	if err := scheduler.Recheck(ctx, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.RunOne(ctx); err != nil {
		t.Fatal(err)
	}
	if j := get(t, s, first.ID); j.State != domain.Succeeded {
		t.Fatal(j)
	}
}
func TestCancellationRetainsSlotUntilResourcesClose(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	j := enqueue(t, s, domain.Inspect, "")
	enqueue(t, s, domain.Inspect, "")
	started, closed, finish := make(chan struct{}), make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	scheduler := New(s, executorFunc(func(ctx context.Context, j store.Job) error {
		j, err := s.Transition(ctx, j.ID, j.Version, domain.Running, "inspect", "")
		if err != nil {
			return err
		}
		close(started)
		<-ctx.Done()
		close(closed)
		<-finish
		return ctx.Err()
	}))
	go func() { _, err := scheduler.RunOne(ctx); done <- err }()
	wait(t, started)
	if err := scheduler.Cancel(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	wait(t, closed)
	if got := get(t, s, j.ID); got.State != domain.CancelRequested {
		t.Fatal(got)
	}
	if _, err := s.Claim(ctx); !errors.Is(err, store.ErrConflict) {
		t.Fatal("claim during blocked cancellation", err)
	}
	close(finish)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, j.ID); got.State != domain.Cancelled {
		t.Fatal(got)
	}
}
func TestQueuedCancelNeverExecutesAndNilWithoutEvidenceFails(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	j := enqueue(t, s, domain.Inspect, "")
	var calls int
	scheduler := New(s, executorFunc(func(context.Context, store.Job) error { calls++; return nil }))
	if err := scheduler.Cancel(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if ran, err := scheduler.RunOne(ctx); ran || err != nil || calls != 0 {
		t.Fatal(ran, err, calls)
	}
	second := enqueue(t, s, domain.Inspect, "")
	scheduler.RunOne(ctx)
	if got := get(t, s, second.ID); got.State != domain.Failed || got.ErrorCode != "MISSING_COMPLETION_EVIDENCE" {
		t.Fatal(got)
	}
}
func TestShutdownCancelsAndWaits(t *testing.T) {
	s := testStore(t)
	j := enqueue(t, s, domain.Inspect, "")
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	scheduler := New(s, executorFunc(func(ctx context.Context, j store.Job) error {
		close(started)
		<-ctx.Done()
		<-release
		return ctx.Err()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	go func() { done <- scheduler.Run(ctx) }()
	wait(t, started)
	cancel()
	select {
	case err := <-done:
		t.Fatal("scheduler returned with executor live", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, j.ID); got.State != domain.Cancelled {
		t.Fatal(got)
	}
}
func TestCancelWriteQuarantines(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := uuid.NewString()
	if err := s.CreateVolume(ctx, store.Volume{ID: id, Kind: "MANAGED", State: domain.BlankCandidate}); err != nil {
		t.Fatal(err)
	}
	j := enqueue(t, s, domain.Initialize, id)
	started := make(chan struct{})
	done := make(chan error, 1)
	scheduler := New(s, executorFunc(func(ctx context.Context, j store.Job) error {
		j, err := s.Transition(ctx, j.ID, j.Version, domain.Running, "initializing", "")
		if err != nil {
			return err
		}
		if err = s.BeginWriteIntent(ctx, store.WriteIntent{JobID: j.ID, VolumeID: id, DriveID: "sim"}); err != nil {
			return err
		}
		if err = s.MarkWriteOpened(ctx, j.ID); err != nil {
			return err
		}
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}))
	go func() { _, err := scheduler.RunOne(ctx); done <- err }()
	wait(t, started)
	if err := scheduler.Cancel(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := get(t, s, j.ID); got.State != domain.RecoveryRequired {
		t.Fatal(got)
	}
	v, err := s.GetVolume(ctx, id)
	if err != nil || v.State != domain.Quarantined {
		t.Fatal(v, err)
	}
}
