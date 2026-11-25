package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/app"
	"github.com/maxya/TapePilot/internal/auth"
	"github.com/maxya/TapePilot/internal/config"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/store"
)

func operationsRequest(h http.Handler, method, path, raw string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://localhost:8787"+path, strings.NewReader(raw))
	req.RemoteAddr = "127.0.0.1:2222"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://localhost:8787")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	result := httptest.NewRecorder()
	h.ServeHTTP(result, req)
	return result
}
func TestOperationsAuthenticatedProfileScheduleAndInbox(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if e := os.Chmod(dir, 0700); e != nil {
		t.Fatal(e)
	}
	db, e := store.Open(ctx, dir)
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	if e = auth.Bootstrap(ctx, db, "synthetic-only-password-123"); e != nil {
		t.Fatal(e)
	}
	runtime := &app.Runtime{Store: db, Settings: config.Settings{Sources: map[string]string{"synthetic": t.TempDir()}}}
	mux := http.NewServeMux()
	RegisterOperations(mux, runtime)
	guard, e := auth.New(db, "http://localhost:8787")
	if e != nil {
		t.Fatal(e)
	}
	handler := guard.Wrap(mux)
	if w := operationsRequest(handler, "GET", "/api/v1/backup-profiles", "", nil, ""); w.Code != 401 {
		t.Fatal("profile visible without session", w.Code)
	}
	anonymous := operationsRequest(handler, "GET", "/api/v1/auth/session", "", nil, "")
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if e = json.Unmarshal(anonymous.Body.Bytes(), &session); e != nil {
		t.Fatal(e)
	}
	login := operationsRequest(handler, "POST", "/api/v1/auth/login", `{"password":"synthetic-only-password-123"}`, anonymous.Result().Cookies()[0], session.CSRF)
	if login.Code != 200 {
		t.Fatal(login.Code, login.Body.String())
	}
	if e = json.Unmarshal(login.Body.Bytes(), &session); e != nil {
		t.Fatal(e)
	}
	cookie := login.Result().Cookies()[0]
	target := uuid.NewString()
	if e = db.CreateVolume(ctx, store.Volume{ID: target, Kind: "MANAGED", State: domain.BlankCandidate, Label: "Synthetic target"}); e != nil {
		t.Fatal(e)
	}
	request := `{"name":"Example","source_id":"synthetic","relative_selection":"data","exclusions":[],"target_volume_id":"` + target + `","enabled":true}`
	if w := operationsRequest(handler, "POST", "/api/v1/backup-profiles", request, cookie, ""); w.Code != 403 {
		t.Fatal("CSRF bypass", w.Code)
	}
	if w := operationsRequest(handler, "POST", "/api/v1/backup-profiles", strings.Replace(request, "synthetic", "unknown", 1), cookie, session.CSRF); w.Code != 400 {
		t.Fatal("unknown source accepted", w.Code)
	}
	result := operationsRequest(handler, "POST", "/api/v1/backup-profiles", request, cookie, session.CSRF)
	if result.Code != 201 {
		t.Fatal(result.Code, result.Body.String())
	}
	var profile store.BackupProfile
	if e = json.Unmarshal(result.Body.Bytes(), &profile); e != nil {
		t.Fatal(e)
	}
	patch := operationsRequest(handler, "PATCH", "/api/v1/backup-profiles/"+profile.ID, `{"revision":1,"name":"Updated"}`, cookie, session.CSRF)
	if patch.Code != 200 {
		t.Fatal(patch.Code, patch.Body.String())
	}
	if w := operationsRequest(handler, "PATCH", "/api/v1/backup-profiles/"+profile.ID, `{"revision":1,"enabled":false}`, cookie, session.CSRF); w.Code != 409 {
		t.Fatal("stale profile revision accepted", w.Code)
	}
	scheduleJSON := `{"profile_id":"` + profile.ID + `","cadence":"daily","timezone":"America/Chicago","local_time":"02:30","weekday":0,"enabled":true}`
	created := operationsRequest(handler, "POST", "/api/v1/schedules", scheduleJSON, cookie, session.CSRF)
	if created.Code != 201 {
		t.Fatal(created.Code, created.Body.String())
	}
	if w := operationsRequest(handler, "POST", "/api/v1/schedules", strings.Replace(scheduleJSON, "America/Chicago", "Local", 1), cookie, session.CSRF); w.Code != 400 {
		t.Fatal("host timezone accepted", w.Code)
	}
	job, e := db.Enqueue(ctx, store.Job{Kind: domain.Inspect})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Claim(ctx); e != nil {
		t.Fatal(e)
	}
	if e = db.FinishExecution(ctx, job.ID, errors.New("synthetic failure")); e != nil {
		t.Fatal(e)
	}
	inbox := operationsRequest(handler, "GET", "/api/v1/notifications", "", cookie, "")
	if inbox.Code != 200 {
		t.Fatal(inbox.Code, inbox.Body.String())
	}
	var notifications []store.Notification
	if e = json.Unmarshal(inbox.Body.Bytes(), &notifications); e != nil || len(notifications) == 0 {
		t.Fatal("failure notification missing", e)
	}
	readPath := "/api/v1/notifications/" + strconv.FormatInt(notifications[0].ID, 10) + "/read"
	if w := operationsRequest(handler, "POST", readPath, `{}`, cookie, session.CSRF); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	audit := operationsRequest(handler, "GET", "/api/v1/audit", "", cookie, "")
	if audit.Code != 200 {
		t.Fatal(audit.Code)
	}
	var entries []store.AuditEntry
	if e = json.Unmarshal(audit.Body.Bytes(), &entries); e != nil {
		t.Fatal(e)
	}
	found := false
	for _, a := range entries {
		if a.Action == "AUTH_LOGIN_SUCCEEDED" {
			found = true
		}
	}
	if !found {
		t.Fatal("login audit absent")
	}
	if w := operationsRequest(handler, "GET", "/api/v1/audit?limit=1&limit=200", "", cookie, ""); w.Code != 400 {
		t.Fatal("duplicate query parameter accepted")
	}
}
