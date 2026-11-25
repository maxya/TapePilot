package restore

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/legacy"
	"github.com/maxya/TapePilot/internal/media"
	"github.com/maxya/TapePilot/internal/progress"
	"github.com/maxya/TapePilot/internal/store"
	"github.com/maxya/TapePilot/internal/tape/sim"
	"golang.org/x/sys/unix"
)

func (r *Restore) step(phase string) error {
	if r.testHook != nil {
		return r.testHook(phase)
	}
	return nil
}
func strictOptions(raw []byte) (Options, error) {
	var options Options
	if len(raw) > 65536 {
		return options, ErrLimit
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return options, ErrUnsafe
	}
	seen := map[string]bool{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return options, ErrUnsafe
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return options, ErrUnsafe
		}
		switch name {
		case "archive_id", "member_ids", "all", "restore_root_id", "destination_slug", "legacy_source_id", "legacy_profile", "legacy_confirmation":
		default:
			return options, ErrUnsafe
		}
		seen[name] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return options, ErrUnsafe
		}
	}
	if _, err = decoder.Token(); err != nil {
		return options, ErrUnsafe
	}
	if _, err = decoder.Token(); err != io.EOF {
		return options, ErrUnsafe
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&options) != nil {
		return options, ErrUnsafe
	}
	return options, nil
}
func trustedRoot(name string) (*os.Root, error) {
	if !filepath.IsAbs(name) || filepath.Clean(name) != name || name == "/" || name == "/dev" || strings.HasPrefix(name, "/dev/") {
		return nil, ErrDestination
	}
	real, err := filepath.EvalSymlinks(name)
	if err != nil {
		return nil, err
	}
	if real != name {
		return nil, ErrDestination
	}
	return os.OpenRoot(name)
}
func available(file *os.File) (int64, error) {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &stat); err != nil {
		return 0, err
	}
	if stat.Bsize <= 0 {
		return 0, ErrSpace
	}
	if stat.Bavail > uint64(math.MaxInt64)/uint64(stat.Bsize) {
		return math.MaxInt64, nil
	}
	return int64(stat.Bavail) * stat.Bsize, nil
}
func reserve(sum int64, extra ...int64) (int64, error) {
	for _, n := range extra {
		if n < 0 || sum > math.MaxInt64-n {
			return 0, ErrLimit
		}
		sum += n
	}
	return sum, nil
}
func checkSpace(spool, output *os.File, spoolBytes, outputBytes, reserveBytes int64) error {
	spoolNeed, err := reserve(spoolBytes, reserveBytes)
	if err != nil {
		return err
	}
	outputNeed, err := reserve(outputBytes, reserveBytes)
	if err != nil {
		return err
	}
	a, err := spool.Stat()
	if err != nil {
		return err
	}
	b, err := output.Stat()
	if err != nil {
		return err
	}
	sa, ok := a.Sys().(*syscall.Stat_t)
	if !ok {
		return ErrSpace
	}
	sb, ok := b.Sys().(*syscall.Stat_t)
	if !ok {
		return ErrSpace
	}
	if sa.Dev == sb.Dev {
		combined, err := reserve(spoolBytes, outputBytes, reserveBytes)
		if err != nil {
			return err
		}
		free, err := available(spool)
		if err != nil {
			return err
		}
		if free < combined {
			return ErrSpace
		}
		return nil
	}
	free, err := available(spool)
	if err != nil {
		return err
	}
	if free < spoolNeed {
		return ErrSpace
	}
	free, err = available(output)
	if err != nil {
		return err
	}
	if free < outputNeed {
		return ErrSpace
	}
	return nil
}
func (r *Restore) Execute(ctx context.Context, j store.Job) (resultErr error) {
	if os.Geteuid() == 0 {
		return ErrRoot
	}
	if r.Store == nil || j.Kind != domain.Restore || j.State != domain.Preflight {
		return ErrUnsafe
	}
	options, err := strictOptions(j.Options)
	if err != nil {
		return err
	}
	p, err := r.Preview(ctx, options)
	if err != nil {
		return err
	}
	if j.VolumeID != p.VolumeID {
		return ErrUnsafe
	}
	outputRoot, err := trustedRoot(p.root)
	if err != nil {
		return err
	}
	defer outputRoot.Close()
	if _, err = outputRoot.Lstat(options.DestinationSlug); !errors.Is(err, os.ErrNotExist) {
		return ErrDestination
	}
	spoolRoot, err := trustedRoot(r.Spool)
	if err != nil {
		return err
	}
	defer spoolRoot.Close()
	outputDir, err := outputRoot.Open(".")
	if err != nil {
		return err
	}
	defer outputDir.Close()
	spoolDir, err := spoolRoot.Open(".")
	if err != nil {
		return err
	}
	defer spoolDir.Close()
	if err = checkSpace(spoolDir, outputDir, p.SpoolBytes, p.OutputBytes, r.ReserveBytes); err != nil {
		return err
	}
	reporter, err := progress.New(j.ID, r.Store)
	if err != nil {
		return err
	}
	if old, e := r.Store.Progress(ctx, j.ID); e == nil {
		reporter, err = progress.FromSnapshot(old, r.Store)
	} else if !errors.Is(e, store.ErrNotFound) {
		return e
	}
	if err != nil {
		return err
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		resultErr = errors.Join(resultErr, reporter.Flush(flushCtx))
	}()
	if err = reporter.Phase(ctx, progress.IdentityCheck, nil); err != nil {
		return err
	}
	var source io.ReadCloser
	var lease *sim.Lease
	if p.volume.Kind == "MANAGED" {
		if r.Drive == nil {
			return store.ErrWaitingMedia
		}
		lease, err = r.Drive.Acquire(ctx)
		if errors.Is(err, sim.ErrNoMedia) {
			return store.ErrWaitingMedia
		}
		if err != nil {
			return err
		}
		defer func() { resultErr = errors.Join(resultErr, lease.Close()) }()
		if err = lease.Rewind(ctx); err != nil {
			return err
		}
		records, err := lease.OpenReader(ctx)
		if err != nil {
			return err
		}
		label, header, readErr := media.ReadVolume(media.NewFileReader(ctx, records))
		closeErr := records.Close()
		if err = errors.Join(readErr, closeErr); err != nil {
			return err
		}
		if label.VolumeID != p.VolumeID {
			return store.ErrWaitingMedia
		}
		if header.SHA256 != p.volume.HeaderSHA256 {
			return ErrUnsafe
		}
		if err = lease.PositionFile(ctx, uint64(p.archive.DataIndex)); err != nil {
			return err
		}
		records, err = lease.OpenReader(ctx)
		if err != nil {
			return err
		}
		source = &recordFile{Reader: media.NewFileReader(ctx, records), closer: records}
	} else {
		metadata, err := r.Store.ImportedVolumeMetadata(ctx, p.VolumeID)
		if err != nil {
			return err
		}
		var binding legacy.Binding
		if err = json.Unmarshal(metadata, &binding); err != nil {
			return err
		}
		if !options.LegacyConfirmation || options.LegacySourceID != binding.SourceID || options.LegacyProfile != binding.Profile {
			return legacy.ErrPolicy
		}
		fresh, err := legacy.Identify(ctx, r.LegacyRoots, binding.SourceID, binding.Profile, true)
		if err != nil {
			return err
		}
		if fresh != binding {
			return legacy.ErrIdentity
		}
		source, err = legacy.Open(ctx, r.LegacyRoots, binding, p.archive.DataIndex)
		if err != nil {
			return err
		}
	}
	// Every source reader is closed on all subsequent exits before the lease.
	defer func() {
		if source != nil {
			resultErr = errors.Join(resultErr, source.Close())
		}
	}()
	j, err = r.Store.Transition(ctx, j.ID, j.Version, domain.Running, "spooling", "")
	if err != nil {
		return err
	}
	workName := "tapepilot-spool-" + uuid.NewString()
	if err = spoolRoot.Mkdir(workName, 0700); err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, spoolRoot.RemoveAll(workName)) }()
	spool, err := spoolRoot.OpenFile(workName+"/archive.tar", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if spool != nil {
			resultErr = errors.Join(resultErr, spool.Close())
		}
	}()
	if err = reporter.Phase(ctx, progress.Spooling, &p.SpoolBytes); err != nil {
		return err
	}
	if err = r.step("before_spool"); err != nil {
		return err
	}
	sourceMismatch := func() error {
		if p.volume.Kind == "MANAGED" {
			return store.ManagedReadMismatch(j.ID, p.VolumeID, p.volume.HeaderSHA256, p.archive.DataIndex, false, ErrUnsafe)
		}
		return ErrUnsafe
	}
	sum := sha256.New()
	buffer := make([]byte, 262144)
	var copied int64
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		n, readErr := source.Read(buffer)
		if n > 0 {
			if int64(n) > p.SpoolBytes-copied {
				return sourceMismatch()
			}
			free, err := available(spoolDir)
			if err != nil {
				return err
			}
			if free < int64(n)+r.ReserveBytes {
				return ErrSpace
			}
			written, writeErr := spool.Write(buffer[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
			sum.Write(buffer[:n])
			copied += int64(n)
			if err = reporter.Read(ctx, int64(n)); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	closeErr := source.Close()
	source = nil
	if closeErr != nil {
		return closeErr
	}
	expected := hex.EncodeToString(sum.Sum(nil))
	if copied != p.SpoolBytes || expected != p.archive.SHA256 {
		return sourceMismatch()
	}
	if err = spool.Sync(); err != nil {
		return err
	}
	if err = spool.Close(); err != nil {
		spool = nil
		return err
	}
	spool = nil
	spool, err = spoolRoot.OpenFile(workName+"/archive.tar", os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	if err = r.step("after_spool"); err != nil {
		return err
	}
	if err = r.validateSpool(ctx, spool, p); err != nil {
		return err
	}
	if err = r.Store.RecordVerifiedSpool(ctx, j.ID, expected, copied, p.SelectedMembers, p.ExpandedMembers); err != nil {
		return err
	}
	if err = checkSpace(spoolDir, outputDir, 0, p.OutputBytes, r.ReserveBytes); err != nil {
		return err
	}
	stageName := ".tapepilot-restore-" + uuid.NewString()
	if err = outputRoot.Mkdir(stageName, 0700); err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			resultErr = errors.Join(resultErr, outputRoot.RemoveAll(stageName))
		}
	}()
	stage, err := outputRoot.Open(stageName)
	if err != nil {
		return err
	}
	defer stage.Close()
	list, err := spoolRoot.OpenFile(workName+"/selection.nul", os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer list.Close()
	for _, name := range p.order {
		if member, selected := p.selected[name]; selected {
			if _, err = io.WriteString(list, member.RawPath+"\x00"); err != nil {
				return err
			}
		}
	}
	if _, err = list.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err = spool.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err = reporter.Phase(ctx, progress.Extracting, nil); err != nil {
		return err
	}
	if err = r.step("before_extract"); err != nil {
		return err
	}
	if err = extract(ctx, stage, spool, list); err != nil {
		return err
	}
	if err = r.step("after_extract"); err != nil {
		return err
	}
	stageRoot, err := os.OpenRoot(fmt.Sprintf("/proc/self/fd/%d", stage.Fd()))
	if err != nil {
		return err
	}
	err = postcheck(ctx, stageRoot, p)
	rootCloseErr := stageRoot.Close()
	if err = errors.Join(err, rootCloseErr); err != nil {
		return err
	}
	if err = stage.Sync(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = reporter.Phase(ctx, progress.Publishing, nil); err != nil {
		return err
	}
	if err = r.Store.BeginRestorePublication(ctx, j.ID, options.RestoreRootID, options.DestinationSlug); err != nil {
		return err
	}
	if err = r.step("before_rename"); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = unix.Renameat2(int(outputDir.Fd()), stageName, int(outputDir.Fd()), options.DestinationSlug, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	published = true
	if err = outputDir.Sync(); err != nil {
		return err
	}
	if err = r.step("after_rename"); err != nil {
		return err
	}
	return r.Store.CompleteRestorePublication(ctx, j.ID)
}

type recordFile struct {
	io.Reader
	closer interface{ Close() error }
}

func (f *recordFile) Close() error { return f.closer.Close() }
func (r *Restore) validateSpool(ctx context.Context, file *os.File, p Plan) error {
	seen := map[string]bool{}
	visitor := func(h *tar.Header) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		name, err := canonical(h.Name)
		if err != nil {
			return err
		}
		expected, ok := p.all[name]
		if !ok || seen[name] {
			return ErrUnsafe
		}
		kind := h.Typeflag
		if kind == tar.TypeRegA {
			kind = tar.TypeReg
		}
		if expected.RawPath != h.Name || expected.Type != string(kind) || expected.Size != h.Size || expected.LinkTarget != h.Linkname {
			return ErrUnsafe
		}
		if kind == tar.TypeLink {
			target, err := canonical(h.Linkname)
			if err != nil || !seen[target] {
				return ErrUnsafe
			}
		}
		seen[name] = true
		return nil
	}
	var scanned media.Digest
	var err error
	if p.volume.Kind == "LEGACY" {
		scanned, err = legacy.Scan(contextReader{ctx, file}, visitor)
	} else {
		scanned, err = media.Scan(contextReader{ctx, file}, visitor)
	}
	if err != nil {
		return err
	}
	if len(seen) != len(p.all) || scanned.SHA256 != p.archive.SHA256 || scanned.Bytes != p.SpoolBytes {
		return ErrUnsafe
	}
	return nil
}

// extract runs fixed GNU tar arguments with pinned input/stage descriptors.
// Cancellation kills the whole child group and observes its exit.
func extract(ctx context.Context, stage, spool, list *os.File) error {
	cmd := exec.CommandContext(ctx, "/usr/bin/tar", "--extract", "--file=/proc/self/fd/4", "--directory=/proc/self/fd/3", "--no-recursion", "--null", "--verbatim-files-from", "--files-from=/proc/self/fd/5", "--keep-old-files", "--no-same-owner", "--no-same-permissions", "--no-acls", "--no-xattrs", "--no-selinux")
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin"}
	cmd.ExtraFiles = []*os.File{stage, spool, list}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	// Filenames in GNU diagnostics stay private and bounded. The API receives
	// only a stable failure, with no raw child stderr.
	logs := &boundedLog{}
	cmd.Stderr = logs
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		return errors.Join(errors.New("TAR_RESTORE_FAILED"), ctx.Err())
	}
	if logs.wrote {
		return errors.New("TAR_RESTORE_WARNING")
	}
	return nil
}

type boundedLog struct {
	bytes int
	wrote bool
}

func (b *boundedLog) Write(p []byte) (int, error) {
	b.wrote = b.wrote || len(p) > 0
	if b.bytes < 65536 {
		n := len(p)
		if n > 65536-b.bytes {
			n = 65536 - b.bytes
		}
		b.bytes += n
	}
	return len(p), nil
}
func postcheck(ctx context.Context, root *os.Root, p Plan) error {
	allowed := map[string]store.Member{}
	for name, m := range p.selected {
		allowed[name] = m
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if _, exists := allowed[parent]; !exists {
				allowed[parent] = store.Member{Path: parent, Type: string(tar.TypeDir), Mode: 0700}
			}
		}
	}
	seen := map[string]bool{}
	directories := []string{}
	err := fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		expected, ok := allowed[name]
		if !ok {
			return ErrUnsafe
		}
		info, err := root.Lstat(name)
		if err != nil {
			return err
		}
		seen[name] = true
		switch expected.Type {
		case string(tar.TypeDir):
			if !info.IsDir() {
				return ErrUnsafe
			}
			if err = root.Chmod(name, 0700); err != nil {
				return err
			}
			directories = append(directories, name)
		case string(tar.TypeSymlink):
			if info.Mode()&os.ModeSymlink == 0 {
				return ErrUnsafe
			}
			target, err := root.Readlink(name)
			if err != nil || target != expected.LinkTarget {
				return ErrUnsafe
			}
		case string(tar.TypeReg), string(tar.TypeLink):
			if !info.Mode().IsRegular() {
				return ErrUnsafe
			}
			if expected.Type == string(tar.TypeLink) {
				target, err := linkTarget(p.all, name)
				if err != nil {
					return err
				}
				other, err := root.Stat(target)
				if err != nil || !os.SameFile(info, other) {
					return ErrUnsafe
				}
			} else if info.Size() != expected.Size {
				return ErrUnsafe
			}
			if err = root.Chmod(name, 0600); err != nil {
				return err
			}
			f, err := root.Open(name)
			if err != nil {
				return err
			}
			mode := expected.Mode
			if expected.Type == string(tar.TypeLink) {
				target, _ := linkTarget(p.all, name)
				mode = p.all[target].Mode
			}
			chmodErr := f.Chmod(os.FileMode(mode) & 0777)
			syncErr := f.Sync()
			closeErr := f.Close()
			if err = errors.Join(chmodErr, syncErr, closeErr); err != nil {
				return err
			}

		default:
			return ErrUnsafe
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(allowed) {
		return ErrUnsafe
	}
	for i := len(directories) - 1; i >= 0; i-- {
		name := directories[i]
		file, err := root.Open(name)
		if err != nil {
			return err
		}
		chmodErr := file.Chmod(os.FileMode(allowed[name].Mode) & 0777)
		syncErr := file.Sync()
		closeErr := file.Close()
		if err = errors.Join(chmodErr, syncErr, closeErr); err != nil {
			return err
		}

	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(b)
}
