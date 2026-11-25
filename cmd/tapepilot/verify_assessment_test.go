package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/media"
	"github.com/maxya/TapePilot/internal/store"
)

// This regression drives the same CLI entry point used by an operator. Every
// command closes its runtime before the synthetic cartridge is changed below.
func TestCLIVerificationAssessmentFailureAndExplicitFixtureRecovery(t *testing.T) {
	ctx := context.Background()
	source := t.TempDir()
	restoreRoot := t.TempDir()
	work := t.TempDir()
	state := filepath.Join(work, "state")
	configPath := filepath.Join(work, "config.json")
	config, err := json.Marshal(map[string]any{"mode": "simulator", "physical_writes_enabled": false, "verify_after_write": true, "state_dir": state, "sources": map[string]string{"sample": source}, "restore_roots": map[string]string{"out": restoreRoot}})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	firstPayload := []byte("synthetic first archive payload for verification regression\n")
	secondPayload := []byte("synthetic second archive payload for verification regression\n")
	sourceFile := filepath.Join(source, "item.txt")
	if err = os.WriteFile(sourceFile, firstPayload, 0600); err != nil {
		t.Fatal(err)
	}
	call := func(args ...string) []byte {
		t.Helper()
		var output bytes.Buffer
		if err := run(ctx, configPath, false, args, &output); err != nil {
			t.Fatalf("%v failed: %v (%s)", args, err, output.String())
		}
		return append([]byte(nil), output.Bytes()...)
	}
	call("sim-create", "lab", "33554432")
	call("sim-insert", "lab")
	var initialized store.Job
	if err = json.Unmarshal(call("initialize", "lab", "Verification fixture", "--confirm-synthetic"), &initialized); err != nil {
		t.Fatal(err)
	}
	volumeID := initialized.VolumeID
	if volumeID == "" {
		t.Fatal("initialization returned no volume identity")
	}
	call("enroll", volumeID)
	call("backup", volumeID, "sample", "item.txt")
	if err = os.WriteFile(sourceFile, secondPayload, 0600); err != nil {
		t.Fatal(err)
	}
	call("backup", volumeID, "sample", "item.txt")
	var initialVerify store.Job
	if err = json.Unmarshal(call("verify", volumeID), &initialVerify); err != nil || initialVerify.State != domain.Succeeded {
		t.Fatal("independent verification failed", initialVerify, err)
	}
	archiveState := func() map[int64]store.Archive {
		t.Helper()
		var entries []store.Archive
		if err := json.Unmarshal(call("archives", volumeID), &entries); err != nil {
			t.Fatal(err)
		}
		out := make(map[int64]store.Archive, len(entries))
		for _, entry := range entries {
			out[entry.DataIndex] = entry
		}
		if len(out) != 2 {
			t.Fatal("expected two archive pairs", len(out))
		}
		return out
	}
	before := archiveState()
	for _, entry := range before {
		if entry.Integrity != domain.Verified || entry.VerifiedAt == "" {
			t.Fatal("missing successful baseline evidence", entry)
		}
	}
	firstPath := filepath.Join(state, "simulator", "lab", "000001.tar")
	secondPath := filepath.Join(state, "simulator", "lab", "000003.tar")
	firstRaw, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	secondRaw, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	offset := bytes.Index(secondRaw, secondPayload)
	if offset < 0 || bytes.Count(secondRaw, secondPayload) != 1 {
		t.Fatal("second payload not uniquely located in ordinary TAR")
	}
	corrupt := append([]byte(nil), secondRaw...)
	corrupt[offset] ^= 0x01
	// Explicit test-only mutation while the application is stopped. Header and
	// commit bytes remain intact, so verification must detect the payload digest.
	if err = os.WriteFile(secondPath, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	var failedOutput bytes.Buffer
	if err = run(ctx, configPath, false, []string{"verify", volumeID}, &failedOutput); err == nil {
		t.Fatal("corrupted payload passed independent verification")
	}
	var failedJob store.Job
	if err = json.Unmarshal(failedOutput.Bytes(), &failedJob); err != nil {
		t.Fatal("failed verification did not return its durable job", failedOutput.String(), err)
	}
	if failedJob.State == domain.Succeeded {
		t.Fatal("corrupted volume job reported success")
	}
	after := archiveState()
	if after[1].ID != before[1].ID || after[1].Integrity != domain.Verified || after[1].SHA256 != before[1].SHA256 {
		t.Fatal("verified prefix changed", after[1])
	}
	affected := after[3]
	if affected.Integrity != domain.IntegrityFailed && affected.Integrity != domain.IntegrityUnknown {
		t.Fatal("corrupted archive retained current verified state", affected)
	}
	if affected.RestoreEligible {
		t.Fatal("corrupted archive remained restore eligible")
	}
	var preview bytes.Buffer
	if err = run(ctx, configPath, false, []string{"restore-preview", affected.ID, "out", "blocked", "all"}, &preview); err == nil {
		t.Fatal("failed current assessment still permitted restore planning")
	}
	if affected.SHA256 != before[3].SHA256 || affected.CommitSHA256 != before[3].CommitSHA256 || affected.StreamBytes != before[3].StreamBytes || affected.VerifiedAt != before[3].VerifiedAt || affected.IntegrityBasis != before[3].IntegrityBasis {
		t.Fatal("historical verification evidence was discarded", before[3], affected)
	}
	var descriptor media.Descriptor
	if err = json.Unmarshal(call("descriptor", volumeID), &descriptor); err != nil {
		t.Fatal(err)
	}
	if len(descriptor.Archives) != 1 || descriptor.Archives[0].ArchiveID != before[1].ID || descriptor.Archives[0].Sequence != 1 {
		t.Fatal("descriptor advertised an unverified suffix", descriptor.Archives)
	}
	unverifiedCaveat := false
	for _, caveat := range descriptor.Caveats {
		text := strings.ToLower(caveat)
		if strings.Contains(text, "sequence 2") && (strings.Contains(text, "failed") || strings.Contains(text, "unknown")) {
			unverifiedCaveat = true
		}
	}
	if !unverifiedCaveat {
		t.Fatal("descriptor omitted damaged sequence warning", descriptor.Caveats)
	}
	volumeState := func() domain.MediaState {
		t.Helper()
		var entries []store.Volume
		if err := json.Unmarshal(call("volumes"), &entries); err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.ID == volumeID {
				return entry.State
			}
		}
		t.Fatal("volume disappeared")
		return ""
	}
	if volumeState() != domain.Quarantined {
		t.Fatal("damaged media was not quarantined")
	}
	actualFirst, err := os.ReadFile(firstPath)
	if err != nil || !bytes.Equal(actualFirst, firstRaw) {
		t.Fatal("earlier tape bytes changed", err)
	}
	// Restore the original synthetic bytes explicitly. The application performs
	// no repair, tail rewrite, quarantine clearing or automatic write retry.
	if err = os.WriteFile(secondPath, secondRaw, 0600); err != nil {
		t.Fatal(err)
	}
	var repaired store.Job
	if err = json.Unmarshal(call("verify", volumeID), &repaired); err != nil || repaired.State != domain.Succeeded {
		t.Fatal("read-only re-verification failed", repaired, err)
	}
	current := archiveState()
	if current[1].Integrity != domain.Verified || current[3].Integrity != domain.Verified || current[3].SHA256 != before[3].SHA256 || current[3].IntegrityBasis != before[3].IntegrityBasis {
		t.Fatal("valid readback did not restore current integrity", current)
	}
	if volumeState() != domain.Quarantined {
		t.Fatal("successful verification silently cleared write protection policy")
	}
	var refused bytes.Buffer
	if err = run(ctx, configPath, false, []string{"backup", volumeID, "sample", "item.txt"}, &refused); err == nil {
		t.Fatal("verification automatically reopened write eligibility")
	}
	if _, err = os.Stat(filepath.Join(state, "simulator", "lab", "000005.tar")); !os.IsNotExist(err) {
		t.Fatal("quarantined media received another archive", err)
	}
	actualSecond, err := os.ReadFile(secondPath)
	if err != nil || !bytes.Equal(actualSecond, secondRaw) {
		t.Fatal("re-verification changed repaired fixture bytes", err)
	}
}
