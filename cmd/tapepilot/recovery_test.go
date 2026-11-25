package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/maxya/TapePilot/internal/store"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// Recover using only a copy of the numbered cartridge and its simulator index.
// The second installation has never seen the source or the original SQLite DB.
func TestCatalogLossAndSelectedRestore(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	dest := t.TempDir()
	work := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	config := func(state string, hasSource bool) string {
		c := map[string]any{"state_dir": state, "mode": "simulator", "physical_writes_enabled": false, "verify_after_write": true, "spool_reserve_bytes": 1048576, "restore_roots": map[string]string{"out": dest}}
		if hasSource {
			c["sources"] = map[string]string{"sample": source}
		}
		data, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(work, filepath.Base(state)+".json")
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	original := filepath.Join(work, "original")
	fresh := filepath.Join(work, "fresh")
	conf := config(original, true)
	call := func(args ...string) []byte {
		t.Helper()
		var b bytes.Buffer
		if err := run(ctx, conf, false, args, &b); err != nil {
			t.Fatalf("%v %s %v", args, b.String(), err)
		}
		return b.Bytes()
	}
	call("sim-create", "lab", "33554432")
	call("sim-insert", "lab")
	var job store.Job
	if err := json.Unmarshal(call("initialize", "lab", "Lab", "--confirm-synthetic"), &job); err != nil {
		t.Fatal(err)
	}
	id := job.VolumeID
	call("enroll", id)
	call("backup", id, "sample")
	if err := os.Mkdir(fresh, 0700); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("cp", "-a", "--", filepath.Join(original, "simulator"), filepath.Join(fresh, "simulator")).Run(); err != nil {
		t.Fatal(err)
	}
	conf = config(fresh, false)
	call("sim-insert", "lab")
	call("import", id)
	var archives []store.Archive
	if err := json.Unmarshal(call("archives", id), &archives); err != nil || len(archives) != 1 {
		t.Fatal(err, archives)
	}
	if !bytes.Contains(call("recovery-text", id), []byte(archives[0].SHA256)) {
		t.Fatal("missing verified recovery digest")
	}
	if !bytes.Contains(call("catalog-ndjson", id), []byte("alpha")) {
		t.Fatal("missing catalog member")
	}
	var members []store.Member
	if err := json.Unmarshal(call("members", archives[0].ID), &members); err != nil || len(members) != 2 {
		t.Fatal(err, members)
	}
	member := members[0]
	call("restore-preview", archives[0].ID, "out", "selected", strconv.FormatInt(member.ID, 10))
	call("restore", archives[0].ID, "out", "selected", strconv.FormatInt(member.ID, 10))
	files, err := os.ReadDir(filepath.Join(dest, "selected"))
	if err != nil || len(files) != 1 || files[0].Name() != member.Path {
		t.Fatal(files, err)
	}
	call("restore", archives[0].ID, "out", "whole", "all")
	for _, name := range []string{"alpha", "beta"} {
		b, err := os.ReadFile(filepath.Join(dest, "whole", name))
		if err != nil || string(b) != name {
			t.Fatal(name, err)
		}
	}
	var output bytes.Buffer
	if run(ctx, conf, false, []string{"restore", archives[0].ID, "out", "whole", "all"}, &output) == nil {
		t.Fatal("overwrote existing destination")
	}
}

func TestLegacyObservedRestore(t *testing.T) {
	work, src, legacyRoot, out := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "item"), []byte("legacy bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(legacyRoot, "000000.tar")
	if err := exec.Command("tar", "--format=pax", "-cf", archivePath, "-C", src, "./item").Run(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(work, "legacy-config.json")
	b, err := json.Marshal(map[string]any{"state_dir": filepath.Join(work, "state"), "mode": "simulator", "physical_writes_enabled": false, "verify_after_write": true, "spool_reserve_bytes": 1048576, "legacy_roots": map[string]string{"old": legacyRoot}, "restore_roots": map[string]string{"out": out}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	call := func(args ...string) []byte {
		t.Helper()
		var b bytes.Buffer
		if err := run(context.Background(), configPath, false, args, &b); err != nil {
			t.Fatalf("%v: %s %v", args, b.String(), err)
		}
		return b.Bytes()
	}
	call("legacy-import", "old", "Old cartridge", "--confirm")
	var vols []store.Volume
	if err = json.Unmarshal(call("volumes"), &vols); err != nil || len(vols) != 1 {
		t.Fatal(err, vols)
	}
	var archives []store.Archive
	if err = json.Unmarshal(call("archives", vols[0].ID), &archives); err != nil || len(archives) != 1 || archives[0].Integrity != "OBSERVED" {
		t.Fatal(err, archives)
	}
	if !bytes.Contains(call("descriptor", vols[0].ID), []byte("OBSERVED_NOW")) {
		t.Fatal("legacy recovery descriptor omitted observed integrity basis")
	}
	call("restore", archives[0].ID, "out", "legacy", "all", "--legacy", "old")
	recovered, err := os.ReadFile(filepath.Join(out, "legacy", "item"))
	if err != nil || string(recovered) != "legacy bytes" {
		t.Fatal(err, string(recovered))
	}
	unchanged, err := os.ReadFile(archivePath)
	if err != nil || !bytes.Equal(unchanged, original) {
		t.Fatal("legacy source changed")
	}
}
