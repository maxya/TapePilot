package linux

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/maxya/TapePilot/internal/legacy"
	"github.com/maxya/TapePilot/internal/tape"
)

var testContext = context.Background()

type frame struct {
	raw       []byte
	kind      tape.RecordKind
	err       error
	ambiguous bool
}
type fakeSystem struct {
	identity                      Identity
	status                        Status
	version                       string
	frames                        []frame
	cursor, calls, reads, motions int
	closeAfterRead                bool
	failMotion                    bool
}

func (f *fakeSystem) Inspect(context.Context, Config) (Identity, error) {
	f.calls++
	return f.identity, nil
}
func (f *fakeSystem) MTVersion(context.Context) (string, error) { f.calls++; return f.version, nil }
func (f *fakeSystem) OpenRead(context.Context, Config, Identity) (readDevice, error) {
	f.calls++
	return &fakeDevice{host: f}, nil
}
func (f *fakeSystem) Motion(_ context.Context, _ Config, _ Identity, op string, index uint64) error {
	f.motions++
	if f.failMotion {
		return errors.New("synthetic motion failed")
	}
	f.cursor = 0
	f.status.FileNumber = int64(index)
	f.status.BlockNumber = 0
	f.status.BOT = op == "rewind"
	return nil
}

type fakeDevice struct {
	host   *fakeSystem
	closed bool
}

func (d *fakeDevice) Status(context.Context) (Status, error) { return d.host.status, nil }
func (d *fakeDevice) Close() error {
	d.closed = true
	if d.host.closeAfterRead && d.host.reads > 0 {
		return errors.New("synthetic close failure")
	}
	return nil
}
func (d *fakeDevice) Read(ctx context.Context, b []byte) (int, error) {
	if e := ctx.Err(); e != nil {
		return 0, e
	}
	f := d.host
	f.reads++
	if f.cursor >= len(f.frames) {
		return 0, errors.New("unexpected extra read")
	}
	next := f.frames[f.cursor]
	f.cursor++
	f.status.BOT = false
	f.status.EOF = false
	f.status.EOD = false
	if next.err != nil {
		return 0, next.err
	}
	if next.ambiguous {
		return 0, nil
	}
	switch next.kind {
	case tape.Data:
		f.status.BlockNumber++
		return copy(b, next.raw), nil
	case tape.Filemark:
		f.status.EOF = true
		f.status.FileNumber++
		f.status.BlockNumber = 0
		return 0, nil
	case tape.EOD:
		f.status.EOD = true
		return 0, nil
	}
	return 0, ErrPosition
}
func fakeHost(p ReadProfile) *fakeSystem {
	return &fakeSystem{identity: Identity{StableID: p.Identity, Vendor: p.Vendor, Model: p.Model, Firmware: p.Firmware, KernelRelease: p.KernelRelease, HCTL: "1:0:0:0", Major: 9, Minor: 128, NodeInode: 1}, status: Status{Online: true, WriteProtected: true, BOT: true}, version: p.MTSTVersion}
}
func fixture(t *testing.T) (Config, ReadProfile, *fakeSystem) {
	t.Helper()
	root := t.TempDir()
	lock := filepath.Join(root, "locks")
	if e := os.Mkdir(lock, 0700); e != nil {
		t.Fatal(e)
	}
	p := ReadProfile{ID: "synthetic-only", Identity: "synthetic-wwid", Vendor: "TEST", Model: "SYNTHETIC", Firmware: "1", KernelRelease: "synthetic-kernel", MTSTVersion: "1.7", BlockMode: "variable", MaxRecordBytes: tape.RecordBytes, ExactRecordBytes: 0, FilemarkPolicy: "zero-read-with-gmt-eof", EODPolicy: "zero-read-with-gmt-eod", ReadOnlyApproved: true, QualificationRef: "synthetic fixture, no hardware evidence"}
	path := filepath.Join(root, "profile.json")
	raw, e := json.Marshal(p)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	return Config{Device: "/dev/nst999", ExpectedIdentity: p.Identity, ProfileFile: path, LockDir: lock}, p, fakeHost(p)
}
func approve() OperatorApproval {
	return OperatorApproval{ConfirmReadOnly: true, ConfirmWriteProtect: true}
}
func TestDefaultAndMissingApprovalsNeverReachPhysicalSystem(t *testing.T) {
	c, _, host := fixture(t)
	for _, approval := range []OperatorApproval{{}, {ConfirmReadOnly: true}, {ConfirmWriteProtect: true}} {
		if _, e := acquire(testContext, c, approval, host); !errors.Is(e, ErrApproval) {
			t.Fatal(e)
		}
	}
	if _, e := acquire(testContext, Config{}, approve(), host); !errors.Is(e, ErrProfile) {
		t.Fatal(e)
	}
	if host.calls != 0 {
		t.Fatal("physical system reached without complete opt-in")
	}
	raw, e := os.ReadFile(c.ProfileFile)
	if e != nil {
		t.Fatal(e)
	}
	raw = bytes.Replace(raw, []byte(`"read_only_approved":true`), []byte(`"read_only_approved":false`), 1)
	if e = os.WriteFile(c.ProfileFile, raw, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = acquire(testContext, c, approve(), host); !errors.Is(e, ErrProfile) || host.calls != 0 {
		t.Fatal("unqualified profile reached hardware", e)
	}
}
func TestIdentityProtectionAndUtilitiesFailClosed(t *testing.T) {
	for _, which := range []string{"identity", "firmware", "kernel", "rewind_alias", "unprotected", "fixed_block", "wrong_mt"} {
		t.Run(which, func(t *testing.T) {
			c, _, host := fixture(t)
			switch which {
			case "identity":
				host.identity.StableID = "other"
			case "firmware":
				host.identity.Firmware = "other"
			case "kernel":
				host.identity.KernelRelease = "other"
			case "rewind_alias":
				host.identity.Minor = 0
			case "unprotected":
				host.status.WriteProtected = false
			case "fixed_block":
				host.status.BlockBytes = 512
			case "wrong_mt":
				host.version = "GNU cpio mt"
			}
			if lease, e := acquire(testContext, c, approve(), host); e == nil {
				lease.Close()
				t.Fatal("unsupported environment accepted")
			}
		})
	}
}
func TestExclusiveLeaseReadBoundariesAndWriteDenial(t *testing.T) {
	c, _, host := fixture(t)
	lease, e := acquire(testContext, c, approve(), host)
	if e != nil {
		t.Fatal(e)
	}
	defer lease.Close()
	if _, e = acquire(testContext, c, approve(), host); !errors.Is(e, ErrBusy) {
		t.Fatal("duplicate lease accepted", e)
	}
	before := host.calls
	if _, e = lease.OpenWriter(testContext, nil); !errors.Is(e, ErrWritesDisabled) {
		t.Fatal(e)
	}
	if e = lease.PositionAppend(testContext, tape.AppendProof{}); !errors.Is(e, ErrWritesDisabled) {
		t.Fatal(e)
	}
	if host.calls != before {
		t.Fatal("write denial touched device")
	}
	host.frames = []frame{{raw: make([]byte, 10240), kind: tape.Data}, {kind: tape.Filemark}, {kind: tape.EOD}}
	reader, e := lease.OpenReader(testContext)
	if e != nil {
		t.Fatal(e)
	}
	if e = lease.Rewind(testContext); !errors.Is(e, ErrBusy) {
		t.Fatal("motion allowed with reader open", e)
	}
	if e = lease.Close(); !errors.Is(e, ErrBusy) {
		t.Fatal("lease released with reader open", e)
	}
	buffer := make([]byte, tape.RecordBytes)
	for _, expected := range []tape.RecordKind{tape.Data, tape.Filemark, tape.EOD} {
		_, kind, e := reader.ReadRecord(testContext, buffer)
		if e != nil || kind != expected {
			t.Fatal(kind, e)
		}
	}
	if e = reader.Close(); e != nil {
		t.Fatal(e)
	}
	host.identity.NodeInode++
	if e = lease.Rewind(testContext); !errors.Is(e, ErrIdentity) {
		t.Fatal("retargeted node accepted", e)
	}
}
func TestAmbiguousZeroReadIsNeverEndOfData(t *testing.T) {
	c, _, host := fixture(t)
	host.frames = []frame{{ambiguous: true}}
	lease, e := acquire(testContext, c, approve(), host)
	if e != nil {
		t.Fatal(e)
	}
	defer lease.Close()
	reader, e := lease.OpenReader(testContext)
	if e != nil {
		t.Fatal(e)
	}
	defer reader.Close()
	if _, _, e = reader.ReadRecord(testContext, make([]byte, tape.RecordBytes)); !errors.Is(e, ErrPosition) {
		t.Fatal("ambiguous read treated as filemark/EOD", e)
	}
}
func TestLeaseChildProcess(t *testing.T) {
	config := os.Getenv("TAPEPILOT_SYNTHETIC_LOCK_CONFIG")
	if config == "" {
		return
	}
	var c Config
	raw, e := os.ReadFile(config)
	if e != nil {
		os.Exit(2)
	}
	if json.Unmarshal(raw, &c) != nil {
		os.Exit(3)
	}
	p, e := LoadProfile(c.ProfileFile)
	if e != nil {
		os.Exit(4)
	}
	if lease, e := acquire(testContext, c, approve(), fakeHost(p)); !errors.Is(e, ErrBusy) {
		if lease != nil {
			lease.Close()
		}
		os.Exit(5)
	}
	os.Exit(0)
}
func TestInterprocessLeaseUsesSameHostLock(t *testing.T) {
	c, _, host := fixture(t)
	lease, e := acquire(testContext, c, approve(), host)
	if e != nil {
		t.Fatal(e)
	}
	defer lease.Close()
	raw, e := json.Marshal(c)
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "synthetic-config.json")
	if e = os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	command := exec.Command(executable, "-test.run=^TestLeaseChildProcess$")
	command.Env = append(os.Environ(), "TAPEPILOT_SYNTHETIC_LOCK_CONFIG="+path)
	if raw, e = command.CombinedOutput(); e != nil {
		t.Fatal(string(raw), e)
	}
}
func tarFixture(t *testing.T, value string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	if e := writer.WriteHeader(&tar.Header{Name: "file", Mode: 0600, Size: int64(len(value))}); e != nil {
		t.Fatal(e)
	}
	if _, e := writer.Write([]byte(value)); e != nil {
		t.Fatal(e)
	}
	if e := writer.Close(); e != nil {
		t.Fatal(e)
	}
	return buffer.Bytes()
}
func TestSnapshotAtomicNumberedFilesAndIndependentRecovery(t *testing.T) {
	c, _, host := fixture(t)
	first := tarFixture(t, "first synthetic archive")
	second := tarFixture(t, "second synthetic archive")
	host.frames = []frame{{raw: first, kind: tape.Data}, {kind: tape.Filemark}, {raw: second, kind: tape.Data}, {kind: tape.Filemark}, {kind: tape.EOD}}
	lease, e := acquire(testContext, c, approve(), host)
	if e != nil {
		t.Fatal(e)
	}
	defer lease.Close()
	parent := t.TempDir()
	if e = os.Chmod(parent, 0700); e != nil {
		t.Fatal(e)
	}
	target := filepath.Join(parent, "captured")
	report, e := Snapshot(testContext, lease, target, SnapshotLimits{MaxFiles: 2, MaxBytes: 1 << 20, ReserveBytes: 0})
	if e != nil || !report.Published || report.PartialPath != "" || len(report.Files) != 2 {
		t.Fatal(report, e)
	}
	roots := []legacy.Root{{ID: "captured", Path: target}}
	binding, e := legacy.Identify(testContext, roots, "captured", legacy.SimulatorProfile, true)
	if e != nil {
		t.Fatal(e)
	}
	if count, e := legacy.Count(testContext, roots, binding); e != nil || count != 2 {
		t.Fatal(count, e)
	}
	for index, want := range []string{"first synthetic archive", "second synthetic archive"} {
		file, e := legacy.Open(testContext, roots, binding, int64(index))
		if e != nil {
			t.Fatal(e)
		}
		sum, e := legacy.Scan(file, nil)
		closeErr := file.Close()
		if e != nil || closeErr != nil || sum.SHA256 != report.Files[index].SHA256 {
			t.Fatal(sum, e, closeErr)
		}
		out, e := exec.Command("/usr/bin/tar", "--extract", "--to-stdout", "--file="+filepath.Join(target, fmt.Sprintf("%06d.tar", index)), "--", "file").Output()
		if e != nil || string(out) != want {
			t.Fatal("independent recovery mismatch", e)
		}
	}
	if _, e = Snapshot(testContext, lease, target, SnapshotLimits{MaxFiles: 2, MaxBytes: 1 << 20}); !errors.Is(e, os.ErrExist) {
		t.Fatal("existing target overwritten", e)
	}
}
func TestSnapshotFailureNeverPublishesTarget(t *testing.T) {
	for _, which := range []string{"budget", "ambiguous_eod", "close", "empty_file", "read_error"} {
		t.Run(which, func(t *testing.T) {
			c, _, host := fixture(t)
			raw := tarFixture(t, "synthetic")
			host.frames = []frame{{raw: raw, kind: tape.Data}, {kind: tape.Filemark}, {kind: tape.EOD}}
			limits := SnapshotLimits{MaxFiles: 2, MaxBytes: 1 << 20}
			switch which {
			case "budget":
				limits.MaxBytes = 512
			case "ambiguous_eod":
				host.frames[2] = frame{ambiguous: true}
			case "close":
				host.closeAfterRead = true
			case "empty_file":
				host.frames = []frame{{kind: tape.Filemark}, {kind: tape.EOD}}
			case "read_error":
				host.frames[0].err = errors.New("synthetic EIO")
			}
			lease, e := acquire(testContext, c, approve(), host)
			if e != nil {
				t.Fatal(e)
			}
			defer lease.Close()
			parent := t.TempDir()
			os.Chmod(parent, 0700)
			target := filepath.Join(parent, "captured")
			report, e := Snapshot(testContext, lease, target, limits)
			if e == nil || report.Published {
				t.Fatal("failed snapshot published", report, e)
			}
			if _, e = os.Stat(target); !os.IsNotExist(e) {
				t.Fatal("target created on failure", e)
			}
			if report.PartialPath == "" {
				t.Fatal("investigation copy not reported")
			}
		})
	}
}

func TestReadOnlyMotionAndCancellationAreBounded(t *testing.T) {
	c, _, host := fixture(t)
	lease, e := acquire(testContext, c, approve(), host)
	if e != nil {
		t.Fatal(e)
	}
	defer lease.Close()
	if e = lease.PositionFile(testContext, 3); e != nil || host.motions != 2 || host.status.FileNumber != 3 {
		t.Fatal("qualified positioning failed", host.motions, e)
	}
	if e = lease.Unload(testContext); e != nil || host.motions != 3 {
		t.Fatal("qualified unload failed", e)
	}
	canceled, cancel := context.WithCancel(testContext)
	cancel()
	before := host.calls
	if _, e = lease.OpenReader(canceled); !errors.Is(e, context.Canceled) || host.calls != before {
		t.Fatal("canceled open reached physical system", e)
	}
	if e = lease.PositionFile(testContext, 100001); !errors.Is(e, ErrLimit) {
		t.Fatal("unbounded motion count accepted", e)
	}
	host.frames = []frame{{raw: make([]byte, 512), kind: tape.Data}}
	lease.profile.ExactRecordBytes = 262144
	reader, e := lease.OpenReader(testContext)
	if e != nil {
		t.Fatal(e)
	}
	defer reader.Close()
	if _, _, e = reader.ReadRecord(testContext, make([]byte, 262144)); !errors.Is(e, ErrProfile) {
		t.Fatal("short physical record accepted", e)
	}
}
