package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/maxya/TapePilot/internal/qualification"
	"golang.org/x/sys/unix"
)

func TestQualificationDocumentBoundary(t *testing.T) {
	template := filepath.Join("..", "..", "docs", "hardware", "record-template.json")
	var output bytes.Buffer
	if err := checkQualification(template, &output); err == nil {
		t.Fatal("pending record accepted")
	}
	var report qualification.Report
	if err := json.Unmarshal(output.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Matrix) != 14 || report.EligibleForOperatorReview || report.HardwareQualified || report.WriteApproval {
		t.Fatal("unexpected pending record assessment")
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	pipe := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(pipe, 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, link, pipe} {
		output.Reset()
		if err := checkQualification(path, &output); err == nil || output.Len() != 0 {
			t.Fatal("accepted non-regular document")
		}
	}
}
