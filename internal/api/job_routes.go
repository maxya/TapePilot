package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/restore"
	"github.com/maxya/TapePilot/internal/source"
	"github.com/maxya/TapePilot/internal/store"
	"math"
	"net/http"
	"path/filepath"
	"time"
)

func (c *console) jobRoutes(m *http.ServeMux) {
	route(m, "GET /api/v1/jobs", 200, func(_ http.ResponseWriter, r *http.Request) (any, error) {
		return c.r.Store.FilterJobs(r.Context(), domain.State(r.URL.Query().Get("state")), 100)
	})
	route(m, "GET /api/v1/jobs/{id}", 200, func(_ http.ResponseWriter, r *http.Request) (any, error) {
		return c.r.Store.GetJob(r.Context(), r.PathValue("id"))
	})
	route(m, "GET /api/v1/jobs/{id}/events", 200, func(_ http.ResponseWriter, r *http.Request) (any, error) {
		after, e := pageNumber(r.URL.Query().Get("after"), 0, math.MaxInt64)
		if e != nil {
			return nil, e
		}
		return c.r.Store.Events(r.Context(), r.PathValue("id"), after, 100)
	})
	for _, op := range []string{"cancel", "recheck-media"} {
		route(m, "POST /api/v1/jobs/{id}/"+op, 200, c.control)
	}
	m.HandleFunc("GET /api/v1/jobs/{id}/stream", c.stream)
	route(m, "POST /api/v1/restores/plan", 201, c.plan)
	route(m, "POST /api/v1/backup-profiles/{id}/plan", 201, c.plan)
	route(m, "GET /api/v1/plans/{id}", 200, func(_ http.ResponseWriter, r *http.Request) (any, error) {
		p, e := c.r.Store.HTTPPlan(r.Context(), r.PathValue("id"))
		return p.Preview, e
	})
	route(m, "POST /api/v1/restores/run", 202, c.runPlan)
	route(m, "POST /api/v1/backup-profiles/{id}/run", 202, c.runPlan)
}
func (c *console) control(w http.ResponseWriter, r *http.Request) (any, error) {
	if body(w, r, &struct{}{}) != nil {
		return nil, ErrInput
	}
	id := r.PathValue("id")
	op := filepath.Base(r.URL.Path)
	if err := c.r.Store.Audit(r.Context(), "JOB_CONTROL", id); err != nil {
		return nil, err
	}
	var err error
	if op == "cancel" {
		err = c.svc.Scheduler.Cancel(r.Context(), id)
	} else {
		err = c.svc.Scheduler.Recheck(r.Context(), id)
	}
	if err != nil {
		return nil, err
	}
	return c.r.Store.GetJob(r.Context(), id)
}
func (c *console) plan(w http.ResponseWriter, r *http.Request) (any, error) {
	select {
	case c.expensive <- struct{}{}:
		defer func() { <-c.expensive }()
	default:
		return nil, store.ErrConflict
	}
	ctx := r.Context()
	p := store.HTTPPlan{ID: uuid.NewString(), ExpiresAt: time.Now().UTC().Add(15 * time.Minute).Format("2006-01-02T15:04:05.000000000Z")}
	preview := map[string]any{}
	if id := r.PathValue("id"); id != "" {
		var req struct {
			Revision int64 `json:"revision"`
		}
		if err := body(w, r, &req); err != nil {
			return nil, err
		}
		profile, err := c.r.Store.BackupProfile(ctx, id)
		if err != nil {
			return nil, err
		}
		if !profile.Enabled || req.Revision != 0 && profile.Revision != req.Revision {
			return nil, store.ErrConflict
		}
		plan, err := source.Create(ctx, c.r.Sources, profile.SourceID, profile.RelativeSelection, c.r.Spool, profile.Exclusions, source.DefaultLimits())
		if err != nil {
			return nil, err
		}
		digest, err := plan.Digest()
		if err = errors.Join(err, plan.Close()); err != nil {
			return nil, err
		}
		p.Kind = "BACKUP"
		p.ProfileID = id
		p.VolumeID = profile.TargetVolumeID
		p.Revision = profile.Revision
		p.Options, err = json.Marshal(digest)
		if err != nil {
			return nil, err
		}
		preview = map[string]any{"revision": p.Revision, "entries": plan.Count, "logical_bytes": plan.LogicalBytes, "estimated_bytes": plan.EstimatedBytes, "volume_id": p.VolumeID, "sparse_files": plan.SparseFiles}
	} else {
		var req restore.Options
		if err := body(w, r, &req); err != nil {
			return nil, err
		}
		plan, err := c.svc.Restore.Preview(ctx, req)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(plan)
		if err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &preview); err != nil {
			return nil, err
		}
		p.Options, err = json.Marshal(req)
		if err != nil {
			return nil, err
		}
		p.Kind = "RESTORE"
		p.VolumeID = plan.VolumeID
	}
	preview["id"] = p.ID
	preview["expires_at"] = p.ExpiresAt
	encoded, err := json.Marshal(preview)
	if err != nil {
		return nil, err
	}
	p.Preview = encoded
	if err = c.r.Store.SaveHTTPPlan(ctx, p); err != nil {
		return nil, err
	}
	return p.Preview, nil
}
func (c *console) runPlan(w http.ResponseWriter, r *http.Request) (any, error) {
	var req struct {
		PlanID   string `json:"plan_id"`
		Revision int64  `json:"revision"`
	}
	if body(w, r, &req) != nil {
		return nil, ErrInput
	}
	key := r.Header.Get("Idempotency-Key")
	if _, err := uuid.Parse(key); err != nil {
		return nil, ErrInput
	}
	if _, err := uuid.Parse(req.PlanID); err != nil {
		return nil, ErrInput
	}
	normalized, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	identity := store.RequestIdentity{Actor: "admin", Route: r.Method + " " + r.URL.Path, Key: key, Request: normalized}
	j, err := c.r.Store.EnqueueHTTPPlan(r.Context(), identity, req.PlanID, r.PathValue("id"), req.Revision)
	if err == nil {
		c.svc.Scheduler.Wake()
	}
	return j, err
}
func (c *console) stream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	after, e := pageNumber(r.Header.Get("Last-Event-ID"), 0, math.MaxInt64)
	if e != nil {
		fail(w, r, e)
		return
	}
	select {
	case c.streams <- struct{}{}:
		defer func() { <-c.streams }()
	default:
		fail(w, r, store.ErrConflict)
		return
	}
	if _, e = c.r.Store.GetJob(r.Context(), id); e != nil {
		fail(w, r, e)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		if !c.auth.ValidSession(ctx, r) {
			return
		}
		controller := http.NewResponseController(w)
		if e = controller.SetWriteDeadline(time.Now().Add(4 * time.Second)); e != nil && !errors.Is(e, http.ErrNotSupported) {
			return
		}
		j, e := c.r.Store.GetJob(ctx, id)
		if e != nil {
			return
		}
		snap := map[string]any{"job": j, "progress": nil}
		if p, e := c.r.Store.Progress(ctx, id); e == nil {
			snap["progress"] = p
		} else if !errors.Is(e, store.ErrNotFound) {
			return
		}
		if j.Kind == domain.Restore && j.State == domain.Succeeded {
			p, e := c.r.Store.RestorePublication(ctx, id)
			if e != nil {
				return
			}
			snap["publication"] = p
		}
		events, e := c.r.Store.Events(ctx, id, after, 100)
		if e != nil {
			return
		}
		for _, v := range events {
			raw, e := json.Marshal(v)
			if e != nil {
				return
			}
			if _, e = fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", v.ID, raw); e != nil {
				return
			}
			after = v.ID
		}
		raw, e := json.Marshal(snap)
		if e != nil {
			return
		}
		if _, e = fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", raw); e != nil {
			return
		}
		if e = controller.Flush(); e != nil {
			return
		}
		if len(events) < 100 {
			switch j.State {
			case domain.Succeeded, domain.Cancelled, domain.Failed, domain.RecoveryRequired:
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
