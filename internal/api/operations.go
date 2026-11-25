package api

import (
	"github.com/maxya/TapePilot/internal/app"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/store"
	"net/http"
	"strconv"
)

// RegisterOperations runs behind the server's admin session and CSRF middleware.
func RegisterOperations(mux *http.ServeMux, runtime *app.Runtime) {
	mux.HandleFunc("GET /api/v1/backup-profiles", func(w http.ResponseWriter, r *http.Request) {
		out, e := runtime.Store.ListBackupProfiles(r.Context())
		if e != nil {
			fail(w, r, e)
			return
		}
		reply(w, 200, out)
	})
	mux.HandleFunc("POST /api/v1/backup-profiles", func(w http.ResponseWriter, r *http.Request) {
		var p store.BackupProfile
		if e := body(w, r, &p); e != nil {
			fail(w, r, e)
			return
		}
		if _, ok := runtime.Settings.Sources[p.SourceID]; !ok {
			fail(w, r, ErrInput)
			return
		}
		out, e := runtime.Store.CreateBackupProfile(r.Context(), p, "admin")
		if e != nil {
			fail(w, r, e)
			return
		}
		reply(w, 201, out)
	})
	mux.HandleFunc("GET /api/v1/backup-profiles/{id}", func(w http.ResponseWriter, r *http.Request) {
		out, e := runtime.Store.BackupProfile(r.Context(), r.PathValue("id"))
		if e != nil {
			fail(w, r, e)
			return
		}
		reply(w, 200, out)
	})
	mux.HandleFunc("PATCH /api/v1/backup-profiles/{id}", func(w http.ResponseWriter, r *http.Request) {
		var change struct {
			Revision          int64     `json:"revision"`
			Name              *string   `json:"name"`
			SourceID          *string   `json:"source_id"`
			RelativeSelection *string   `json:"relative_selection"`
			Exclusions        *[]string `json:"exclusions"`
			TargetVolumeID    *string   `json:"target_volume_id"`
			Enabled           *bool     `json:"enabled"`
		}
		if e := body(w, r, &change); e != nil {
			fail(w, r, e)
			return
		}
		p, e := runtime.Store.BackupProfile(r.Context(), r.PathValue("id"))
		if e != nil {
			fail(w, r, e)
			return
		}
		p.Revision = change.Revision
		if change.Name != nil {
			p.Name = *change.Name
		}
		if change.SourceID != nil {
			p.SourceID = *change.SourceID
		}
		if change.RelativeSelection != nil {
			p.RelativeSelection = *change.RelativeSelection
		}
		if change.Exclusions != nil {
			p.Exclusions = *change.Exclusions
		}
		if change.TargetVolumeID != nil {
			p.TargetVolumeID = *change.TargetVolumeID
		}
		if change.Enabled != nil {
			p.Enabled = *change.Enabled
		}
		if _, ok := runtime.Settings.Sources[p.SourceID]; !ok {
			fail(w, r, ErrInput)
			return
		}
		out, e := runtime.Store.UpdateBackupProfile(r.Context(), p, "admin")
		if e != nil {
			fail(w, r, e)
			return
		}
		reply(w, 200, out)
	})
	mux.HandleFunc("GET /api/v1/schedules", func(w http.ResponseWriter, r *http.Request) {
		out, e := runtime.Store.ListSchedules(r.Context())
		if e != nil {
			fail(w, r, e)
			return
		}
		reply(w, 200, out)
	})
	mux.HandleFunc("POST /api/v1/schedules", func(w http.ResponseWriter, r *http.Request) {
		var p store.Schedule
		if e := body(w, r, &p); e != nil {
			fail(w, r, e)
			return
		}
		out, e := runtime.Store.CreateSchedule(r.Context(), p, "admin")
		if e != nil {
			fail(w, r, e)
			return
		}
		reply(w, 201, out)
	})
	mux.HandleFunc("GET /api/v1/schedules/{id}", func(w http.ResponseWriter, r *http.Request) {
		out, e := runtime.Store.Schedule(r.Context(), r.PathValue("id"))
		if e != nil {
			fail(w, r, e)
			return
		}
		reply(w, 200, out)
	})
	mux.HandleFunc("PATCH /api/v1/schedules/{id}", func(w http.ResponseWriter, r *http.Request) {
		var change struct {
			Operation *domain.Kind `json:"operation"`
			Revision  int64        `json:"revision"`
			ProfileID *string      `json:"profile_id"`
			Cadence   *string      `json:"cadence"`
			Timezone  *string      `json:"timezone"`
			LocalTime *string      `json:"local_time"`
			Weekday   *int         `json:"weekday"`
			Enabled   *bool        `json:"enabled"`
		}
		if e := body(w, r, &change); e != nil {
			fail(w, r, e)
			return
		}
		p, e := runtime.Store.Schedule(r.Context(), r.PathValue("id"))
		if e != nil {
			fail(w, r, e)
			return
		}
		p.Revision = change.Revision
		if change.Operation != nil {
			p.Operation = *change.Operation
		}
		if change.ProfileID != nil {
			p.ProfileID = *change.ProfileID
		}
		if change.Cadence != nil {
			p.Cadence = *change.Cadence
		}
		if change.Timezone != nil {
			p.Timezone = *change.Timezone
		}
		if change.LocalTime != nil {
			p.LocalTime = *change.LocalTime
		}
		if change.Weekday != nil {
			p.Weekday = *change.Weekday
		}
		if change.Enabled != nil {
			p.Enabled = *change.Enabled
		}
		out, e := runtime.Store.UpdateSchedule(r.Context(), p, "admin")
		if e != nil {
			fail(w, r, e)
			return
		}
		reply(w, 200, out)
	})
	mux.HandleFunc("GET /api/v1/notifications", func(w http.ResponseWriter, r *http.Request) {
		after, limit, e := operationCursor(r)
		if e != nil {
			fail(w, r, e)
			return
		}
		out, e := runtime.Store.ListNotifications(r.Context(), after, limit)
		if e != nil {
			fail(w, r, e)
			return
		}
		reply(w, 200, out)
	})
	mux.HandleFunc("POST /api/v1/notifications/{id}/read", func(w http.ResponseWriter, r *http.Request) {
		var empty struct{}
		if e := body(w, r, &empty); e != nil {
			fail(w, r, e)
			return
		}
		id, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if e != nil || id < 1 {
			fail(w, r, ErrInput)
			return
		}
		if e = runtime.Store.ReadNotification(r.Context(), id); e != nil {
			fail(w, r, e)
			return
		}
		reply(w, 200, map[string]bool{"read": true})
	})
	mux.HandleFunc("GET /api/v1/audit", func(w http.ResponseWriter, r *http.Request) {
		after, limit, e := operationCursor(r)
		if e != nil {
			fail(w, r, e)
			return
		}
		out, e := runtime.Store.ListAudit(r.Context(), after, limit)
		if e != nil {
			fail(w, r, e)
			return
		}
		reply(w, 200, out)
	})
}
func operationCursor(r *http.Request) (int64, int, error) {
	after := int64(0)
	limit := 100
	for name, values := range r.URL.Query() {
		if len(values) != 1 {
			return 0, 0, ErrInput
		}
		if name != "after" && name != "limit" {
			return 0, 0, ErrInput
		}
	}
	if value := r.URL.Query().Get("after"); value != "" {
		n, e := strconv.ParseInt(value, 10, 64)
		if e != nil || n < 0 {
			return 0, 0, ErrInput
		}
		after = n
	}
	if value := r.URL.Query().Get("limit"); value != "" {
		n, e := strconv.Atoi(value)
		if e != nil || n < 1 || n > 200 {
			return 0, 0, ErrInput
		}
		limit = n
	}
	return after, limit, nil
}
