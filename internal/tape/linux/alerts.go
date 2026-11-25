package linux

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

const TapeAlertLayout = "ssc-tapealert-2e-sp0-v1"
const MaxTapeAlertCaptureBytes = 4096

var ErrTapeAlertProfile = errors.New("TAPEALERT_CAPTURE_LAYOUT_REQUIRED")
var ErrTapeAlertCapture = errors.New("INVALID_TAPEALERT_CAPTURE")

type TapeAlertCaptureProfile struct {
	Layout               string `json:"layout"`
	QualificationRef     string `json:"qualification_ref"`
	PeripheralDeviceType uint8  `json:"peripheral_device_type"`
}

func (p TapeAlertCaptureProfile) Validate() error {
	if p.Layout != TapeAlertLayout || !text(p.QualificationRef) || p.PeripheralDeviceType != 1 {
		return ErrTapeAlertProfile
	}
	return nil
}

type TapeAlertFlag struct {
	Code             uint16 `json:"code"`
	Name             string `json:"name"`
	Known            bool   `json:"known"`
	Active           *bool  `json:"active"`
	Severity         string `json:"severity"`
	ParameterControl byte   `json:"parameter_control"`
	RawValue         byte   `json:"raw_value"`
}
type TapeAlertReport struct {
	Availability         string          `json:"availability"`
	Basis                string          `json:"basis"`
	Layout               string          `json:"layout"`
	QualificationRef     string          `json:"qualification_ref"`
	CaptureSHA256        string          `json:"capture_sha256,omitempty"`
	Flags                []TapeAlertFlag `json:"flags"`
	Severity             string          `json:"severity"`
	HighestKnownSeverity string          `json:"highest_known_severity"`
	CleaningStatus       string          `json:"cleaning_status"`
	CleaningRequired     *bool           `json:"cleaning_required"`
	CleaningRequested    *bool           `json:"cleaning_requested"`
	UnknownFlags         bool            `json:"unknown_flags"`
	LiveProbeAvailable   bool            `json:"live_probe_available"`
	Warnings             []string        `json:"warnings"`
}

func unavailableAlerts(profile TapeAlertCaptureProfile) TapeAlertReport {
	return TapeAlertReport{Availability: "UNAVAILABLE", Basis: "CAPTURED_TAPEALERT_PAGE", Layout: profile.Layout, QualificationRef: profile.QualificationRef, Flags: []TapeAlertFlag{}, Severity: "UNKNOWN", HighestKnownSeverity: "UNKNOWN", CleaningStatus: "UNAVAILABLE", Warnings: []string{}}
}

// InterpretCapturedTapeAlerts interprets only a complete, explicitly identified
// SSC page 2Eh/subpage zero raw response. It cannot discover or query hardware.
// Missing parameters remain unavailable, and unknown flags remain unknown.
func InterpretCapturedTapeAlerts(reader io.Reader, profile TapeAlertCaptureProfile) (TapeAlertReport, error) {
	report := unavailableAlerts(profile)
	if e := profile.Validate(); e != nil {
		return report, e
	}
	if reader == nil {
		return report, ErrTapeAlertCapture
	}
	raw, e := io.ReadAll(io.LimitReader(reader, MaxTapeAlertCaptureBytes+1))
	if e != nil {
		return report, errors.Join(ErrTapeAlertCapture, e)
	}
	if len(raw) < 4 || len(raw) > MaxTapeAlertCaptureBytes || raw[0] != 0x2e || raw[1] != 0 || int(binary.BigEndian.Uint16(raw[2:4])) != len(raw)-4 {
		return report, ErrTapeAlertCapture
	}
	if len(raw) == 4 {
		return report, nil
	}
	known := map[uint16]struct{ name, severity string }{
		1: {"Read warning", "WARNING"}, 2: {"Write warning", "WARNING"}, 3: {"Hard I/O fault", "WARNING"},
		4: {"Medium fault", "CRITICAL"}, 5: {"Read operation failed", "CRITICAL"}, 6: {"Write operation failed", "CRITICAL"},
		20: {"Cleaning required", "CRITICAL"}, 21: {"Cleaning requested", "WARNING"},
	}
	flags := make(map[uint16]TapeAlertFlag)
	unknownActive, unknownValue := false, false
	for offset := 4; offset < len(raw); {
		if len(raw)-offset < 5 || len(flags) >= 256 {
			return unavailableAlerts(profile), ErrTapeAlertCapture
		}
		code := binary.BigEndian.Uint16(raw[offset : offset+2])
		control := raw[offset+2]
		length := int(raw[offset+3])
		if length != 1 || control&3 != 0 {
			return unavailableAlerts(profile), ErrTapeAlertCapture
		}
		if _, duplicate := flags[code]; duplicate {
			return unavailableAlerts(profile), ErrTapeAlertCapture
		}
		value := raw[offset+4]
		definition, mapped := known[code]
		flag := TapeAlertFlag{Code: code, Known: mapped, Name: "Unknown TapeAlert flag", Severity: "UNKNOWN", ParameterControl: control, RawValue: value}
		if mapped {
			flag.Name = definition.name
			flag.Severity = definition.severity
		} else {
			report.UnknownFlags = true
		}
		if value <= 1 {
			active := value == 1
			flag.Active = &active
		} else {
			flag.Severity = "UNKNOWN"
			unknownValue = true
		}
		if flag.Active == nil || !flag.Known && *flag.Active {
			unknownActive = true
		}
		flags[code] = flag
		offset += 5
	}
	report.HighestKnownSeverity = "NONE_REPORTED"
	for _, flag := range flags {
		report.Flags = append(report.Flags, flag)
		if flag.Known && flag.Active != nil && *flag.Active {
			if flag.Severity == "CRITICAL" || flag.Severity == "WARNING" && report.HighestKnownSeverity != "CRITICAL" {
				report.HighestKnownSeverity = flag.Severity
			}
		}
	}
	sort.Slice(report.Flags, func(i, j int) bool { return report.Flags[i].Code < report.Flags[j].Code })
	if flag, ok := flags[20]; ok {
		report.CleaningRequired = flag.Active
	}
	if flag, ok := flags[21]; ok {
		report.CleaningRequested = flag.Active
	}
	switch {
	case report.CleaningRequired != nil && *report.CleaningRequired:
		report.CleaningStatus = "REQUIRED"
	case report.CleaningRequired != nil && report.CleaningRequested != nil && *report.CleaningRequested:
		report.CleaningStatus = "REQUESTED"
	case report.CleaningRequired != nil && report.CleaningRequested != nil:
		report.CleaningStatus = "NOT_REQUESTED"
	}
	report.Severity = report.HighestKnownSeverity
	if unknownActive {
		report.Severity = "UNKNOWN"
	}
	if report.UnknownFlags {
		report.Warnings = append(report.Warnings, "Unrecognized flag codes retain unknown severity. Consult the captured device documentation.")
	}
	if unknownValue {
		report.Warnings = append(report.Warnings, "Unrecognized parameter values remain unavailable.")
	}
	if report.CleaningStatus == "UNAVAILABLE" {
		report.Warnings = append(report.Warnings, "Cleaning status is unavailable because the required flag values were not reported.")
	}
	sum := sha256.Sum256(raw)
	report.CaptureSHA256 = hex.EncodeToString(sum[:])
	report.Availability = "AVAILABLE"
	return report, nil
}

// ReadCapturedTapeAlerts opens only a bounded regular offline file. Device,
// procfs/sysfs and symlink paths are rejected before any content read.
func ReadCapturedTapeAlerts(path string, profile TapeAlertCaptureProfile) (TapeAlertReport, error) {
	report := unavailableAlerts(profile)
	if e := profile.Validate(); e != nil {
		return report, e
	}
	absolute, e := filepath.Abs(path)
	if e != nil {
		return report, e
	}
	for _, system := range []string{"/dev", "/sys", "/proc"} {
		if absolute == system || strings.HasPrefix(absolute, system+"/") {
			return report, ErrTapeAlertCapture
		}
	}
	resolved, e := filepath.EvalSymlinks(absolute)
	if e != nil {
		return report, e
	}
	if resolved != absolute {
		return report, ErrTapeAlertCapture
	}
	for _, system := range []string{"/dev", "/sys", "/proc"} {
		if absolute == system || strings.HasPrefix(absolute, system+"/") {
			return report, ErrTapeAlertCapture
		}
	}
	fd, e := unix.Open(absolute, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return report, e
	}
	pinned := os.NewFile(uintptr(fd), "offline-tapealert-capture")
	defer pinned.Close()
	info, e := pinned.Stat()
	if e != nil {
		return report, e
	}
	if !info.Mode().IsRegular() || info.Size() < 4 || info.Size() > MaxTapeAlertCaptureBytes {
		return report, ErrTapeAlertCapture
	}
	file, e := os.Open(fmt.Sprintf("/proc/self/fd/%d", fd))
	if e != nil {
		return report, e
	}
	decoded, decodeErr := InterpretCapturedTapeAlerts(file, profile)
	closeErr := file.Close()
	if e = errors.Join(decodeErr, closeErr); e != nil {
		return report, e
	}
	return decoded, nil
}
