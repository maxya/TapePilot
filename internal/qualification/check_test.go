package qualification

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// completeSynthetic constructs assertion-complete generated evidence. It is
// explicitly synthetic and must never qualify a physical drive.
func completeSynthetic(t *testing.T) Record {
	t.Helper()
	record := Record{SchemaVersion: 1, ProfileID: "hp-ultrium-5-test", Synthetic: true, CreatedAt: "2026-11-24T23:00:00Z", Identity: Identity{Vendor: "HP", Model: "Ultrium 5-SCSI", StableIdentity: "SYNTHETIC-ONLY-DRIVE", Firmware: "test", MediaGeneration: "LTO-5", CartridgeKind: "RW", CartridgeReference: "SYNTHETIC-CARTRIDGE", BlankOrigin: "generated fixture", HBAIdentity: "test-hba", HBAFirmware: "test", SASPath: "synthetic-path", KernelRelease: "test-linux", Distribution: "synthetic", MTSTVersion: "mt-st 1.7", TarVersion: "GNU tar 1.35", MbufferVersion: "20230301", SG3UtilsVersion: "test", RuntimeVersion: "test-compose", UID: 1000, GID: 1000, NonrewindDevice: "/dev/nst0", Profile: Profile{BlockMode: "variable", RecordBytes: 262144, MinRecordBytes: 1, MaxRecordBytes: 1048576, DataPartitions: 1, FilemarkPolicy: "single-on-close", EODPolicy: "zero-read-with-gmt-eod", AppendPositionPolicy: "verified-file-and-block-at-eod", SynchronousClose: true, AsyncWritesDisabled: true, SoftwareCompression: "none", Encryption: "none"}}, Authorization: Authorization{Operator: "synthetic operator", Reference: "synthetic-local-record", AuthorizedAt: "2026-11-24T20:00:00Z", PhysicalReadAuthorizationRecorded: true, PhysicalWriteAuthorizationRecorded: true, SacrificialMediaConfirmed: true, SoleCopyExcluded: true, LocalSupervision: true}, Tests: []Test{}}
	sum := strings.Repeat("1", 64)
	for _, req := range requirements {
		test := Test{ID: req.id, Source: "synthetic", Outcome: "PASS", ObservedAt: "2026-11-24T22:00:00Z", Assertions: map[string]bool{}, Evidence: []Evidence{{Reference: strings.ToLower(req.id) + ".log", SHA256: sum, Command: "synthetic test command, never executed"}}, Comparisons: []Comparison{}, Trials: []Trial{}, Notes: "Synthetic fixture. No hardware accessed."}
		for _, name := range req.assertions {
			test.Assertions[name] = true
		}
		for n := 0; n < req.comparisons; n++ {
			test.Comparisons = append(test.Comparisons, Comparison{Label: req.id + strings.Repeat("x", n+1), ExpectedSHA256: sum, ActualSHA256: sum, ExpectedBytes: 262144, ActualBytes: 262144})
		}
		if req.id == "HW-12" {
			for _, kind := range []string{"baseline", "application"} {
				for n := 0; n < 3; n++ {
					test.Trials = append(test.Trials, Trial{Kind: kind, DatasetSHA256: sum, WriteBytes: 1048576, WriteSeconds: 1, ReadbackBytes: 1048576, ReadbackSeconds: 2, CPUSeconds: 0.5, PeakRSSBytes: 1048576})
				}
			}
		}
		record.Tests = append(record.Tests, test)
	}
	var err error
	record.IdentityFingerprint, err = Fingerprint(record.Identity)
	if err != nil {
		t.Fatal(err)
	}
	return record
}
func encode(t *testing.T, record Record) []byte {
	t.Helper()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func TestSyntheticPassEvidenceCannotQualifyHardware(t *testing.T) {
	record := completeSynthetic(t)
	report, err := Check(bytes.NewReader(encode(t, record)))
	if err != nil {
		t.Fatal(err)
	}
	if report.EligibleForOperatorReview || report.HardwareQualified || report.WriteApproval || len(report.Matrix) != 14 {
		t.Fatal("synthetic hardware qualification", report)
	}
	for _, row := range report.Matrix {
		if row.Complete {
			t.Fatal("synthetic row considered physical", row)
		}
	}
}
func TestSelfReportedPhysicalAttestationOnlyBecomesReviewCandidate(t *testing.T) {
	record := completeSynthetic(t)
	// These declarations merely exercise validation of an operator claim. No
	// filesystem evidence is loaded and no physical result is asserted by tests.
	record.Synthetic = false
	for n := range record.Tests {
		record.Tests[n].Source = "physical"
	}
	report, err := Check(bytes.NewReader(encode(t, record)))
	if err != nil {
		t.Fatal(err)
	}
	if !report.EligibleForOperatorReview || report.HardwareQualified || report.WriteApproval {
		t.Fatal("validator granted physical authority", report)
	}
	if !strings.Contains(report.Limitations, "untrusted") {
		t.Fatal("attestation trust boundary missing")
	}
}
func TestIncompleteFailedSkippedAndSimulatedEvidenceWithholdsReview(t *testing.T) {
	for _, kind := range []string{"missing", "FAIL", "SKIP", "PARTIAL", "PENDING", "simulator", "mixed", "false-assertion", "missing-output", "bad-digest", "bad-bytes", "fingerprint", "unarmed", "bad-profile", "bad-time", "slow-performance"} {
		t.Run(kind, func(t *testing.T) {
			record := completeSynthetic(t)
			record.Synthetic = false
			for n := range record.Tests {
				record.Tests[n].Source = "physical"
			}
			switch kind {
			case "missing":
				record.Tests = record.Tests[1:]
			case "FAIL", "SKIP", "PARTIAL", "PENDING":
				record.Tests[0].Outcome = kind
			case "simulator", "mixed":
				record.Tests[0].Source = kind
			case "false-assertion":
				record.Tests[0].Assertions["stable_drive_identity"] = false
			case "missing-output":
				record.Tests[0].Evidence = []Evidence{}
			case "bad-digest":
				record.Tests[1].Comparisons[0].ActualSHA256 = strings.Repeat("2", 64)
			case "bad-bytes":
				record.Tests[1].Comparisons[0].ActualBytes = 1
			case "fingerprint":
				record.Identity.Firmware = "changed after qualification"
			case "unarmed":
				record.Authorization.LocalSupervision = false
			case "bad-profile":
				record.Identity.Profile.RecordBytes = 65536
				record.IdentityFingerprint, _ = Fingerprint(record.Identity)
			case "bad-time":
				record.Tests[0].ObservedAt = "2026-11-24T19:00:00Z"
			case "slow-performance":
				record.Tests[11].Trials[3].WriteSeconds = 100
				record.Tests[11].Trials[4].WriteSeconds = 100
				record.Tests[11].Trials[5].WriteSeconds = 100
			}
			report, err := Check(bytes.NewReader(encode(t, record)))
			if err != nil {
				t.Fatal(err)
			}
			if report.EligibleForOperatorReview || report.HardwareQualified || report.WriteApproval || len(report.Blockers) == 0 {
				t.Fatal("incomplete evidence accepted", report)
			}
		})
	}
}
func TestStrictJSONRejectsAmbiguousOrUnknownRecords(t *testing.T) {
	raw := string(encode(t, completeSynthetic(t)))
	for _, bad := range []string{
		"{}", raw + "{}", strings.Replace(raw, `"schema_version":1`, `"schema_version":1,"schema_version":1`, 1),
		strings.Replace(raw, `"synthetic":true`, `"Synthetic":true`, 1),
		strings.Replace(raw, `"vendor":"HP"`, `"vendor":"HP","vendor":"other"`, 1),
		strings.Replace(raw, `"stable_drive_identity":true`, `"stable_drive_identity":true,"stable_drive_identity":false`, 1),
		strings.Replace(raw, `"hardware_compression":false`, `"hardware_compression":null`, 1),
		strings.Replace(raw, `"schema_version":1`, `"schema_version":1.0`, 1),
		strings.Repeat("x", maxRecordBytes+1),
	} {
		if report, err := Check(strings.NewReader(bad)); err == nil || report.HardwareQualified || report.WriteApproval {
			t.Fatal("invalid record accepted")
		}
	}
}
func TestFingerprintBindsProfileAndDeploymentWithoutCartridgeLabel(t *testing.T) {
	original := completeSynthetic(t).Identity
	first, _ := Fingerprint(original)
	changed := original
	changed.Firmware = "new"
	second, _ := Fingerprint(changed)
	if first == second {
		t.Fatal("firmware change retained fingerprint")
	}
	changed = original
	changed.Profile.HardwareCompression = true
	second, _ = Fingerprint(changed)
	if first == second {
		t.Fatal("settings change retained fingerprint")
	}
	changed = original
	changed.CartridgeReference = "different sacrificial cartridge"
	changed.BlankOrigin = "separately authorized new media"
	second, _ = Fingerprint(changed)
	if first != second {
		t.Fatal("label became drive identity")
	}
}
func TestTemplateRemainsPending(t *testing.T) {
	file, err := os.Open("../../docs/hardware/record-template.json")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	report, err := Check(file)
	if err != nil {
		t.Fatal(err)
	}
	if report.EligibleForOperatorReview || report.HardwareQualified || report.WriteApproval || len(report.Matrix) != 14 {
		t.Fatal(report)
	}
}
func FuzzQualificationRecord(f *testing.F) {
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"schema_version":1}`))
	if raw, err := os.ReadFile("../../docs/hardware/record-template.json"); err == nil {
		f.Add(raw)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > maxRecordBytes+1 {
			return
		}
		report, _ := Check(bytes.NewReader(raw))
		if report.HardwareQualified || report.WriteApproval {
			t.Fatal("offline parser issued authority")
		}
	})
}
