package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/api"
	"github.com/maxya/TapePilot/internal/app"
	"github.com/maxya/TapePilot/internal/auth"
	"github.com/maxya/TapePilot/internal/config"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/store"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestWebWorkflow(t *testing.T) {
	ctx := context.Background()
	base, src, out := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "example.txt"), []byte("original content"), 0600); err != nil {
		t.Fatal(err)
	}
	legacyDir := t.TempDir()
	conf := filepath.Join(base, "config.json")
	raw, err := json.Marshal(map[string]any{"legacy_roots": map[string]string{"old": legacyDir}, "mode": "simulator", "physical_writes_enabled": false, "verify_after_write": true, "state_dir": filepath.Join(base, "state"), "restore_roots": map[string]string{"output": out}, "sources": map[string]string{"sample": src}, "spool_reserve_bytes": 1024})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(conf, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cli := func(args ...string) []byte {
		t.Helper()
		var b bytes.Buffer
		if err := run(ctx, conf, false, args, &b); err != nil {
			t.Fatal(args, err)
		}
		return b.Bytes()
	}
	cli("sim-create", "lab", "33554432")
	cli("sim-insert", "lab")
	var init store.Job
	if err = json.Unmarshal(cli("initialize", "lab", "Lab", "--confirm-synthetic"), &init); err != nil {
		t.Fatal(err)
	}
	cli("enroll", init.VolumeID)
	settings, err := config.Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := app.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Store.Close()
	services := runtime.Services()
	password := uuid.NewString()
	if err = auth.Bootstrap(ctx, runtime.Store, password); err != nil {
		t.Fatal(err)
	}
	handler, err := api.NewConsole(runtime, services)
	if err != nil {
		t.Fatal(err)
	}
	csrf := ""
	cookies := map[string]*http.Cookie{}
	request := func(method, path string, data any, key string, want int) []byte {
		t.Helper()
		var payload io.Reader
		if data != nil {
			encoded, e := json.Marshal(data)
			if e != nil {
				t.Fatal(e)
			}
			payload = bytes.NewReader(encoded)
		}
		q := httptest.NewRequest(method, settings.PublicOrigin+"/api/v1/"+path, payload)
		q.RemoteAddr = "127.0.0.1:45678"
		for _, cookie := range cookies {
			q.AddCookie(cookie)
		}
		if method != "GET" {
			q.Header.Set("Origin", settings.PublicOrigin)
			q.Header.Set("Content-Type", "application/json")
			q.Header.Set("X-CSRF-Token", csrf)
			if key != "" {
				q.Header.Set("Idempotency-Key", key)
			}
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, q)
		if w.Code != want {
			t.Fatalf("%s %s got %d want %d body %s", method, path, w.Code, want, w.Body.String())
		}
		for _, cookie := range w.Result().Cookies() {
			cookies[cookie.Name] = cookie
		}
		return w.Body.Bytes()
	}
	object := func(raw []byte) map[string]any {
		t.Helper()
		var result map[string]any
		if e := json.Unmarshal(raw, &result); e != nil {
			t.Fatal(e)
		}
		return result
	}
	request("GET", "jobs", nil, "", 401)
	csrf = object(request("GET", "auth/session", nil, "", 200))["csrf_token"].(string)
	request("POST", "auth/login", map[string]string{"password": password}, "", 200)
	csrf = object(request("GET", "auth/session", nil, "", 200))["csrf_token"].(string)
	profile := object(request("POST", "backup-profiles", map[string]any{"name": "Daily", "source_id": "sample", "relative_selection": "", "exclusions": []string{}, "target_volume_id": init.VolumeID, "enabled": true}, "", 201))
	path := fmt.Sprintf("backup-profiles/%s/", profile["id"])
	plan := object(request("POST", path+"plan", map[string]any{"revision": profile["revision"]}, "", 201))
	key := uuid.NewString()
	params := map[string]any{"plan_id": plan["id"], "revision": profile["revision"]}
	job := object(request("POST", path+"run", params, key, 202))
	again := object(request("POST", path+"run", params, key, 202))
	if job["id"] != again["id"] {
		t.Fatal("duplicate run")
	}
	finish := func(id string) {
		t.Helper()
		if ok, e := services.Scheduler.RunOne(ctx); e != nil || !ok {
			t.Fatal(ok, e)
		}
		j, e := runtime.Store.GetJob(ctx, id)
		if e != nil || j.State != domain.Succeeded {
			t.Fatalf("%+v %v", j, e)
		}
	}
	finish(job["id"].(string))
	var found []store.CatalogMember
	if err = json.Unmarshal(request("GET", "catalog/search?q=example.txt", nil, "", 200), &found); err != nil || len(found) != 1 {
		t.Fatal(err, found)
	}
	restore := object(request("POST", "restores/plan", map[string]any{"archive_id": found[0].ArchiveID, "member_ids": []int64{found[0].ID}, "restore_root_id": "output", "destination_slug": "recovered"}, "", 201))
	j := object(request("POST", "restores/run", map[string]string{"plan_id": restore["id"].(string)}, uuid.NewString(), 202))
	finish(j["id"].(string))
	actual, e := os.ReadFile(filepath.Join(out, "recovered", "example.txt"))
	if e != nil || string(actual) != "original content" {
		t.Fatal(string(actual), e)
	}
	// A preview cannot authorize a different source enumeration at execution time.
	stale := object(request("POST", path+"plan", map[string]any{"revision": profile["revision"]}, "", 201))
	if e := os.WriteFile(filepath.Join(src, "added-after-preview"), []byte("additional content"), 0600); e != nil {
		t.Fatal(e)
	}
	changed := object(request("POST", path+"run", map[string]any{"plan_id": stale["id"], "revision": profile["revision"]}, uuid.NewString(), 202))
	if ok, e := services.Scheduler.RunOne(ctx); e != nil || !ok {
		t.Fatal(e)
	}
	failed, e := runtime.Store.GetJob(ctx, changed["id"].(string))
	if e != nil || failed.State != domain.Failed {
		t.Fatalf("changed plan outcome %+v %v", failed, e)
	}
	volume, e := runtime.Store.GetVolume(ctx, init.VolumeID)
	if e != nil || volume.State != domain.Managed {
		t.Fatal("changed plan altered media", volume, e)
	}

	// Exercise the same list contract for a legacy file with no commit marker.
	data, e := os.ReadFile(filepath.Join(base, "state", "simulator", "lab", "000001.tar"))
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(legacyDir, "000000.tar"), data, 0600); e != nil {
		t.Fatal(e)
	}
	imported := object(request("POST", "imports", map[string]any{"mode": "legacy", "source_id": "old", "label": "Legacy", "profile": "simulator-plain-tar-v1", "confirmed": true}, uuid.NewString(), 202))
	finish(imported["id"].(string))
	verifyArchiveResponse(t, request("GET", "archives", nil, "", 200))

}
