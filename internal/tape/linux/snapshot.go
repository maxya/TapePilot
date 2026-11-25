package linux

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/maxya/TapePilot/internal/tape"
	"golang.org/x/sys/unix"
)

type SnapshotLimits struct {
	MaxFiles     int   `json:"max_files"`
	MaxBytes     int64 `json:"max_bytes"`
	ReserveBytes int64 `json:"reserve_bytes"`
}
type SnapshotFile struct {
	Index   int    `json:"index"`
	Bytes   int64  `json:"bytes"`
	Records int64  `json:"records"`
	SHA256  string `json:"sha256"`
}
type SnapshotReport struct {
	Target             string         `json:"target"`
	PartialPath        string         `json:"partial_path,omitempty"`
	Files              []SnapshotFile `json:"files"`
	Bytes              int64          `json:"bytes"`
	ProfileFingerprint string         `json:"profile_fingerprint"`
	Identity           Identity       `json:"identity"`
	Published          bool           `json:"published"`
}

// Snapshot captures observed raw bytes. It does not assert archive integrity or
// restoration eligibility. The normal importer validates the resulting TARs.
// Only the final complete snapshot is published to the configured target path.
func Snapshot(ctx context.Context, lease *Lease, target string, limits SnapshotLimits) (report SnapshotReport, err error) {
	if lease == nil || !regularPath(target) || limits.MaxFiles < 1 || limits.MaxFiles > 100000 || limits.MaxBytes < 1 || limits.MaxBytes > 64<<40 || limits.ReserveBytes < 0 {
		return report, ErrLimit
	}
	for _, system := range []string{"/dev", "/sys", "/proc"} {
		if target == system || strings.HasPrefix(target, system+"/") {
			return report, ErrProfile
		}
	}
	parent := filepath.Dir(target)
	resolved, e := filepath.EvalSymlinks(parent)
	if e != nil {
		return report, e
	}
	if resolved != parent {
		return report, ErrProfile
	}
	directory, e := os.OpenRoot(parent)
	if e != nil {
		return report, e
	}
	defer directory.Close()
	owner, e := directory.Stat(".")
	if e != nil {
		return report, e
	}
	if !owner.IsDir() || owner.Mode().Perm()&0077 != 0 {
		return report, ErrProfile
	}
	name := filepath.Base(target)
	if _, e = directory.Lstat(name); e == nil {
		return report, os.ErrExist
	} else if !os.IsNotExist(e) {
		return report, e
	}
	nonce := make([]byte, 16)
	if _, e = rand.Read(nonce); e != nil {
		return report, e
	}
	tempName := "." + name + ".partial-" + hex.EncodeToString(nonce)
	if e = directory.Mkdir(tempName, 0700); e != nil {
		return report, e
	}
	partial := filepath.Join(parent, tempName)
	report = SnapshotReport{Target: target, PartialPath: partial, Files: []SnapshotFile{}, ProfileFingerprint: lease.report.ProfileFingerprint, Identity: lease.report.Identity}
	staged, e := directory.OpenRoot(tempName)
	if e != nil {
		return report, e
	}
	defer staged.Close()
	if e = lease.Rewind(ctx); e != nil {
		return report, e
	}
	reader, e := lease.OpenReader(ctx)
	if e != nil {
		return report, e
	}
	readerClosed := false
	defer func() {
		if !readerClosed {
			err = errors.Join(err, reader.Close())
		}
	}()
	buffer := make([]byte, lease.profile.MaxRecordBytes)
	var output *os.File
	hash := sha256.New()
	var current SnapshotFile
	defer func() {
		if output != nil {
			err = errors.Join(err, output.Close())
		}
	}()
	for {
		if e = ctx.Err(); e != nil {
			return report, e
		}
		n, kind, readErr := reader.ReadRecord(ctx, buffer)
		if readErr != nil {
			return report, readErr
		}
		switch kind {
		case tape.Data:
			if n < 1 || n > len(buffer) || int64(n) > limits.MaxBytes-report.Bytes {
				return report, ErrLimit
			}
			if output == nil {
				if len(report.Files) >= limits.MaxFiles {
					return report, ErrLimit
				}
				current = SnapshotFile{Index: len(report.Files)}
				hash.Reset()
				output, e = staged.OpenFile(fmt.Sprintf("%06d.tar", current.Index), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
				if e != nil {
					return report, e
				}
			}
			var disk unix.Statfs_t
			if e = unix.Fstatfs(int(output.Fd()), &disk); e != nil {
				return report, e
			}
			if disk.Bsize <= 0 || disk.Bavail > uint64(^uint64(0)>>1)/uint64(disk.Bsize) {
				return report, ErrLimit
			}
			free := int64(disk.Bavail * uint64(disk.Bsize))
			if free < limits.ReserveBytes || int64(n) > free-limits.ReserveBytes {
				return report, ErrLimit
			}
			written, writeErr := output.Write(buffer[:n])
			if writeErr != nil {
				return report, writeErr
			}
			if written != n {
				return report, errors.New("SHORT_SNAPSHOT_WRITE")
			}
			hash.Write(buffer[:n])
			report.Bytes += int64(n)
			current.Bytes += int64(n)
			current.Records++
		case tape.Filemark:
			if output == nil || current.Bytes == 0 {
				return report, ErrPosition
			}
			e = errors.Join(output.Sync(), output.Close())
			output = nil
			if e != nil {
				return report, e
			}
			current.SHA256 = hex.EncodeToString(hash.Sum(nil))
			report.Files = append(report.Files, current)
		case tape.EOD:
			if output != nil || len(report.Files) == 0 {
				return report, ErrPosition
			}
			readerClosed = true
			if e = reader.Close(); e != nil {
				return report, e
			}
			if _, e = lease.Observe(ctx); e != nil {
				return report, e
			}
			fd, e := staged.Open(".")
			if e != nil {
				return report, e
			}
			e = errors.Join(fd.Sync(), fd.Close())
			if e != nil {
				return report, e
			}
			// Pin the parent descriptor for atomic no-replace publication.
			parentFile, e := directory.Open(".")
			if e != nil {
				return report, e
			}
			defer parentFile.Close()
			currentParent, e := os.Lstat(parent)
			if e != nil {
				return report, e
			}
			if !os.SameFile(owner, currentParent) || !currentParent.IsDir() {
				return report, ErrIdentity
			}
			capturedStage, e := staged.Stat(".")
			if e != nil {
				return report, e
			}
			currentStage, e := directory.Lstat(tempName)
			if e != nil {
				return report, e
			}
			if !os.SameFile(capturedStage, currentStage) || !currentStage.IsDir() {
				return report, ErrIdentity
			}
			if e = unix.Renameat2(int(parentFile.Fd()), tempName, int(parentFile.Fd()), name, unix.RENAME_NOREPLACE); e != nil {
				return report, e
			}
			report.PartialPath = ""
			report.Published = true
			if e = parentFile.Sync(); e != nil {
				return report, e
			}
			return report, nil
		default:
			return report, ErrPosition
		}
	}
}
