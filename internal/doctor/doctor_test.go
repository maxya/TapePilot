package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/maxya/TapePilot/internal/config"
)

func settings(t *testing.T) config.Settings {
	t.Helper()
	dir := t.TempDir()
	c := config.Default()
	c.StateDir = filepath.Join(dir, "state")
	c.SpoolReserveBytes = 1024
	c.Sources = map[string]string{"sample": filepath.Join(dir, "source")}
	c.RestoreRoots = map[string]string{"out": filepath.Join(dir, "restored")}
	c.LegacyRoots = map[string]string{"old": filepath.Join(dir, "legacy")}
	for _, name := range []string{c.StateDir, filepath.Join(c.StateDir, "spool"), c.Sources["sample"], c.RestoreRoots["out"], c.LegacyRoots["old"]} {
		if err := os.Mkdir(name, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return c
}
func knownProbe(_ context.Context, name string, args []string) (string, error) {
	if !reflect.DeepEqual(args, []string{"--version"}) {
		return "", errors.New("unexpected invocation")
	}
	switch name {
	case "/usr/bin/tar":
		return "tar (GNU tar) 1.35\n", nil
	case "/usr/bin/mbuffer":
		return "mbuffer version 20230301\n", nil
	case "/usr/bin/mt":
		return "mt-st v. 1.7\n", nil
	case "/usr/bin/lsscsi":
		return "lsscsi version: 0.32\n", nil
	}
	return "", errors.New("unexpected utility")
}
func byID(t *testing.T, r Report, id string) Check {
	t.Helper()
	for _, check := range r.Checks {
		if check.ID == id {
			return check
		}
	}
	t.Fatal("missing check", id)
	return Check{}
}
func TestSimulatorDoctorUsesOnlyVersionAllowlistAndOperationalIDs(t *testing.T) {
	c := settings(t)
	calls := []string{}
	report := (Inspector{Probe: func(ctx context.Context, name string, args []string) (string, error) {
		calls = append(calls, name)
		return knownProbe(ctx, name, args)
	}}).Inspect(context.Background(), c)
	if !report.Ready || report.Mode != "simulator" || len(calls) != 4 {
		t.Fatal(report, calls)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), filepath.Dir(c.StateDir)) {
		t.Fatal("web report exposed configured absolute paths")
	}
	for _, id := range []string{"root:spool", "root:restore:out"} {
		check := byID(t, report, id)
		if check.AvailableBytes == nil || check.RequiredBytes == nil || check.Identity == "" {
			t.Fatal(check)
		}
	}
	if byID(t, report, "roots:overlap").Status != "ok" {
		t.Fatal(report)
	}
}
func TestMissingAndWrongUtilitiesRemainExplicit(t *testing.T) {
	c := settings(t)
	report := (Inspector{Probe: func(ctx context.Context, name string, args []string) (string, error) {
		if name == "/usr/bin/mt" {
			return "mt (GNU cpio) 2.15\n", nil
		}
		if name == "/usr/bin/mbuffer" {
			return "", os.ErrNotExist
		}
		return knownProbe(ctx, name, args)
	}}).Inspect(context.Background(), c)
	if byID(t, report, "utility:mt-st").Status != "warning" || !strings.Contains(byID(t, report, "utility:mt-st").Message, "GNU mt") {
		t.Fatal(report)
	}
	if byID(t, report, "utility:mbuffer").Status != "warning" || !strings.Contains(byID(t, report, "utility:mbuffer").Message, "bounded Go") {
		t.Fatal(report)
	}
	report = (Inspector{Probe: func(context.Context, string, []string) (string, error) { return "", os.ErrNotExist }}).Inspect(context.Background(), c)
	if report.Ready || byID(t, report, "utility:tar").Status != "error" {
		t.Fatal(report)
	}
}
func TestConfiguredRootFailuresAndOverlap(t *testing.T) {
	for _, kind := range []string{"overlap", "symlink", "public-state", "missing-source", "device-path"} {
		t.Run(kind, func(t *testing.T) {
			c := settings(t)
			switch kind {
			case "overlap":
				c.RestoreRoots["out"] = c.Sources["sample"]
			case "symlink":
				alias := filepath.Join(filepath.Dir(c.StateDir), "alias")
				if err := os.Symlink(c.Sources["sample"], alias); err != nil {
					t.Fatal(err)
				}
				c.Sources["sample"] = alias
			case "public-state":
				if err := os.Chmod(c.StateDir, 0755); err != nil {
					t.Fatal(err)
				}
			case "missing-source":
				c.Sources["sample"] = filepath.Join(filepath.Dir(c.StateDir), "absent")
			case "device-path":
				c.Sources["sample"] = "/dev/nst0"
			}
			report := (Inspector{Probe: knownProbe}).Inspect(context.Background(), c)
			if report.Ready {
				t.Fatal("unsafe root reported ready", report)
			}
		})
	}
}
func TestCancellationAndOutputLimits(t *testing.T) {
	c := settings(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report := (Inspector{Probe: func(context.Context, string, []string) (string, error) {
		t.Fatal("probe after cancellation")
		return "", nil
	}}).Inspect(ctx, c)
	if report.Ready || byID(t, report, "inspection").Status != "error" {
		t.Fatal(report)
	}
	output := &limitedOutput{}
	if _, err := output.Write(make([]byte, 4097)); err == nil || !output.exceeded || len(output.data) > 4096 {
		t.Fatal("output unbounded")
	}
	report = (Inspector{Probe: func(context.Context, string, []string) (string, error) { return "", context.DeadlineExceeded }}).Inspect(context.Background(), c)
	if !strings.Contains(byID(t, report, "utility:tar").Message, "timed out") {
		t.Fatal(report)
	}
}
func TestProbeRejectsArbitraryProgramsAndArguments(t *testing.T) {
	for _, args := range []struct {
		name string
		args []string
	}{{"/bin/sh", []string{"--version"}}, {"/usr/bin/mt", []string{"-f", "/dev/nst0", "status"}}, {"/usr/bin/tar", []string{"--create"}}} {
		if _, err := version(context.Background(), args.name, args.args); err == nil {
			t.Fatal("arbitrary utility invocation")
		}
	}
}
func TestInstalledGNUtarVersionWithoutDeviceQueries(t *testing.T) {
	if _, err := os.Stat("/usr/bin/tar"); errors.Is(err, os.ErrNotExist) {
		t.Skip("GNU tar is absent")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := version(ctx, "/usr/bin/tar", []string{"--version"})
	if err != nil {
		t.Fatal(err)
	}
	if variant("tar", output) == "" {
		t.Fatal("installed tar variant unrecognized")
	}
}
func TestPrivateFilesystemPermissionAndSpaceChecks(t *testing.T) {
	c := settings(t)
	c.SpoolReserveBytes = int64(^uint64(0) >> 1)
	report := (Inspector{Probe: knownProbe}).Inspect(context.Background(), c)
	if report.Ready || byID(t, report, "root:spool").Status != "error" {
		t.Fatal("impossible space reserve accepted")
	}
}
func TestDateVersionAndUnknownVariant(t *testing.T) {
	if got := variant("mbuffer", "mbuffer version 20230301"); got != "20230301" {
		t.Fatal(got)
	}
	for _, value := range []string{"mt (GNU cpio) 2.15", "BusyBox v1.36.1", "unrecognized mt"} {
		if variant("mt-st", value) != "" {
			t.Fatal("unknown mt variant accepted")
		}
	}
}
