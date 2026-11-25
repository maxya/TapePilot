package api

import (
	"context"
	"encoding/json"
	"github.com/maxya/TapePilot/internal/app"
	"github.com/maxya/TapePilot/internal/auth"
	"github.com/maxya/TapePilot/internal/config"
	"github.com/maxya/TapePilot/internal/store"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestBarcodeAndVerifyScheduleHTTPContract(t *testing.T) {
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
	runtime := &app.Runtime{Store: db, Settings: config.Settings{StateDir: dir, PublicOrigin: "http://localhost:8787", Sources: map[string]string{"synthetic": t.TempDir()}}}
	h, err := NewConsole(runtime, app.Services{})
	if err != nil {
		t.Fatal(err)
	}
	guest := operationsRequest(h, "GET", "/api/v1/auth/session", "", nil, "")
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	json.Unmarshal(guest.Body.Bytes(), &session)
	signed := operationsRequest(h, "POST", "/api/v1/auth/login", `{"password":"synthetic-only-password-123"}`, guest.Result().Cookies()[0], session.CSRF)
	if signed.Code != 200 {
		t.Fatal(signed.Code, signed.Body.String())
	}
	json.Unmarshal(signed.Body.Bytes(), &session)
	cookie := signed.Result().Cookies()[0]
	create := operationsRequest(h, "POST", "/api/v1/volumes", `{"label":"Offline tape","location":"Shelf 1","barcode":"ABC123L5"}`, cookie, session.CSRF)
	if create.Code != 201 {
		t.Fatal(create.Code, create.Body.String())
	}
	var v store.Volume
	if err = json.Unmarshal(create.Body.Bytes(), &v); err != nil || v.Barcode != "ABC123L5" || v.State != "UNCLASSIFIED" {
		t.Fatal(v, err)
	}
	edit := operationsRequest(h, "PATCH", "/api/v1/volumes/"+v.ID, `{"label":"Offline tape","location":"Shelf 2","barcode":"DEF456L5","previous_label":"Offline tape","previous_location":"Shelf 1","previous_barcode":"ABC123L5"}`, cookie, session.CSRF)
	if edit.Code != 200 {
		t.Fatal(edit.Code, edit.Body.String())
	}
	stale := operationsRequest(h, "PATCH", "/api/v1/volumes/"+v.ID, `{"label":"Offline tape","location":"Shelf 1","barcode":"old","previous_label":"Offline tape","previous_location":"Shelf 1","previous_barcode":"ABC123L5"}`, cookie, session.CSRF)
	if stale.Code != 409 {
		t.Fatal(stale.Code, stale.Body.String())
	}
	oversized := operationsRequest(h, "POST", "/api/v1/volumes", `{"label":"Too long","barcode":"`+strings.Repeat("x", 129)+`"}`, cookie, session.CSRF)
	if oversized.Code != 400 {
		t.Fatal(oversized.Code, oversized.Body.String())
	}
	p, err := db.CreateBackupProfile(ctx, store.BackupProfile{Name: "Synthetic", SourceID: "synthetic", TargetVolumeID: v.ID, Enabled: true}, "admin")
	if err != nil {
		t.Fatal(err)
	}
	schedule := operationsRequest(h, "POST", "/api/v1/schedules", `{"profile_id":"`+p.ID+`","operation":"VERIFY","cadence":"daily","timezone":"UTC","local_time":"12:00","enabled":true}`, cookie, session.CSRF)
	if schedule.Code != 201 {
		t.Fatal(schedule.Code, schedule.Body.String())
	}
	var s store.Schedule
	if err = json.Unmarshal(schedule.Body.Bytes(), &s); err != nil || s.Operation != "VERIFY" {
		t.Fatal(s, err)
	}
	invalid := operationsRequest(h, "PATCH", "/api/v1/schedules/"+s.ID, `{"revision":1,"operation":"INITIALIZE"}`, cookie, session.CSRF)
	if invalid.Code != 400 {
		t.Fatal(invalid.Code, invalid.Body.String())
	}
	paused := operationsRequest(h, "PATCH", "/api/v1/schedules/"+s.ID, `{"revision":1,"enabled":false}`, cookie, session.CSRF)
	if paused.Code != 200 {
		t.Fatal(paused.Code, paused.Body.String())
	}
	var updated store.Schedule
	json.Unmarshal(paused.Body.Bytes(), &updated)
	if updated.Operation != "VERIFY" || updated.Revision != 2 {
		t.Fatal(updated)
	}
	// A valid session still needs CSRF before catalog metadata changes.
	if result := operationsRequest(h, http.MethodPatch, "/api/v1/volumes/"+v.ID, `{}`, cookie, ""); result.Code != 403 {
		t.Fatal(result.Code)
	}
}
