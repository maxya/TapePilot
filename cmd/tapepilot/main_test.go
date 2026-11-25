package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/store"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestOfflineSimulatorCLI(t *testing.T) {
	source := t.TempDir()
	state := filepath.Join(t.TempDir(), "state")
	conf := filepath.Join(t.TempDir(), "config.json")
	data, err := json.Marshal(map[string]any{"mode": "simulator", "physical_writes_enabled": false, "verify_after_write": true, "state_dir": state, "sources": map[string]string{"sample": source}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(conf, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, "sample.txt"), []byte("independent recovery\n"), 0600); err != nil {
		t.Fatal(err)
	}
	call := func(args ...string) string {
		t.Helper()
		var buf bytes.Buffer
		if err := run(context.Background(), conf, false, args, &buf); err != nil {
			t.Fatalf("%v: %v (%s)", args, err, buf.String())
		}
		return buf.String()
	}
	call("sim-create", "lab", "33554432")
	call("sim-insert", "lab")
	var j store.Job
	if err = json.Unmarshal([]byte(call("initialize", "lab", "Lab", "--confirm-synthetic")), &j); err != nil {
		t.Fatal(err)
	}
	if j.State != domain.Succeeded || j.VolumeID == "" {
		t.Fatal(j)
	}
	call("enroll", j.VolumeID)
	call("backup", j.VolumeID, "sample", "sample.txt")
	call("backup", j.VolumeID, "sample", "sample.txt")
	for _, name := range []string{"000001.tar", "000003.tar"} {
		cmd := exec.Command("tar", "-xOf", filepath.Join(state, "simulator", "lab", name), "sample.txt")
		data, err := cmd.Output()
		if err != nil || string(data) != "independent recovery\n" {
			t.Fatal(name, err, string(data))
		}
	}
	call("revoke", j.VolumeID)
	if err := run(context.Background(), conf, false, []string{"backup", j.VolumeID, "sample", "sample.txt"}, &bytes.Buffer{}); err == nil {
		t.Fatal("revoked enrollment allowed backup")
	}
}
