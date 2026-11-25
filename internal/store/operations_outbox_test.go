package store

import (
	"errors"
	"github.com/maxya/TapePilot/internal/domain"
	"testing"
	"time"
)

func failedOutboxJob(t *testing.T, s *Store) Job {
	t.Helper()
	j, e := s.Enqueue(ctx, Job{Kind: domain.Inspect})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Claim(ctx); e != nil {
		t.Fatal(e)
	}
	if e = s.FinishExecution(ctx, j.ID, errors.New("synthetic failure")); e != nil {
		t.Fatal(e)
	}
	return j
}
func TestSMTPOptInAtEventTimeAndDisableSuppressesPending(t *testing.T) {
	s, _ := openTest(t)
	failedOutboxJob(t, s)
	if e := s.ConfigureSMTP(ctx, true); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ClaimOutbox(ctx, time.Now().UTC().Add(time.Hour)); !errors.Is(e, ErrNotFound) {
		t.Fatal("historical disabled event queued", e)
	}
	failedOutboxJob(t, s)
	message, e := s.ClaimOutbox(ctx, time.Now().UTC().Add(time.Hour))
	if e != nil {
		t.Fatal(e)
	}
	if e = s.FinishOutbox(ctx, message, false, false, "SMTP_NETWORK_FAILED", time.Now().UTC().Add(time.Minute)); e != nil {
		t.Fatal(e)
	}
	if e = s.ConfigureSMTP(ctx, false); e != nil {
		t.Fatal(e)
	}
	if e = s.ConfigureSMTP(ctx, true); e != nil {
		t.Fatal(e)
	}
	if _, e = s.ClaimOutbox(ctx, time.Now().UTC().Add(24*time.Hour)); !errors.Is(e, ErrNotFound) {
		t.Fatal("disabled pending notification replayed", e)
	}
}
func TestSMTPRetriesBoundedAndDeliveryDoesNotChangeJob(t *testing.T) {
	s, _ := openTest(t)
	if e := s.ConfigureSMTP(ctx, true); e != nil {
		t.Fatal(e)
	}
	j := failedOutboxJob(t, s)
	now := time.Now().UTC().Add(time.Hour)
	for attempt := 1; attempt <= 5; attempt++ {
		message, e := s.ClaimOutbox(ctx, now)
		if e != nil || message.Attempts != attempt {
			t.Fatal(message, e)
		}
		if e = s.FinishOutbox(ctx, message, false, false, "SMTP_NETWORK_FAILED", now.Add(time.Minute)); e != nil {
			t.Fatal(e)
		}
		now = now.Add(time.Hour)
	}
	if _, e := s.ClaimOutbox(ctx, now); !errors.Is(e, ErrNotFound) {
		t.Fatal("retry limit bypassed", e)
	}
	actual, e := s.GetJob(ctx, j.ID)
	if e != nil || actual.State != domain.Failed {
		t.Fatal("notification altered job outcome", actual, e)
	}
	notifications, e := s.ListNotifications(ctx, 0, 200)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, n := range notifications {
		if n.Code == "SMTP_DELIVERY_FAILED" {
			found = true
		}
	}
	if !found {
		t.Fatal("exhausted delivery not surfaced")
	}
}
func TestSMTPPendingQueueCapacityIsVisible(t *testing.T) {
	s, _ := openTest(t)
	if e := s.ConfigureSMTP(ctx, true); e != nil {
		t.Fatal(e)
	}
	j := failedOutboxJob(t, s)
	_, e := s.db.ExecContext(ctx, `WITH RECURSIVE numbers(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM numbers WHERE n<9999)
 INSERT INTO notification_outbox(job_id,code,created_at,next_attempt_at)
 SELECT ?,printf('SYNTHETIC_%d',n),?,? FROM numbers`, j.ID, stamp(), stamp())
	if e != nil {
		t.Fatal(e)
	}
	failedOutboxJob(t, s)
	var count int
	if e = s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM notification_outbox WHERE delivered_at IS NULL AND permanent_failure=0").Scan(&count); e != nil || count != 10000 {
		t.Fatal("outbox capacity exceeded", count, e)
	}
	notifications, e := s.ListNotifications(ctx, 0, 200)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, n := range notifications {
		if n.Code == "SMTP_QUEUE_FULL" {
			found = true
		}
	}
	if !found {
		t.Fatal("queue overflow silent")
	}
}
