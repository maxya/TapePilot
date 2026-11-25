// Package qualification assesses offline operator evidence. It performs no
// hardware access and grants no physical qualification or write authority.
package qualification

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var ErrRecord = errors.New("INVALID_QUALIFICATION_RECORD")

const maxRecordBytes = 1 << 20

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var profileID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)
var nonrewind = regexp.MustCompile(`^/dev/nst[0-9]+$`)

type Identity struct {
	Vendor             string  `json:"vendor"`
	Model              string  `json:"model"`
	StableIdentity     string  `json:"stable_identity"`
	Firmware           string  `json:"firmware"`
	MediaGeneration    string  `json:"media_generation"`
	CartridgeKind      string  `json:"cartridge_kind"`
	CartridgeReference string  `json:"cartridge_reference"`
	BlankOrigin        string  `json:"blank_origin"`
	HBAIdentity        string  `json:"hba_identity"`
	HBAFirmware        string  `json:"hba_firmware"`
	SASPath            string  `json:"sas_path"`
	KernelRelease      string  `json:"kernel_release"`
	Distribution       string  `json:"distribution"`
	MTSTVersion        string  `json:"mt_st_version"`
	TarVersion         string  `json:"tar_version"`
	MbufferVersion     string  `json:"mbuffer_version"`
	SG3UtilsVersion    string  `json:"sg3_utils_version"`
	RuntimeVersion     string  `json:"runtime_version"`
	UID                int64   `json:"uid"`
	GID                int64   `json:"gid"`
	NonrewindDevice    string  `json:"nonrewind_device"`
	Profile            Profile `json:"profile"`
}
type Profile struct {
	BlockMode            string `json:"block_mode"`
	RecordBytes          int64  `json:"record_bytes"`
	MinRecordBytes       int64  `json:"min_record_bytes"`
	MaxRecordBytes       int64  `json:"max_record_bytes"`
	DataPartitions       int64  `json:"data_partitions"`
	FilemarkPolicy       string `json:"filemark_policy"`
	EODPolicy            string `json:"eod_policy"`
	AppendPositionPolicy string `json:"append_position_policy"`
	SynchronousClose     bool   `json:"synchronous_close"`
	AsyncWritesDisabled  bool   `json:"async_writes_disabled"`
	HardwareCompression  bool   `json:"hardware_compression"`
	SoftwareCompression  string `json:"software_compression"`
	Encryption           string `json:"encryption"`
}
type Authorization struct {
	Operator                           string `json:"operator"`
	Reference                          string `json:"reference"`
	AuthorizedAt                       string `json:"authorized_at"`
	PhysicalReadAuthorizationRecorded  bool   `json:"physical_read_authorization_recorded"`
	PhysicalWriteAuthorizationRecorded bool   `json:"physical_write_authorization_recorded"`
	SacrificialMediaConfirmed          bool   `json:"sacrificial_media_confirmed"`
	SoleCopyExcluded                   bool   `json:"sole_copy_excluded"`
	LocalSupervision                   bool   `json:"local_supervision"`
}
type Evidence struct {
	Reference string `json:"reference"`
	SHA256    string `json:"sha256"`
	Command   string `json:"command"`
	ExitCode  int64  `json:"exit_code"`
}
type Comparison struct {
	Label          string `json:"label"`
	ExpectedSHA256 string `json:"expected_sha256"`
	ActualSHA256   string `json:"actual_sha256"`
	ExpectedBytes  int64  `json:"expected_bytes"`
	ActualBytes    int64  `json:"actual_bytes"`
}
type Trial struct {
	Kind            string  `json:"kind"`
	DatasetSHA256   string  `json:"dataset_sha256"`
	WriteBytes      int64   `json:"write_bytes"`
	WriteSeconds    float64 `json:"write_seconds"`
	ReadbackBytes   int64   `json:"readback_bytes"`
	ReadbackSeconds float64 `json:"readback_seconds"`
	CPUSeconds      float64 `json:"cpu_seconds"`
	PeakRSSBytes    int64   `json:"peak_rss_bytes"`
}
type Test struct {
	ID          string          `json:"id"`
	Source      string          `json:"source"`
	Outcome     string          `json:"outcome"`
	ObservedAt  string          `json:"observed_at"`
	Assertions  map[string]bool `json:"assertions"`
	Evidence    []Evidence      `json:"evidence"`
	Comparisons []Comparison    `json:"comparisons"`
	Trials      []Trial         `json:"trials"`
	Notes       string          `json:"notes"`
}
type Record struct {
	SchemaVersion       int64         `json:"schema_version"`
	ProfileID           string        `json:"profile_id"`
	Synthetic           bool          `json:"synthetic"`
	CreatedAt           string        `json:"created_at"`
	Identity            Identity      `json:"identity"`
	IdentityFingerprint string        `json:"identity_fingerprint"`
	Authorization       Authorization `json:"authorization"`
	Tests               []Test        `json:"tests"`
}
type Assessment struct {
	ID                string   `json:"id"`
	Outcome           string   `json:"outcome"`
	Source            string   `json:"source"`
	Complete          bool     `json:"complete"`
	MissingAssertions []string `json:"missing_assertions"`
	Issues            []string `json:"issues"`
}
type Report struct {
	ProfileID                 string       `json:"profile_id"`
	IdentityFingerprint       string       `json:"identity_fingerprint"`
	EligibleForOperatorReview bool         `json:"eligible_for_operator_review"`
	HardwareQualified         bool         `json:"hardware_qualified"`
	WriteApproval             bool         `json:"write_approval"`
	Limitations               string       `json:"limitations"`
	Blockers                  []string     `json:"blockers"`
	Matrix                    []Assessment `json:"matrix"`
}

type requirement struct {
	id          string
	assertions  []string
	comparisons int
}

var requirements = []requirement{
	{"HW-01", []string{"stable_drive_identity", "nonrewind_node_confirmed", "exclusive_mapping", "record_limits_observed"}, 0},
	{"HW-02", []string{"write_protection_confirmed", "baseline_bytes_match", "baseline_sha256_match", "no_write_open"}, 1},
	{"HW-03", []string{"sacrificial_blank_origin_confirmed", "blank_classification_observed", "variable_records_observed", "single_filemark_close_observed", "synchronous_flush_observed"}, 0},
	{"HW-04", []string{"permit_before_open", "exact_records_written", "close_checked", "header_reread_sha256_match", "filemark_eod_observed"}, 1},
	{"HW-05", []string{"fresh_scan_before_each_append", "positive_eod_position", "two_archives_commits_recovered", "earlier_hashes_unchanged"}, 4},
	{"HW-06", []string{"file_payload_hashes_match", "catalog_from_readback", "full_restore_matches", "selective_restore_matches"}, 1},
	{"HW-07", []string{"wrong_identity_refused", "write_protect_refused", "foreign_media_refused", "sealed_revoked_refused", "stale_permit_refused", "no_writer_open"}, 0},
	{"HW-08", []string{"interrupted_tail_detected", "no_auto_retry", "append_refused", "earlier_archives_match"}, 1},
	{"HW-09", []string{"physical_eom_observed", "no_partial_success", "no_retry_or_span", "tail_quarantined"}, 0},
	{"HW-10", []string{"active_write_restart_recovery_required", "no_auto_resume", "valid_prefix_recovered"}, 1},
	{"HW-11", []string{"app_binary_absent", "sqlite_absent", "write_protected_recovery", "gnu_tar_extraction", "source_sha256_match", "header_commit_chain_match", "second_operator_witness"}, 1},
	{"HW-12", []string{"three_baseline_trials", "three_application_trials", "matching_record_compression_profile", "write_readback_measured_separately", "cpu_rss_recorded"}, 0},
	{"HW-13", []string{"budgeted_synthetic_dataset", "backup_verify_import_restore_success", "no_leaks_or_stalls", "previous_hashes_intact"}, 1},
	{"HW-14", []string{"actual_compose_profile_tested", "nonroot_permissions", "explicit_device_mapping", "shared_exclusive_lease", "reconnect_identity_revalidated"}, 1},
}

// Check parses and assesses declarations and evidence references only. It does
// not read referenced logs, authenticate an operator, or issue runtime permits.
func Check(reader io.Reader) (Report, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, maxRecordBytes+1))
	if err != nil {
		return Report{}, err
	}
	if len(raw) > maxRecordBytes || !utf8.Valid(raw) {
		return Report{}, ErrRecord
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err = shape(decoder, reflect.TypeOf(Record{})); err != nil {
		return Report{}, err
	}
	if _, err = decoder.Token(); err != io.EOF {
		return Report{}, ErrRecord
	}
	var record Record
	if err = json.Unmarshal(raw, &record); err != nil {
		return Report{}, ErrRecord
	}
	if record.SchemaVersion != 1 || len(record.Tests) > 14 || !profileID.MatchString(record.ProfileID) {
		return Report{}, ErrRecord
	}
	fingerprint, err := Fingerprint(record.Identity)
	if err != nil {
		return Report{}, err
	}
	report := Report{ProfileID: record.ProfileID, IdentityFingerprint: fingerprint, Limitations: "Offline self-reported evidence is untrusted until an operator independently reviews its logs and actual hardware. This assessment grants no hardware qualification, write authorization, media initialization or enrollment.", Blockers: []string{}, Matrix: []Assessment{}}
	if record.Synthetic {
		report.Blockers = append(report.Blockers, "Record is explicitly synthetic.")
	}
	created, createdOK := instant(record.CreatedAt)
	authorized, authorizedOK := instant(record.Authorization.AuthorizedAt)
	if !createdOK {
		report.Blockers = append(report.Blockers, "Record creation time must be a valid UTC instant.")
	}
	if !authorizedOK || createdOK && authorized.After(created) {
		report.Blockers = append(report.Blockers, "Supervised test authorization must predate the evidence record.")
	}
	auth := record.Authorization
	if !text(auth.Operator, 256) || !text(auth.Reference, 1024) || !auth.PhysicalReadAuthorizationRecorded || !auth.PhysicalWriteAuthorizationRecorded || !auth.SacrificialMediaConfirmed || !auth.SoleCopyExcluded || !auth.LocalSupervision {
		report.Blockers = append(report.Blockers, "Operator supervision, sacrificial-media provenance and test authorizations are incomplete.")
	}
	if !identityComplete(record.Identity) {
		report.Blockers = append(report.Blockers, "Hardware/software identity and the candidate record/flush/EOD profile are incomplete or unsupported.")
	}
	if record.IdentityFingerprint != fingerprint {
		report.Blockers = append(report.Blockers, "Recorded identity fingerprint does not match the declared identity and profile.")
	}
	tests := map[string]Test{}
	for _, test := range record.Tests {
		req, known := find(test.ID)
		if !known || tests[test.ID].ID != "" || !validSource(test.Source) || !validOutcome(test.Outcome) || len(test.Evidence) > 64 || len(test.Comparisons) > 10000 || len(test.Trials) > 6 || len(test.Assertions) > len(req.assertions) || len(test.Notes) > 4096 {
			return Report{}, ErrRecord
		}
		for assertion := range test.Assertions {
			found := false
			for _, known := range req.assertions {
				if assertion == known {
					found = true
				}
			}
			if !found {
				return Report{}, ErrRecord
			}
		}
		tests[test.ID] = test
	}
	for _, req := range requirements {
		test, exists := tests[req.id]
		assessment := Assessment{ID: req.id, Outcome: "MISSING", Source: "unknown", MissingAssertions: []string{}, Issues: []string{}}
		if !exists {
			assessment.Issues = append(assessment.Issues, "Required evidence row is missing.")
		} else {
			assessment.Outcome = test.Outcome
			assessment.Source = test.Source
			if test.Outcome != "PASS" {
				assessment.Issues = append(assessment.Issues, "Outcome is incomplete, failed or skipped.")
			}
			if test.Source != "physical" {
				assessment.Issues = append(assessment.Issues, "Simulator, mixed or synthetic results cannot establish physical qualification.")
			}
			observed, ok := instant(test.ObservedAt)
			if !ok || createdOK && observed.After(created) || authorizedOK && observed.Before(authorized) {
				assessment.Issues = append(assessment.Issues, "Observed time must fall between operator authorization and record creation.")
			}
			for _, name := range req.assertions {
				if !test.Assertions[name] {
					assessment.MissingAssertions = append(assessment.MissingAssertions, name)
				}
			}
			if len(test.Evidence) == 0 {
				assessment.Issues = append(assessment.Issues, "Command and output evidence is missing.")
			}
			refs := map[string]bool{}
			for _, evidence := range test.Evidence {
				if !reference(evidence.Reference) || refs[evidence.Reference] || !digestPattern.MatchString(evidence.SHA256) || !text(evidence.Command, 4096) || evidence.ExitCode != 0 {
					assessment.Issues = append(assessment.Issues, "Evidence references, digests or literal command records are invalid.")
					break
				}
				refs[evidence.Reference] = true
			}
			if len(test.Comparisons) < req.comparisons {
				assessment.Issues = append(assessment.Issues, "Required exact-byte and SHA-256 comparisons are missing.")
			}
			labels := map[string]bool{}
			for _, comparison := range test.Comparisons {
				if !text(comparison.Label, 256) || labels[comparison.Label] || !digestPattern.MatchString(comparison.ExpectedSHA256) || comparison.ExpectedSHA256 != comparison.ActualSHA256 || comparison.ExpectedBytes <= 0 || comparison.ExpectedBytes != comparison.ActualBytes {
					assessment.Issues = append(assessment.Issues, "A recorded byte count or digest comparison does not match.")
					break
				}
				labels[comparison.Label] = true
			}
			if req.id == "HW-12" {
				if !performance(test.Trials) {
					assessment.Issues = append(assessment.Issues, "Three comparable baseline and application trials with separate readback and at least 80 percent write-stream ratio are required.")
				}
			} else if len(test.Trials) != 0 {
				return Report{}, ErrRecord
			}
		}
		assessment.Complete = len(assessment.Issues) == 0 && len(assessment.MissingAssertions) == 0
		if !assessment.Complete {
			report.Blockers = append(report.Blockers, req.id+" requires complete physical evidence and operator review.")
		}
		report.Matrix = append(report.Matrix, assessment)
	}
	report.EligibleForOperatorReview = len(report.Blockers) == 0
	return report, nil
}

// Fingerprint binds the declared drive, firmware, host, utilities and exact
// record/flush/position profile. It provides change detection without attestation.
func Fingerprint(identity Identity) (string, error) {
	identity.CartridgeReference = ""
	identity.BlankOrigin = ""
	raw, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
func find(id string) (requirement, bool) {
	for _, req := range requirements {
		if req.id == id {
			return req, true
		}
	}
	return requirement{}, false
}
func text(s string, max int) bool {
	return s != "" && len(s) <= max && utf8.ValidString(s) && !strings.ContainsAny(s, "\x00\r\n")
}
func reference(s string) bool {
	return len(s) <= 1024 && s != "." && fs.ValidPath(s) && !strings.ContainsAny(s, "\x00\r\n\\")
}
func instant(s string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, s)
	return parsed, err == nil && strings.HasSuffix(s, "Z")
}
func validSource(s string) bool {
	switch s {
	case "physical", "simulator", "synthetic", "mixed", "template":
		return true
	}
	return false
}
func validOutcome(s string) bool {
	switch s {
	case "PASS", "FAIL", "SKIP", "PARTIAL", "PENDING":
		return true
	}
	return false
}
func identityComplete(identity Identity) bool {
	values := reflect.ValueOf(identity)
	for n := 0; n < values.NumField(); n++ {
		if values.Field(n).Kind() == reflect.String && !text(values.Field(n).String(), 256) {
			return false
		}
	}
	p := identity.Profile
	return identity.MediaGeneration == "LTO-5" && identity.CartridgeKind == "RW" && identity.UID > 0 && identity.GID >= 0 && nonrewind.MatchString(identity.NonrewindDevice) && p.BlockMode == "variable" && p.RecordBytes == 262144 && p.MinRecordBytes > 0 && p.MinRecordBytes <= p.RecordBytes && p.MaxRecordBytes >= p.RecordBytes && p.MaxRecordBytes <= 1<<20 && p.DataPartitions == 1 && p.FilemarkPolicy == "single-on-close" && p.EODPolicy == "zero-read-with-gmt-eod" && p.AppendPositionPolicy == "verified-file-and-block-at-eod" && p.SynchronousClose && p.AsyncWritesDisabled && p.SoftwareCompression == "none" && p.Encryption == "none"
}
func performance(trials []Trial) bool {
	counts := map[string]int{}
	rates := map[string]float64{}
	dataset := ""
	var bytes int64
	for _, trial := range trials {
		if trial.Kind != "baseline" && trial.Kind != "application" || !digestPattern.MatchString(trial.DatasetSHA256) || trial.WriteBytes <= 0 || trial.WriteBytes%262144 != 0 || trial.ReadbackBytes != trial.WriteBytes || trial.WriteSeconds <= 0 || trial.ReadbackSeconds <= 0 || trial.CPUSeconds < 0 || trial.PeakRSSBytes <= 0 {
			return false
		}
		for _, number := range []float64{trial.WriteSeconds, trial.ReadbackSeconds, trial.CPUSeconds} {
			if math.IsNaN(number) || math.IsInf(number, 0) {
				return false
			}
		}
		if dataset == "" {
			dataset = trial.DatasetSHA256
			bytes = trial.WriteBytes
		}
		if dataset != trial.DatasetSHA256 || bytes != trial.WriteBytes {
			return false
		}
		counts[trial.Kind]++
		rates[trial.Kind] += float64(trial.WriteBytes) / trial.WriteSeconds
	}
	return counts["baseline"] == 3 && counts["application"] == 3 && rates["application"] >= 0.8*rates["baseline"]
}

// shape enforces exact case-sensitive keys and rejects duplicate keys at every
// level before encoding/json can silently collapse them.
func shape(d *json.Decoder, t reflect.Type) error {
	token, err := d.Token()
	if err != nil {
		return ErrRecord
	}
	switch t.Kind() {
	case reflect.Struct:
		if token != json.Delim('{') {
			return ErrRecord
		}
		fields := map[string]reflect.Type{}
		for n := 0; n < t.NumField(); n++ {
			field := t.Field(n)
			fields[field.Tag.Get("json")] = field.Type
		}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return ErrRecord
			}
			name, ok := key.(string)
			kind, known := fields[name]
			if !ok || !known {
				return ErrRecord
			}
			delete(fields, name)
			if err = shape(d, kind); err != nil {
				return err
			}
		}
		if token, err = d.Token(); err != nil || token != json.Delim('}') || len(fields) != 0 {
			return ErrRecord
		}
	case reflect.Slice:
		if token != json.Delim('[') {
			return ErrRecord
		}
		for d.More() {
			if err = shape(d, t.Elem()); err != nil {
				return err
			}
		}
		if token, err = d.Token(); err != nil || token != json.Delim(']') {
			return ErrRecord
		}
	case reflect.Map:
		if token != json.Delim('{') {
			return ErrRecord
		}
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return ErrRecord
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return ErrRecord
			}
			seen[name] = true
			if err = shape(d, t.Elem()); err != nil {
				return err
			}
		}
		if token, err = d.Token(); err != nil || token != json.Delim('}') {
			return ErrRecord
		}
	case reflect.String:
		if _, ok := token.(string); !ok {
			return ErrRecord
		}
	case reflect.Bool:
		if _, ok := token.(bool); !ok {
			return ErrRecord
		}
	case reflect.Int64:
		if value, ok := token.(json.Number); !ok {
			return ErrRecord
		} else if _, err = strconv.ParseInt(value.String(), 10, 64); err != nil {
			return ErrRecord
		}
	case reflect.Float64:
		if _, ok := token.(json.Number); !ok {
			return ErrRecord
		}
	default:
		return fmt.Errorf("%w: unsupported record field", ErrRecord)
	}
	return nil
}
