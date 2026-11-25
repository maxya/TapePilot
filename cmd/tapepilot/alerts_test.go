package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	linux "github.com/maxya/TapePilot/internal/tape/linux"
)

func TestOfflineTapeAlertCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "captured.bin")
	raw := []byte{0x2e, 0, 0, 10, 0, 20, 0xc0, 1, 1, 0, 21, 0xc0, 1, 0}
	if e := os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	args := []string{"tape-alerts", path, "--layout", linux.TapeAlertLayout, "--qualification-ref", "synthetic fixture", "--pdt", "1"}
	var output bytes.Buffer
	if e := alertsCommand(args, &output); e != nil {
		t.Fatal(e)
	}
	var report linux.TapeAlertReport
	if e := json.Unmarshal(output.Bytes(), &report); e != nil || report.CleaningStatus != "REQUIRED" || report.LiveProbeAvailable {
		t.Fatal(report, e)
	}
	args[7] = "8"
	output.Reset()
	if e := alertsCommand(args, &output); e == nil {
		t.Fatal("changer layout accepted as tape drive")
	}
	if e := alertsCommand(args[:2], &output); e == nil {
		t.Fatal("implicit layout accepted")
	}
}
