package api

import (
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/app"
	"github.com/maxya/TapePilot/internal/auth"
	"github.com/maxya/TapePilot/internal/config"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/jobs"
	"github.com/maxya/TapePilot/internal/source"
	"github.com/maxya/TapePilot/internal/store"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

type noExecute struct{}

func (noExecute) Execute(context.Context, store.Job) error {
	panic("HTTP request test must only enqueue")
}
func idempotencyRequest(t *testing.T, h http.Handler, path, body, key string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "http://localhost:8787"+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:2345"
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "http://localhost:8787")
	r.Header.Set("X-CSRF-Token", csrf)
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	r.AddCookie(cookie)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestBackupAndRestoreRunBindActualPlanIDs(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	db, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = auth.Bootstrap(ctx, db, "synthetic-only-password-123"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err = os.WriteFile(root+"/example.txt", []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	runtime := &app.Runtime{Store: db, Spool: dir, Settings: config.Settings{StateDir: dir, PublicOrigin: "http://localhost:8787", Sources: map[string]string{"synthetic": root}}, Sources: []source.Root{{ID: "synthetic", Path: root}}}
	services := app.Services{Scheduler: jobs.New(db, noExecute{})}
	handler, err := NewConsole(runtime, services)
	if err != nil {
		t.Fatal(err)
	}
	anonymous := operationsRequest(handler, "GET", "/api/v1/auth/session", "", nil, "")
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	json.Unmarshal(anonymous.Body.Bytes(), &session)
	login := operationsRequest(handler, "POST", "/api/v1/auth/login", `{"password":"synthetic-only-password-123"}`, anonymous.Result().Cookies()[0], session.CSRF)
	if login.Code != 200 {
		t.Fatal(login.Code, login.Body.String())
	}
	json.Unmarshal(login.Body.Bytes(), &session)
	cookie := login.Result().Cookies()[0]
	volume := uuid.NewString()
	if err = db.CreateVolume(ctx, store.Volume{ID: volume, Kind: "MANAGED", State: domain.BlankCandidate, Label: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	profile, err := db.CreateBackupProfile(ctx, store.BackupProfile{Name: "Backup", SourceID: "synthetic", TargetVolumeID: volume, Enabled: true}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	var planIDs []string
	for range 2 {
		preview := idempotencyRequest(t, handler, "/api/v1/backup-profiles/"+profile.ID+"/plan", `{"revision":1}`, "", cookie, session.CSRF)
		if preview.Code != 201 {
			t.Fatal(preview.Code, preview.Body.String())
		}
		var p struct {
			ID string `json:"id"`
		}
		if err = json.Unmarshal(preview.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		planIDs = append(planIDs, p.ID)
	}
	runPath := "/api/v1/backup-profiles/" + profile.ID + "/run"
	key := uuid.NewString()
	first := idempotencyRequest(t, handler, runPath, `{"plan_id":"`+planIDs[0]+`","revision":1}`, key, cookie, session.CSRF)
	if first.Code != 202 {
		t.Fatal(first.Code, first.Body.String())
	}
	changed := idempotencyRequest(t, handler, runPath, `{"plan_id":"`+planIDs[1]+`","revision":1}`, key, cookie, session.CSRF)
	if changed.Code != 409 {
		t.Fatal("changed backup plan reused key", changed.Code, changed.Body.String())
	}
	replay := idempotencyRequest(t, handler, runPath, `{ "revision":1, "plan_id":"`+planIDs[0]+`" }`, key, cookie, session.CSRF)
	if replay.Code != 202 || replay.Body.String() != first.Body.String() {
		t.Fatal(replay.Code, replay.Body.String(), first.Body.String())
	}
	// Restore plans use identical validated execution options. The run endpoint
	// still binds the operator's distinct plan IDs independently of those options.
	restoreOptions := json.RawMessage(`{"archive_id":"` + uuid.NewString() + `","restore_root_id":"recovered","destination_slug":"fresh","all":true}`)
	var restores []store.HTTPPlan
	for range 2 {
		p := store.HTTPPlan{ID: uuid.NewString(), Kind: "RESTORE", VolumeID: volume, Options: restoreOptions, Preview: json.RawMessage(`{}`), ExpiresAt: time.Now().Add(time.Hour).UTC().Format("2006-01-02T15:04:05.000000000Z")}
		if err = db.SaveHTTPPlan(ctx, p); err != nil {
			t.Fatal(err)
		}
		restores = append(restores, p)
	}
	restoreKey := uuid.NewString()
	restoreFirst := idempotencyRequest(t, handler, "/api/v1/restores/run", `{"plan_id":"`+restores[0].ID+`"}`, restoreKey, cookie, session.CSRF)
	if restoreFirst.Code != 202 {
		t.Fatal(restoreFirst.Code, restoreFirst.Body.String())
	}
	restoreChanged := idempotencyRequest(t, handler, "/api/v1/restores/run", `{"plan_id":"`+restores[1].ID+`"}`, restoreKey, cookie, session.CSRF)
	if restoreChanged.Code != 409 {
		t.Fatal("changed restore plan reused key", restoreChanged.Code, restoreChanged.Body.String())
	}
	restoreReplay := idempotencyRequest(t, handler, "/api/v1/restores/run", `{"plan_id":"`+restores[0].ID+`","revision":0}`, restoreKey, cookie, session.CSRF)
	if restoreReplay.Code != 202 || restoreReplay.Body.String() != restoreFirst.Body.String() {
		t.Fatal(restoreReplay.Code, restoreReplay.Body.String())
	}
	list, err := db.ListJobs(ctx, 100)
	if err != nil || len(list) != 2 {
		t.Fatal("unexpected queued jobs", list, err)
	}
}
