package sim

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/maxya/TapePilot/internal/tape"
)

var ctx = context.Background()

func newDrive(t *testing.T, f Faults, capacity int64) (*Drive, *Lease) {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "drive"))
	if err != nil {
		t.Fatal(err)
	}
	if err = d.Create("lab", capacity, false); err != nil {
		t.Fatal(err)
	}
	if err = d.Insert("lab"); err != nil {
		t.Fatal(err)
	}
	if err = d.SetFaults(f); err != nil {
		t.Fatal(err)
	}
	l, err := d.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return d, l
}
func initWriter(t *testing.T, l *Lease) tape.RecordWriter {
	t.Helper()
	permit, err := l.AuthorizeInitialization(ctx, "init-intent", true)
	if err != nil {
		t.Fatal(err)
	}
	w, err := l.OpenWriter(ctx, permit)
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func writeFile(t *testing.T, l *Lease, value byte) {
	t.Helper()
	w := initWriter(t, l)
	if err := w.WriteRecord(ctx, bytes.Repeat([]byte{value}, tape.RecordBytes)); err != nil {
		t.Fatal(err)
	}
	if err := w.CloseAndFlush(); err != nil {
		t.Fatal(err)
	}
}
func appendPermit(t *testing.T, l *Lease) tape.WritePermit {
	t.Helper()
	obs, err := l.Observe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	proof := tape.AppendProof{Observation: obs, NextIndex: uint64(len(l.data.Files)), VolumeID: "synthetic-volume", HeaderDigest: [32]byte{1}}
	if err = l.PositionAppend(ctx, proof); err != nil {
		t.Fatal(err)
	}
	p, err := l.AuthorizeAppend(ctx, "append-intent")
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func TestRecordsFilemarksEODAndRestart(t *testing.T) {
	d, l := newDrive(t, Faults{}, 8*tape.RecordBytes)
	writeFile(t, l, 1)
	p := appendPermit(t, l)
	w, err := l.OpenWriter(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteRecord(ctx, bytes.Repeat([]byte{2}, tape.RecordBytes)); err != nil {
		t.Fatal(err)
	}
	if err = w.CloseAndFlush(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	other, err := Open(d.root)
	if err != nil {
		t.Fatal(err)
	}
	if err = other.Insert("lab"); err != nil {
		t.Fatal(err)
	}
	l, err = other.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	r, err := l.OpenReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b := make([]byte, tape.RecordBytes)
	for i, want := range []tape.RecordKind{tape.Data, tape.Filemark, tape.Data, tape.Filemark, tape.EOD, tape.EOD} {
		n, kind, err := r.ReadRecord(ctx, b)
		if err != nil || kind != want {
			t.Fatalf("read %d: %d %d %v", i, n, kind, err)
		}
		if want == tape.Data && (n != tape.RecordBytes || b[0] != byte(i/2+1)) {
			t.Fatal("wrong record")
		}
	}
}
func TestExclusiveLeaseAndBusyHandles(t *testing.T) {
	d, l := newDrive(t, Faults{}, 8*tape.RecordBytes)
	if _, err := d.Acquire(ctx); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	other, err := Open(d.root)
	if err != nil {
		t.Fatal(err)
	}
	if err = other.Insert("lab"); err != nil {
		t.Fatal(err)
	}
	if _, err = other.Acquire(ctx); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	r, err := l.OpenReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Rewind(ctx); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if err := l.Close(); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if err = d.Insert("lab"); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := other.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}
func TestWriteFaultsQuarantineWithoutRetry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fault    Faults
		capacity int64
		records  int
		want     error
	}{
		{"short", Faults{ShortWriteRecord: 1}, 8 * tape.RecordBytes, 1, io.ErrShortWrite},
		{"write", Faults{FailWriteRecord: 1}, 8 * tape.RecordBytes, 1, ErrInjected},
		{"close", Faults{FailClose: true}, 8 * tape.RecordBytes, 1, ErrUncertain},
		{"eom", Faults{}, tape.RecordBytes, 2, ErrEndOfMedia},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, l := newDrive(t, tc.fault, tc.capacity)
			w := initWriter(t, l)
			var result error
			for i := 0; i < tc.records; i++ {
				result = w.WriteRecord(ctx, make([]byte, tape.RecordBytes))
				if result != nil {
					break
				}
			}
			closeErr := w.CloseAndFlush()
			if tc.name == "close" {
				result = closeErr
			}
			if !errors.Is(result, tc.want) {
				t.Fatalf("got %v want %v", result, tc.want)
			}
			if closeErr == nil {
				t.Fatal("uncertain close accepted")
			}
			if err := w.WriteRecord(ctx, make([]byte, tape.RecordBytes)); !errors.Is(err, ErrUncertain) {
				t.Fatal(err)
			}
			if err := l.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(d.root)
			if err != nil {
				t.Fatal(err)
			}
			if err = reopened.Insert("lab"); err != nil {
				t.Fatal(err)
			}
			fresh, err := reopened.Acquire(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer fresh.Close()
			if _, err = fresh.AuthorizeInitialization(ctx, "replay", true); !errors.Is(err, ErrUncertain) {
				t.Fatal(err)
			}
		})
	}
}
func TestPermitsAndPositionFailClosed(t *testing.T) {
	_, l := newDrive(t, Faults{}, 8*tape.RecordBytes)
	defer l.Close()
	if _, err := l.OpenWriter(ctx, nil); err == nil {
		t.Fatal("nil permit")
	}
	if _, err := l.AuthorizeInitialization(ctx, "intent", false); err == nil {
		t.Fatal("unconfirmed init")
	}
	p, err := l.AuthorizeInitialization(ctx, "intent", true)
	if err != nil {
		t.Fatal(err)
	}
	w, err := l.OpenWriter(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteRecord(ctx, make([]byte, tape.RecordBytes)); err != nil {
		t.Fatal(err)
	}
	if err = w.CloseAndFlush(); err != nil {
		t.Fatal(err)
	}
	if _, err = l.OpenWriter(ctx, p); err == nil {
		t.Fatal("reused permit")
	}
	if _, err = l.AuthorizeInitialization(ctx, "intent", true); err == nil {
		t.Fatal("overwrites populated media")
	}
	observation, _ := l.Observe(ctx)
	for _, modify := range []func(*tape.AppendProof){func(p *tape.AppendProof) { p.Observation.Nonce = "stale" }, func(p *tape.AppendProof) { p.NextIndex = 0 }, func(p *tape.AppendProof) { p.Observation.MediaGeneration = "other" }, func(p *tape.AppendProof) { p.Observation.Lease = "expired" }} {
		proof := tape.AppendProof{Observation: observation, NextIndex: 1, VolumeID: "volume", HeaderDigest: [32]byte{1}}
		modify(&proof)
		if err = l.PositionAppend(ctx, proof); err == nil {
			t.Fatal("stale proof accepted")
		}
	}
}
func TestPendingTailReadAndEarlierFilesPreserved(t *testing.T) {
	d, l := newDrive(t, Faults{}, 8*tape.RecordBytes)
	writeFile(t, l, 7)
	p := appendPermit(t, l)
	w, err := l.OpenWriter(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteRecord(ctx, make([]byte, tape.RecordBytes)); err != nil {
		t.Fatal(err)
	}
	// Inject a failed close while retaining a complete but uncommitted raw file.
	d.faults.FailClose = true
	if err = w.CloseAndFlush(); err == nil {
		t.Fatal("close succeeded")
	}
	if err = l.Rewind(ctx); err != nil {
		t.Fatal(err)
	}
	r, err := l.OpenReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b := make([]byte, tape.RecordBytes)
	for _, want := range []tape.RecordKind{tape.Data, tape.Filemark, tape.Data} {
		_, got, err := r.ReadRecord(ctx, b)
		if err != nil || got != want {
			t.Fatal(got, err)
		}
	}
	if _, _, err = r.ReadRecord(ctx, b); !errors.Is(err, ErrUncertain) {
		t.Fatal(err)
	}
	r.Close()
	l.Close()
	raw, err := os.ReadFile(filePath(filepath.Join(d.root, "lab"), 0))
	if err != nil || !bytes.Equal(raw, bytes.Repeat([]byte{7}, tape.RecordBytes)) {
		t.Fatal("earlier archive changed", err)
	}
}
func TestRejectDevicePathsAndSymlinkRoots(t *testing.T) {
	for _, name := range []string{"/dev/nst0", "/dev", "/", "relative"} {
		if _, err := Open(name); err == nil {
			t.Fatal(name)
		}
	}
	root := t.TempDir()
	if err := os.Symlink(root, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(root, "alias", "child")); err == nil {
		t.Fatal("symlink ancestor")
	}
}
func TestMotionReadAndProtectionFaults(t *testing.T) {
	d, l := newDrive(t, Faults{}, 8*tape.RecordBytes)
	writeFile(t, l, 9)
	l.Close()
	d.SetFaults(Faults{FailMotion: true})
	l, err := d.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Rewind(ctx); !errors.Is(err, ErrInjected) {
		t.Fatal(err)
	}
	l.Close()
	for _, fault := range []Faults{{FailReadRecord: 1}, {CorruptReadRecord: 1}} {
		d.SetFaults(fault)
		l, err = d.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		r, err := l.OpenReader(ctx)
		if err != nil {
			t.Fatal(err)
		}
		b := make([]byte, tape.RecordBytes)
		_, _, err = r.ReadRecord(ctx, b)
		if fault.FailReadRecord != 0 && !errors.Is(err, ErrInjected) {
			t.Fatal(err)
		}
		if fault.CorruptReadRecord != 0 && (err != nil || b[0] == 9) {
			t.Fatal("corruption not injected")
		}
		r.Close()
		l.Close()
	}
	if err = d.Create("protected", tape.RecordBytes, true); err != nil {
		t.Fatal(err)
	}
	d.Insert("protected")
	l, err = d.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if _, err = l.OpenWriter(ctx, nil); !errors.Is(err, ErrProtected) {
		t.Fatal(err)
	}
}
func TestCommitAuthorityAndUncommittedTail(t *testing.T) {
	d, l := newDrive(t, Faults{}, 12*tape.RecordBytes)
	writeFile(t, l, 1)
	permit := appendPermit(t, l)
	w, err := l.OpenWriter(ctx, permit)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.WriteRecord(ctx, make([]byte, tape.RecordBytes)); err != nil {
		t.Fatal(err)
	}
	if err = w.CloseAndFlush(); err != nil {
		t.Fatal(err)
	}
	observation, _ := l.Observe(ctx)
	if err = l.PositionAppend(ctx, tape.AppendProof{Observation: observation, NextIndex: 2, VolumeID: "volume", HeaderDigest: [32]byte{1}}); err == nil {
		t.Fatal("append beyond uncommitted data")
	}
	if _, err = l.AuthorizeCommit(ctx, "different-intent"); err == nil {
		t.Fatal("commit accepted different intent")
	}
	permit, err = l.AuthorizeCommit(ctx, "append-intent")
	if err != nil {
		t.Fatal(err)
	}
	w, err = l.OpenWriter(ctx, permit)
	if err != nil {
		t.Fatal(err)
	}
	if err = w.WriteRecord(ctx, make([]byte, tape.RecordBytes)); err != nil {
		t.Fatal(err)
	}
	if err = w.CloseAndFlush(); err != nil {
		t.Fatal(err)
	}
	if _, err = l.AuthorizeCommit(ctx, "second-commit"); err == nil {
		t.Fatal("duplicate commit")
	}
	// A second data file closes, but the process's authority to commit is lost
	// when the lease ends. A new lease may only inspect the uncommitted tail.
	permit = appendPermit(t, l)
	w, err = l.OpenWriter(ctx, permit)
	if err != nil {
		t.Fatal(err)
	}
	w.WriteRecord(ctx, make([]byte, tape.RecordBytes))
	w.CloseAndFlush()
	l.Close()
	fresh, err := d.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if _, err = fresh.AuthorizeCommit(ctx, "replay"); err == nil {
		t.Fatal("commit after restart")
	}
	observation, _ = fresh.Observe(ctx)
	if err = fresh.PositionAppend(ctx, tape.AppendProof{Observation: observation, NextIndex: 4, VolumeID: "volume", HeaderDigest: [32]byte{1}}); err == nil {
		t.Fatal("append beyond crash tail")
	}
}
func TestRecordAlignedTruncationAndExtraFilesFailClosed(t *testing.T) {
	for _, extra := range []bool{false, true} {
		d, l := newDrive(t, Faults{}, 8*tape.RecordBytes)
		w := initWriter(t, l)
		w.WriteRecord(ctx, make([]byte, tape.RecordBytes))
		w.WriteRecord(ctx, make([]byte, tape.RecordBytes))
		w.CloseAndFlush()
		l.Close()
		if extra {
			if err := os.WriteFile(filepath.Join(d.root, "lab", "unexpected.tar"), nil, 0600); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := os.Truncate(filePath(filepath.Join(d.root, "lab"), 0), tape.RecordBytes); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := d.Acquire(ctx); !errors.Is(err, ErrUncertain) {
			t.Fatal("altered completed inventory accepted", err)
		}
	}
}
func TestInvalidRecordAndSmallReadBuffer(t *testing.T) {
	_, l := newDrive(t, Faults{}, 8*tape.RecordBytes)
	w := initWriter(t, l)
	if err := w.WriteRecord(ctx, []byte{1}); err == nil {
		t.Fatal("partial record")
	}
	if err := w.CloseAndFlush(); err == nil {
		t.Fatal("invalid record close")
	}
	l.Rewind(ctx)
	r, err := l.OpenReader(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = r.ReadRecord(ctx, make([]byte, 1)); err == nil {
		t.Fatal("small buffer accepted")
	}
	r.Close()
	l.Close()
}
func TestCrashRetainsPendingIntent(t *testing.T) {
	if root := os.Getenv("TAPEPILOT_SIM_CRASH_ROOT"); root != "" {
		d, err := Open(root)
		if err != nil {
			t.Fatal(err)
		}
		if err = d.Insert("lab"); err != nil {
			t.Fatal(err)
		}
		l, err := d.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		w := initWriter(t, l)
		if err = w.WriteRecord(ctx, make([]byte, tape.RecordBytes)); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}
	d, l := newDrive(t, Faults{}, 8*tape.RecordBytes)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestCrashRetainsPendingIntent$")
	command.Env = append(os.Environ(), "TAPEPILOT_SIM_CRASH_ROOT="+d.root)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	fresh, err := d.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if !fresh.data.Uncertain || fresh.data.Pending == nil {
		t.Fatal("crash lost write intent")
	}
	if _, err = fresh.AuthorizeInitialization(ctx, "replay", true); err == nil {
		t.Fatal("resumed crash write")
	}
}
func TestMotionInvalidatesIssuedPermit(t *testing.T) {
	_, l := newDrive(t, Faults{}, 8*tape.RecordBytes)
	defer l.Close()
	permit, err := l.AuthorizeInitialization(ctx, "intent", true)
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Rewind(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = l.OpenWriter(ctx, permit); err == nil {
		t.Fatal("permit survived intervening motion")
	}
}
