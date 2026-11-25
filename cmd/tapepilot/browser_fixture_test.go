package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/app"
	"github.com/maxya/TapePilot/internal/auth"
	"github.com/maxya/TapePilot/internal/config"
	"github.com/maxya/TapePilot/internal/store"
	"os"
	"path/filepath"
	"testing"
)

// TestCreateBrowserFixture supplies an explicitly requested private synthetic
// installation to the browser acceptance harness through normal local APIs.
func TestCreateBrowserFixture(t *testing.T) {
	dir := os.Getenv("TAPEPILOT_BROWSER_FIXTURE_DIR")
	if dir == "" {
		return
	}
	resolved, e := filepath.EvalSymlinks(dir)
	if e != nil || resolved != dir || !filepath.IsAbs(dir) {
		t.Fatal("require a fresh private absolute directory")
	}
	fi, e := os.Stat(dir)
	if e != nil || !fi.IsDir() || fi.Mode().Perm() != 0700 {
		t.Fatal("require mode 0700")
	}
	rows, e := os.ReadDir(dir)
	if e != nil || len(rows) != 0 {
		t.Fatal("fixture directory must be empty")
	}
	origin := os.Getenv("TAPEPILOT_BROWSER_ORIGIN")
	if !config.ValidOrigin(origin) {
		t.Fatal("invalid test origin")
	}
	ctx := context.Background()
	src, out := filepath.Join(dir, "source"), filepath.Join(dir, "restored")
	for _, p := range []string{src, out} {
		if e := os.Mkdir(p, 0700); e != nil {
			t.Fatal(e)
		}
	}
	write := func(name string, b []byte) {
		t.Helper()
		if e := os.WriteFile(filepath.Join(dir, name), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	write("source/example.txt", []byte("TapePilot browser acceptance fixture\n"))
	write("source/another.txt", []byte("Unselected example\n"))
	raw, e := json.Marshal(map[string]any{"mode": "simulator", "physical_writes_enabled": false, "verify_after_write": true, "state_dir": filepath.Join(dir, "state"), "restore_roots": map[string]string{"out": out}, "sources": map[string]string{"sample": src}, "public_origin": origin, "spool_reserve_bytes": 1048576})
	if e != nil {
		t.Fatal(e)
	}
	write("config.json", raw)
	cli := func(args ...string) []byte {
		t.Helper()
		var b bytes.Buffer
		if e := run(ctx, filepath.Join(dir, "config.json"), false, args, &b); e != nil {
			t.Fatal(args, e)
		}
		return b.Bytes()
	}
	for _, id := range []string{"other", "lab"} {
		cli("sim-create", id, "33554432")
		cli("sim-insert", id)
		var job store.Job
		if e := json.Unmarshal(cli("initialize", id, id, "--confirm-synthetic"), &job); e != nil {
			t.Fatal(e)
		}
		if id == "lab" {
			cli("enroll", job.VolumeID)
			write("volume", []byte(job.VolumeID))
		}
	}
	c, e := config.Read(bytes.NewReader(raw))
	if e != nil {
		t.Fatal(e)
	}
	r, e := app.Open(ctx, c)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Store.Close()
	secret := uuid.NewString()
	if e = auth.Bootstrap(ctx, r.Store, secret); e != nil {
		t.Fatal(e)
	}
	write("password", []byte(secret))
}
