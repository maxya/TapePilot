package volume

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maxya/TapePilot/internal/media"
	"github.com/maxya/TapePilot/internal/tape"
	"github.com/maxya/TapePilot/internal/tape/sim"
)

var testContext = context.Background()

type fixtureAdapter struct {
	files        [][]byte
	reader       *fixtureReader
	observations int
	changed      bool
	closeErr     error
	writes       int
}
type fixtureReader struct {
	adapter      *fixtureAdapter
	file, offset int
	closed       bool
}

func (a *fixtureAdapter) Identity(context.Context) (string, error) { return "synthetic-drive", nil }
func (a *fixtureAdapter) Observe(context.Context) (tape.Observation, error) {
	a.observations++
	nonce := "stable"
	if a.changed && a.observations > 1 {
		nonce = "changed"
	}
	return tape.Observation{Drive: "synthetic-drive", Lease: "exclusive-test-lease", Profile: "simulator", Nonce: nonce}, nil
}
func (a *fixtureAdapter) Rewind(context.Context) error {
	a.reader = &fixtureReader{adapter: a}
	return nil
}
func (a *fixtureAdapter) PositionFile(context.Context, uint64) error {
	return errors.New("unexpected movement")
}
func (a *fixtureAdapter) PositionAppend(context.Context, tape.AppendProof) error {
	return errors.New("unexpected append positioning")
}
func (a *fixtureAdapter) Unload(context.Context) error { return errors.New("unexpected unload") }
func (a *fixtureAdapter) OpenWriter(context.Context, tape.WritePermit) (tape.RecordWriter, error) {
	a.writes++
	return nil, errors.New("unexpected writer")
}
func (a *fixtureAdapter) OpenReader(context.Context) (tape.RecordReader, error) { return a.reader, nil }
func (r *fixtureReader) Close() error                                           { r.closed = true; return r.adapter.closeErr }
func (r *fixtureReader) ReadRecord(ctx context.Context, b []byte) (int, tape.RecordKind, error) {
	if e := ctx.Err(); e != nil {
		return 0, 0, e
	}
	if r.file == len(r.adapter.files) {
		return 0, tape.EOD, nil
	}
	raw := r.adapter.files[r.file]
	if r.offset == len(raw) {
		r.file++
		r.offset = 0
		return 0, tape.Filemark, nil
	}
	n := copy(b, raw[r.offset:])
	if n > tape.RecordBytes {
		n = tape.RecordBytes
	}
	r.offset += n
	if n != tape.RecordBytes {
		return n, 0, io.ErrUnexpectedEOF
	}
	return n, tape.Data, nil
}
func digest(raw []byte) string { value := sha256.Sum256(raw); return hex.EncodeToString(value[:]) }
func testFiles(t *testing.T) ([][]byte, []media.Commit) {
	t.Helper()
	var volume media.Volume
	raw, e := os.ReadFile("../../schemas/fixtures/volume.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &volume); e != nil {
		t.Fatal(e)
	}
	header, e := media.EncodeVolume(volume)
	if e != nil {
		t.Fatal(e)
	}
	files := [][]byte{header}
	var commits []media.Commit
	root := t.TempDir()
	if e = os.WriteFile(filepath.Join(root, "sample"), []byte("independent payload"), 0600); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command("/usr/bin/tar", "--create", "--format=pax", "--blocking-factor=512", "--file=-", "--directory="+root, "--no-recursion", "--null", "--verbatim-files-from", "--files-from=-")
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin"}
	cmd.Stdin = strings.NewReader("sample\x00")
	data, e := cmd.Output()
	if e != nil {
		t.Fatal(e)
	}
	var commit media.Commit
	raw, e = os.ReadFile("../../schemas/fixtures/commit.json")
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &commit); e != nil {
		t.Fatal(e)
	}
	previous := ""
	for sequence := int64(1); sequence <= 2; sequence++ {
		c := commit
		c.VolumeID = volume.VolumeID
		c.VolumeHeaderSHA256 = digest(header)
		c.Sequence = sequence
		c.DataFileIndex = 2*sequence - 1
		c.CommitFileIndex = 2 * sequence
		c.DataStreamBytes = int64(len(data))
		c.DataRecordCount = int64(len(data)) / tape.RecordBytes
		c.DataSHA256 = digest(data)
		if sequence == 2 {
			c.ArchiveID = "6fa69f68-9d52-4542-9614-b19f1074ef50"
			c.JobID = "2f2d805c-e530-4f68-ab29-5cb4c8f63320"
			p := previous
			c.PreviousCommitSHA256 = &p
		}
		encoded, e := media.EncodeCommit(c)
		if e != nil {
			t.Fatal(e)
		}
		previous = digest(encoded)
		files = append(files, append([]byte(nil), data...), encoded)
		commits = append(commits, c)
	}
	return files, commits
}
func TestCompleteScanAndIndependentGNURecovery(t *testing.T) {
	files, commits := testFiles(t)
	adapter := &fixtureAdapter{files: files}
	members, pairs := 0, 0
	result, e := Scan(testContext, adapter, Visitor{Member: func(sequence int64, h *tar.Header) error {
		members++
		if h.Name != "sample" {
			t.Fatal(h.Name)
		}
		return nil
	}, Pair: func(p Pair) error {
		pairs++
		if p.Commit.ArchiveID != commits[pairs-1].ArchiveID {
			t.Fatal("wrong committed identity")
		}
		return nil
	}})
	if e != nil || result.Proof == nil || result.Pairs != 2 || result.Proof.NextIndex != 5 || members != 2 || pairs != 2 || adapter.writes != 0 || !adapter.reader.closed {
		t.Fatalf("%#v %v", result, e)
	}
	descriptor, e := result.Descriptor(time.Now())
	if e != nil || len(descriptor.Archives) != 2 {
		t.Fatal(e)
	}
	// Recover only the numbered files with standard utilities. No database, lease,
	// scanner or application executable participates in these commands.
	recovery := t.TempDir()
	for i, c := range commits {
		archive := filepath.Join(recovery, c.ArchiveID+".tar")
		if e = os.WriteFile(archive, files[2*i+1], 0600); e != nil {
			t.Fatal(e)
		}
		out, e := exec.Command("/usr/bin/sha256sum", archive).Output()
		if e != nil || strings.Fields(string(out))[0] != c.DataSHA256 {
			t.Fatal("independent raw digest mismatch", e)
		}
		out, e = exec.Command("/usr/bin/tar", "--extract", "--to-stdout", "--file="+archive, "--", "sample").Output()
		if e != nil || string(out) != "independent payload" {
			t.Fatal("independent payload recovery failed", e)
		}
		if digest(files[2*i+1]) != c.DataSHA256 {
			t.Fatal("earlier archive altered")
		}
	}
}
func TestMalformedTailsPreserveVerifiedPrefix(t *testing.T) {
	files, commits := testFiles(t)
	for _, which := range []string{"missing_commit", "empty_file", "corrupt_data", "wrong_chain", "duplicate_archive", "extra_tail", "close_failure", "identity_change"} {
		t.Run(which, func(t *testing.T) {
			current := make([][]byte, len(files))
			for i, b := range files {
				current[i] = append([]byte(nil), b...)
			}
			adapter := &fixtureAdapter{files: current}
			wantPairs := int64(1)
			switch which {
			case "missing_commit":
				adapter.files = current[:4]
			case "empty_file":
				adapter.files = append(current[:3], nil)
			case "corrupt_data":
				current[3][512] ^= 1
			case "wrong_chain":
				bad := commits[1]
				x := strings.Repeat("0", 64)
				bad.PreviousCommitSHA256 = &x
				current[4], _ = media.EncodeCommit(bad)
			case "duplicate_archive":
				bad := commits[1]
				bad.ArchiveID = commits[0].ArchiveID
				current[4], _ = media.EncodeCommit(bad)
			case "extra_tail":
				adapter.files = append(current, []byte{})
				wantPairs = 2
			case "close_failure":
				adapter.closeErr = errors.New("injected close")
				wantPairs = 2
			case "identity_change":
				adapter.changed = true
				wantPairs = 2
			}
			var committed int64
			result, e := Scan(testContext, adapter, Visitor{Pair: func(Pair) error { committed++; return nil }})
			if e == nil || result.Proof != nil || result.Pairs != wantPairs || committed != wantPairs || adapter.writes != 0 || !adapter.reader.closed {
				t.Fatalf("prefix=%d commits=%d proof=%v error=%v", result.Pairs, committed, result.Proof, e)
			}
			descriptor, e := result.Descriptor(time.Now())
			if e != nil || len(descriptor.Archives) != int(wantPairs) || len(descriptor.Caveats) < 2 {
				t.Fatal("prefix descriptor lost", e)
			}
		})
	}
}
func TestHeaderOnlyAndVisitorFailure(t *testing.T) {
	files, _ := testFiles(t)
	result, e := Scan(testContext, &fixtureAdapter{files: files[:1]}, Visitor{})
	if e != nil || result.Proof == nil || result.Proof.NextIndex != 1 || result.Proof.PreviousCommit != nil {
		t.Fatal(result, e)
	}
	sentinel := errors.New("staging unavailable")
	adapter := &fixtureAdapter{files: files}
	result, e = Scan(testContext, adapter, Visitor{Member: func(int64, *tar.Header) error { return sentinel }})
	if !errors.Is(e, sentinel) || result.Proof != nil || result.Pairs != 0 || !adapter.reader.closed {
		t.Fatal(result, e)
	}
}

func writeRecords(t *testing.T, lease *sim.Lease, permit tape.WritePermit, raw []byte) {
	t.Helper()
	writer, e := lease.OpenWriter(testContext, permit)
	if e != nil {
		t.Fatal(e)
	}
	for offset := 0; offset < len(raw); offset += tape.RecordBytes {
		if e = writer.WriteRecord(testContext, raw[offset:offset+tape.RecordBytes]); e != nil {
			t.Fatal(e)
		}
	}
	if e = writer.CloseAndFlush(); e != nil {
		t.Fatal(e)
	}
}
func TestSimulatorTwoAppendsRemainIndependentlyRecoverable(t *testing.T) {
	files, commits := testFiles(t)
	root := filepath.Join(t.TempDir(), "drive")
	drive, e := sim.Open(root)
	if e != nil {
		t.Fatal(e)
	}
	if e = drive.Create("lab", 20*tape.RecordBytes, false); e != nil {
		t.Fatal(e)
	}
	if e = drive.Insert("lab"); e != nil {
		t.Fatal(e)
	}
	lease, e := drive.Acquire(testContext)
	if e != nil {
		t.Fatal(e)
	}
	permit, e := lease.AuthorizeInitialization(testContext, "init", true)
	if e != nil {
		t.Fatal(e)
	}
	writeRecords(t, lease, permit, files[0])
	for i := 0; i < 2; i++ {
		result, e := Scan(testContext, lease, Visitor{})
		if e != nil || result.Proof == nil {
			t.Fatal(e)
		}
		if e = lease.PositionAppend(testContext, *result.Proof); e != nil {
			t.Fatal(e)
		}
		intent := fmt.Sprintf("backup-%d", i)
		permit, e = lease.AuthorizeAppend(testContext, intent)
		if e != nil {
			t.Fatal(e)
		}
		writeRecords(t, lease, permit, files[2*i+1])
		permit, e = lease.AuthorizeCommit(testContext, intent)
		if e != nil {
			t.Fatal(e)
		}
		writeRecords(t, lease, permit, files[2*i+2])
	}
	result, e := Scan(testContext, lease, Visitor{})
	if e != nil || result.Pairs != 2 {
		t.Fatal(result, e)
	}
	if e = lease.Close(); e != nil {
		t.Fatal(e)
	}
	// Release application ownership, then use only raw numbered files and GNU tools.
	for i, c := range commits {
		archive := filepath.Join(root, "lab", fmt.Sprintf("%06d.tar", 2*i+1))
		out, e := exec.Command("/usr/bin/sha256sum", archive).Output()
		if e != nil || strings.Fields(string(out))[0] != c.DataSHA256 {
			t.Fatal("stored archive changed", e)
		}
		out, e = exec.Command("/usr/bin/tar", "--extract", "--to-stdout", "--file="+archive, "--", "sample").Output()
		if e != nil || string(out) != "independent payload" {
			t.Fatal("stored archive recovery failed", e)
		}
	}
}
