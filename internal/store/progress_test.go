package store

import (
	"errors"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/progress"
	"os"
	"testing"
	"time"
)

func TestProgressPersistsWithoutChangingJobState(t *testing.T) {
	s, _ := openTest(t)
	j, e := s.Enqueue(ctx, Job{Kind: domain.Inspect})
	if e != nil {
		t.Fatal(e)
	}
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	p := progress.Snapshot{JobID: j.ID, Phase: progress.Streaming, SampledAt: now, StartedAt: now}
	if e = s.SaveProgress(ctx, p); e != nil {
		t.Fatal(e)
	}
	total := int64(1048576)
	percent := 25.0
	eta := 1.5
	p.PhaseTotalBytes = &total
	p.EstimatedPercent = &percent
	p.ETASeconds = &eta
	p.SampledAt = now.Add(500 * time.Millisecond)
	p.AcceptedBytes = 262144
	p.ElapsedSeconds = .5
	if e = s.SaveProgress(ctx, p); e != nil {
		t.Fatal(e)
	}
	got, e := s.Progress(ctx, j.ID)
	if e != nil || got.AcceptedBytes != p.AcceptedBytes || !got.SampledAt.Equal(p.SampledAt) || got.PhaseTotalBytes == nil || *got.PhaseTotalBytes != total || got.EstimatedPercent == nil || *got.EstimatedPercent != percent || got.ETASeconds == nil || *got.ETASeconds != eta {
		t.Fatalf("%#v %v", got, e)
	}
	stale := p
	stale.SampledAt = now
	if !errors.Is(s.SaveProgress(ctx, stale), ErrConflict) {
		t.Fatal("stale sample overwrote latest")
	}
	stale = p
	stale.AcceptedBytes = 0
	stale.SampledAt = p.SampledAt.Add(time.Second)
	if !errors.Is(s.SaveProgress(ctx, stale), ErrConflict) {
		t.Fatal("counter regressed")
	}
	job, e := s.GetJob(ctx, j.ID)
	if e != nil || job.State != domain.Queued {
		t.Fatal("progress asserted job completion")
	}
	var count int
	if e = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM job_progress WHERE job_id=?", j.ID).Scan(&count); e != nil || count != 1 {
		t.Fatal("unbounded stored samples", e)
	}
}

func TestProgressSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	s, e := Open(ctx, dir)
	if e != nil {
		t.Fatal(e)
	}
	j, e := s.Enqueue(ctx, Job{Kind: domain.Inspect})
	if e != nil {
		s.Close()
		t.Fatal(e)
	}
	now := time.Now().UTC()
	sample := progress.Snapshot{JobID: j.ID, Phase: progress.IdentityCheck, SampledAt: now, StartedAt: now}
	if e = s.SaveProgress(ctx, sample); e != nil {
		s.Close()
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	reopened, e := Open(ctx, dir)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	actual, e := reopened.Progress(ctx, j.ID)
	if e != nil || actual.JobID != j.ID || actual.Phase != sample.Phase || !actual.StartedAt.Equal(sample.StartedAt) {
		t.Fatalf("%#v %v", actual, e)
	}
}
