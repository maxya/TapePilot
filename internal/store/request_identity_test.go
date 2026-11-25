package store

import (
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/domain"
	"strings"
	"sync"
	"testing"
	"time"
)

func planIdentity(t *testing.T, plan, route, key string, revision int64) RequestIdentity {
	t.Helper()
	raw, err := json.Marshal(struct {
		PlanID   string `json:"plan_id"`
		Revision int64  `json:"revision"`
	}{plan, revision})
	if err != nil {
		t.Fatal(err)
	}
	return RequestIdentity{Actor: "admin", Route: route, Key: key, Request: raw}
}
func savePlan(t *testing.T, s *Store, p HTTPPlan) HTTPPlan {
	t.Helper()
	p.ID = uuid.NewString()
	p.ExpiresAt = time.Now().Add(time.Hour).UTC().Format(catalogInstantLayout)
	p.Preview = json.RawMessage(`{}`)
	if err := s.SaveHTTPPlan(ctx, p); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestRequestIdentityRejectsChangedPlanWithIdenticalExecutionOptions(t *testing.T) {
	for _, kind := range []string{"BACKUP", "RESTORE"} {
		t.Run(kind, func(t *testing.T) {
			s, dir := openTest(t)
			p := profile(t, s)
			plan := HTTPPlan{Kind: kind, VolumeID: p.TargetVolumeID, Options: json.RawMessage(`{"archive_id":"synthetic","restore_root_id":"out","destination_slug":"fresh","all":true}`)}
			profileID := ""
			revision := int64(0)
			route := "POST /api/v1/restores/run"
			if kind == "BACKUP" {
				profileID = p.ID
				revision = p.Revision
				route = "POST /api/v1/backup-profiles/" + p.ID + "/run"
				plan.ProfileID = p.ID
				plan.Revision = p.Revision
				plan.Options = json.RawMessage(`"` + strings.Repeat("a", 64) + `"`)
			}
			first := savePlan(t, s, plan)
			second := savePlan(t, s, plan)
			key := uuid.NewString()
			identity := planIdentity(t, first.ID, route, key, revision)
			accepted, err := s.EnqueueHTTPPlan(ctx, identity, first.ID, profileID, revision)
			if err != nil {
				t.Fatal(err)
			}
			changed := planIdentity(t, second.ID, route, key, revision)
			if _, err = s.EnqueueHTTPPlan(ctx, changed, second.ID, profileID, revision); !errors.Is(err, ErrConflict) {
				t.Fatal("changed plan accepted", err)
			}
			// Successful retries replay the original accepted response after expiry,
			// deletion, profile edits and job execution state changes.
			if _, err = s.db.Exec("DELETE FROM http_plans WHERE id=?", first.ID); err != nil {
				t.Fatal(err)
			}
			if err = s.RequestCancel(ctx, accepted.ID); err != nil {
				t.Fatal(err)
			}
			if kind == "BACKUP" {
				p.Name = "new profile revision"
				if _, err = s.UpdateBackupProfile(ctx, p, "admin"); err != nil {
					t.Fatal(err)
				}
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(ctx, dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			s = reopened

			replayed, err := s.EnqueueHTTPPlan(ctx, identity, first.ID, profileID, revision)
			if err != nil || replayed.ID != accepted.ID || replayed.State != domain.Queued {
				t.Fatal("accepted result did not replay", replayed, err)
			}
			jobs, err := s.ListJobs(ctx, 500)
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, j := range jobs {
				if j.Kind == domain.Kind(kind) {
					count++
				}
			}
			if count != 1 {
				t.Fatal("extra job created", count)
			}
		})
	}
}
func TestRequestIdentityConcurrentAtomicReplayAndRouteScope(t *testing.T) {
	s, _ := openTest(t)
	identity := RequestIdentity{Actor: "admin", Route: "POST /api/v1/drives/simulator/inspect", Key: uuid.NewString(), Request: json.RawMessage(`{}`)}
	var wg sync.WaitGroup
	ids := make(chan string, 16)
	for range 16 {
		wg.Go(func() {
			j, err := s.EnqueueHTTPRequest(ctx, identity, Job{Kind: domain.Inspect})
			if err != nil {
				t.Error(err)
				return
			}
			ids <- j.ID
		})
	}
	wg.Wait()
	close(ids)
	var expected string
	for id := range ids {
		if expected == "" {
			expected = id
		}
		if id != expected {
			t.Fatal("concurrent replay made another job")
		}
	}
	changed := identity
	changed.Request = json.RawMessage(`{"changed":true}`)
	if _, err := s.EnqueueHTTPRequest(ctx, changed, Job{Kind: domain.Inspect}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	anotherRoute := identity
	anotherRoute.Route = "POST /api/v1/drives/simulator/rewind"
	other, err := s.EnqueueHTTPRequest(ctx, anotherRoute, Job{Kind: domain.Rewind})
	if err != nil || other.ID == expected {
		t.Fatal(other, err)
	}
	var requests, jobs int
	if err = s.db.QueryRow("SELECT COUNT(*) FROM http_request_results").Scan(&requests); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("SELECT COUNT(*) FROM jobs WHERE kind IN('INSPECT','REWIND')").Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || jobs != 2 {
		t.Fatal(requests, jobs)
	}
}

func TestEarlierExecutionDerivedKeysFailClosed(t *testing.T) {
	s, _ := openTest(t)
	p := profile(t, s)
	key := uuid.NewString()
	if _, err := s.Enqueue(ctx, Job{Kind: domain.Inspect, DedupeKey: "http:" + key}); err != nil {
		t.Fatal(err)
	}
	i := RequestIdentity{Actor: "admin", Route: "POST /api/v1/drives/simulator/inspect", Key: key, Request: json.RawMessage(`{}`)}
	if _, err := s.EnqueueHTTPRequest(ctx, i, Job{Kind: domain.Inspect}); !errors.Is(err, ErrConflict) {
		t.Fatal("old key silently created a second operation", err)
	}
	plan := savePlan(t, s, HTTPPlan{Kind: "BACKUP", ProfileID: p.ID, VolumeID: p.TargetVolumeID, Revision: p.Revision, Options: json.RawMessage(`"` + strings.Repeat("a", 64) + `"`)})
	oldKey := uuid.NewString()
	if _, err := s.EnqueueManualProfile(ctx, p.ID, p.Revision, oldKey, strings.Repeat("a", 64), "admin"); err != nil {
		t.Fatal(err)
	}
	identity := planIdentity(t, plan.ID, "POST /api/v1/backup-profiles/"+p.ID+"/run", oldKey, p.Revision)
	if _, err := s.EnqueueHTTPPlan(ctx, identity, plan.ID, p.ID, p.Revision); !errors.Is(err, ErrConflict) {
		t.Fatal("ambiguous old backup request adopted a plan identity", err)
	}
}

func TestHTTPRequestIdentityFailureRollsBackJobAndAudit(t *testing.T) {
	s, _ := openTest(t)
	if _, err := s.db.Exec("CREATE TRIGGER reject_request_identity BEFORE INSERT ON http_request_results BEGIN SELECT RAISE(ABORT,'injected persistence failure'); END"); err != nil {
		t.Fatal(err)
	}
	identity := RequestIdentity{Actor: "admin", Route: "POST /api/v1/drives/simulator/inspect", Key: uuid.NewString(), Request: json.RawMessage(`{}`)}
	if _, err := s.EnqueueHTTPRequest(ctx, identity, Job{Kind: domain.Inspect}); err == nil {
		t.Fatal("request persistence failure swallowed")
	}
	for _, table := range []string{"jobs", "job_events", "audit_events", "http_request_results"} {
		var count int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatal("partial request transaction", table, count, err)
		}
	}
	if _, err := s.db.Exec("DROP TRIGGER reject_request_identity"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueHTTPRequest(ctx, identity, Job{Kind: domain.Inspect}); err != nil {
		t.Fatal(err)
	}
}
