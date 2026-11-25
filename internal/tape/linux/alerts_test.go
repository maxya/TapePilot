package linux

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func alertProfile() TapeAlertCaptureProfile {
	return TapeAlertCaptureProfile{Layout: TapeAlertLayout, QualificationRef: "synthetic fixture, no device qualification", PeripheralDeviceType: 1}
}
func alertCapture(flags ...[2]uint16) []byte {
	raw := []byte{0x2e, 0, 0, 0}
	for _, flag := range flags {
		raw = append(raw, byte(flag[0]>>8), byte(flag[0]), 0xc0, 1, byte(flag[1]))
	}
	binary.BigEndian.PutUint16(raw[2:4], uint16(len(raw)-4))
	return raw
}
func TestCapturedCleaningAndUnknownFlagsRemainDistinct(t *testing.T) {
	for _, test := range []struct {
		name                string
		required, requested uint16
		cleaning, severity  string
	}{
		{"required", 1, 0, "REQUIRED", "CRITICAL"}, {"requested", 0, 1, "REQUESTED", "WARNING"}, {"none reported", 0, 0, "NOT_REQUESTED", "NONE_REPORTED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			report, e := InterpretCapturedTapeAlerts(bytes.NewReader(alertCapture([2]uint16{20, test.required}, [2]uint16{21, test.requested})), alertProfile())
			if e != nil || report.Availability != "AVAILABLE" || report.CleaningStatus != test.cleaning || report.Severity != test.severity || report.LiveProbeAvailable || report.CleaningRequired == nil || report.CleaningRequested == nil {
				t.Fatalf("%#v %v", report, e)
			}
		})
	}
	report, e := InterpretCapturedTapeAlerts(bytes.NewReader(alertCapture([2]uint16{20, 1}, [2]uint16{21, 0}, [2]uint16{500, 1})), alertProfile())
	if e != nil || report.Severity != "UNKNOWN" || report.HighestKnownSeverity != "CRITICAL" || !report.UnknownFlags || report.Flags[2].Known {
		t.Fatalf("unknown alert became known: %#v %v", report, e)
	}
	if len(report.CaptureSHA256) != 64 {
		t.Fatal("capture hash missing")
	}
}
func TestMissingAndUnrecognizedValuesNeverBecomeZero(t *testing.T) {
	report, e := InterpretCapturedTapeAlerts(bytes.NewReader(alertCapture([2]uint16{21, 0})), alertProfile())
	if e != nil || report.CleaningStatus != "UNAVAILABLE" || report.CleaningRequired != nil || report.CleaningRequested == nil {
		t.Fatal(report, e)
	}
	report, e = InterpretCapturedTapeAlerts(bytes.NewReader(alertCapture([2]uint16{20, 2}, [2]uint16{21, 0})), alertProfile())
	if e != nil || report.CleaningRequired != nil || report.CleaningStatus != "UNAVAILABLE" || report.Severity != "UNKNOWN" {
		t.Fatal("unknown bits interpreted as clear", report, e)
	}
	report, e = InterpretCapturedTapeAlerts(bytes.NewReader([]byte{0x2e, 0, 0, 0}), alertProfile())
	if e != nil || report.Availability != "UNAVAILABLE" || report.Severity != "UNKNOWN" || report.CleaningRequired != nil {
		t.Fatal("empty page interpreted as healthy", report, e)
	}
}
func TestCapturedPageBoundsLayoutAndDuplicates(t *testing.T) {
	original := alertCapture([2]uint16{20, 1}, [2]uint16{21, 0})
	variants := [][]byte{nil, original[:3], append(append([]byte(nil), original...), 0), alertCapture([2]uint16{20, 0}, [2]uint16{20, 1}), bytes.Repeat([]byte("x"), MaxTapeAlertCaptureBytes+1)}
	for _, index := range []int{0, 1, 2, 7} {
		bad := append([]byte(nil), original...)
		bad[index]++
		variants = append(variants, bad)
	}
	bad := append([]byte(nil), original...)
	bad[6] = 3
	variants = append(variants, bad)
	for _, raw := range variants {
		report, e := InterpretCapturedTapeAlerts(bytes.NewReader(raw), alertProfile())
		if !errors.Is(e, ErrTapeAlertCapture) || report.Availability != "UNAVAILABLE" || report.CleaningRequired != nil {
			t.Fatal("malformed capture interpreted", report, e)
		}
	}
	for _, profile := range []TapeAlertCaptureProfile{{}, {Layout: TapeAlertLayout, QualificationRef: "fixture", PeripheralDeviceType: 8}, {Layout: "other-page-layout", QualificationRef: "fixture", PeripheralDeviceType: 1}} {
		if _, e := InterpretCapturedTapeAlerts(bytes.NewReader(original), profile); !errors.Is(e, ErrTapeAlertProfile) {
			t.Fatal("unqualified layout accepted", e)
		}
	}
}
func TestOfflineCaptureNeverOpensDevicesOrSymlinks(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "tapealert.bin")
	if e := os.WriteFile(path, alertCapture([2]uint16{20, 0}, [2]uint16{21, 0}), 0600); e != nil {
		t.Fatal(e)
	}
	if report, e := ReadCapturedTapeAlerts(path, alertProfile()); e != nil || report.Availability != "AVAILABLE" {
		t.Fatal(report, e)
	}
	link := filepath.Join(root, "link")
	if e := os.Symlink(path, link); e != nil {
		t.Fatal(e)
	}
	if _, e := ReadCapturedTapeAlerts(link, alertProfile()); !errors.Is(e, ErrTapeAlertCapture) {
		t.Fatal("symlink followed", e)
	}
	for _, path := range []string{"/dev/sg999999", "/sys/class/scsi_tape/nst999999", "/proc/self/fd/0"} {
		if _, e := ReadCapturedTapeAlerts(path, alertProfile()); !errors.Is(e, ErrTapeAlertCapture) {
			t.Fatal("system/device path reached content open", e)
		}
	}
	if len(QualifiedWriteProfiles()) != 0 {
		t.Fatal("diagnostics opened hardware write gate")
	}
}
func FuzzCapturedTapeAlertPage(f *testing.F) {
	f.Add(alertCapture([2]uint16{20, 1}, [2]uint16{21, 0}))
	f.Add([]byte("truncated"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		report, e := InterpretCapturedTapeAlerts(bytes.NewReader(raw), alertProfile())
		if e != nil && report.Availability != "UNAVAILABLE" {
			t.Fatal("failure became available")
		}
		if strings.Contains(report.CaptureSHA256, "/") {
			t.Fatal("invalid digest")
		}
	})
}
