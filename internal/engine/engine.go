// Package engine executes archive jobs through the simulator. Job ownership,
// cancellation and unsuccessful terminal states belong to the scheduler.
package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/media"
	"github.com/maxya/TapePilot/internal/progress"
	"github.com/maxya/TapePilot/internal/source"
	"github.com/maxya/TapePilot/internal/store"
	"github.com/maxya/TapePilot/internal/stream"
	"github.com/maxya/TapePilot/internal/tape"
	"github.com/maxya/TapePilot/internal/tape/sim"
	"github.com/maxya/TapePilot/internal/volume"
)

var ErrPolicy = errors.New("UNSUPPORTED_POLICY")
var ErrCapacity = errors.New("CAPACITY_BUDGET_EXCEEDED")

type InitOptions struct {
	CartridgeID string `json:"cartridge_id"`
	Generation  string `json:"generation"`
	Confirmed   bool   `json:"confirmed"`
}
type BackupOptions struct {
	SourceID    string   `json:"source_id"`
	Selection   string   `json:"selection"`
	Exclusions  []string `json:"exclusions"`
	SourceLabel string   `json:"source_label"`
}

// InitializeArmed and InitializeJobID are a local CLI capability for exactly one
// job. The HTTP-serving engine must always leave both at their zero values.
type Engine struct {
	Store           *store.Store
	Drive           *sim.Drive
	Sources         []source.Root
	Spool           string
	InitializeArmed bool
	InitializeJobID string
	testHook        func(string) error
}

func (e *Engine) step(name string) error {
	if e.testHook != nil {
		return e.testHook(name)
	}
	return nil
}
func decodeOptions(raw []byte, dest any) error {
	if len(raw) > 16384 {
		return ErrPolicy
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return ErrPolicy
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return ErrPolicy
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return ErrPolicy
		}
		seen[name] = true
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return ErrPolicy
		}
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') {
		return ErrPolicy
	}
	if _, err = d.Token(); err != io.EOF {
		return ErrPolicy
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(dest) != nil {
		return ErrPolicy
	}
	return nil
}
func (e *Engine) Execute(ctx context.Context, j store.Job) (resultErr error) {
	if e.Store == nil || e.Drive == nil || j.State != domain.Preflight {
		return ErrPolicy
	}
	if j.Kind != domain.Initialize && j.Kind != domain.Backup {
		return ErrPolicy
	}
	reporter, err := progress.New(j.ID, e.Store)
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
	v, err := e.Store.GetVolume(ctx, j.VolumeID)
	if err != nil {
		return err
	}
	l, err := e.Drive.Acquire(ctx)
	if errors.Is(err, sim.ErrNoMedia) {
		return store.ErrWaitingMedia
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, l.Close()) }()
	if j.Kind == domain.Initialize {
		return e.initialize(ctx, j, v, l, reporter)
	}
	return e.backup(ctx, j, v, l, reporter)
}
func (e *Engine) running(ctx context.Context, j store.Job, phase string) (store.Job, error) {
	return e.Store.Transition(ctx, j.ID, j.Version, domain.Running, phase, "")
}
func (e *Engine) verifying(ctx context.Context, j store.Job) (store.Job, error) {
	return e.Store.Transition(ctx, j.ID, j.Version, domain.Verifying, "readback", "")
}
func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func (e *Engine) initialize(ctx context.Context, j store.Job, v store.Volume, l *sim.Lease, reporter *progress.Reporter) error {
	var options InitOptions
	if err := decodeOptions(j.Options, &options); err != nil {
		return err
	}
	if !e.InitializeArmed || e.InitializeJobID != j.ID || !options.Confirmed || options.Generation == "" || v.Kind != "MANAGED" || v.State != domain.BlankCandidate {
		return ErrPolicy
	}
	status, err := l.Status(ctx)
	if err != nil {
		return err
	}
	if status.CartridgeID != options.CartridgeID || status.Generation != options.Generation {
		return store.ErrWaitingMedia
	}
	if status.Uncertain || status.CompletedFiles != 0 || status.WriteProtected {
		return ErrPolicy
	}
	label := media.Volume{Magic: tape.VolumeMagic, FormatMajor: 1, FormatMinor: 0, VolumeID: v.ID, LabelAtInitialization: v.Label, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), CreatedByVersion: "0.1.0-alpha.simulator", MediaGeneration: v.Generation, NativeCapacityBytes: &status.CapacityBytes, TapeProfile: media.StandardProfile(), ArchiveFormat: "pax-tar", HashAlgorithm: "sha256"}
	raw, err := media.EncodeVolume(label)
	if err != nil {
		return err
	}
	if int64(len(raw)) > status.CapacityBytes {
		return ErrCapacity
	}
	j, err = e.running(ctx, j, "initialization")
	if err != nil {
		return err
	}
	intent := uuid.NewString()
	drive, err := l.Identity(ctx)
	if err != nil {
		return err
	}
	if err = e.step("before_intent"); err != nil {
		return err
	}
	if err = e.Store.BeginWriteIntent(ctx, store.WriteIntent{ID: intent, JobID: j.ID, VolumeID: v.ID, DriveID: drive, Phase: "initialization"}); err != nil {
		return err
	}
	if err = e.step("after_intent"); err != nil {
		return err
	}
	if err = e.Store.MarkWriteOpened(ctx, j.ID); err != nil {
		return err
	}
	if err = e.Store.RecordExpectedHeader(ctx, j.ID, digest(raw), int64(len(raw))); err != nil {
		return err
	}
	permit, err := l.AuthorizeInitialization(ctx, intent, true)
	if err != nil {
		return err
	}
	if err = e.writeMetadata(ctx, l, permit, raw, "header", reporter); err != nil {
		return err
	}
	j, err = e.verifying(ctx, j)
	if err != nil {
		return err
	}
	if err = e.step("before_readback"); err != nil {
		return err
	}
	total := int64(len(raw))
	if err = reporter.Phase(ctx, progress.Readback, &total); err != nil {
		return err
	}
	scanned, err := volume.Scan(ctx, reporter.Adapter(l), volume.Visitor{})
	if err != nil {
		return err
	}
	if scanned.Volume.VolumeID != v.ID || scanned.Header.SHA256 != digest(raw) || scanned.Header.Bytes != int64(len(raw)) || scanned.Pairs != 0 || scanned.Proof == nil {
		return store.ErrEvidence
	}
	if err = reporter.Phase(ctx, progress.Publishing, nil); err != nil {
		return err
	}
	if err = e.step("before_publish"); err != nil {
		return err
	}
	return e.Store.PublishInitializedVolume(ctx, j.ID, store.Verification{JobID: j.ID, Basis: "WRITE_READBACK", ExpectedSHA256: digest(raw), ActualSHA256: scanned.Header.SHA256, ExpectedBytes: int64(len(raw)), ActualBytes: scanned.Header.Bytes, IdentityValid: true, FormatValid: true})
}
func (e *Engine) writeMetadata(ctx context.Context, l *sim.Lease, permit tape.WritePermit, raw []byte, phase string, reporter *progress.Reporter) error {
	if err := e.step("before_" + phase + "_open"); err != nil {
		return err
	}
	w, err := l.OpenWriter(ctx, permit)
	if err != nil {
		return err
	}
	if err = e.step("after_" + phase + "_open"); err != nil {
		return errors.Join(err, w.CloseAndFlush())
	}
	_, pumpErr := stream.Pump(ctx, bytes.NewReader(raw), reporter.Writer(w))
	closeErr := w.CloseAndFlush()
	return errors.Join(pumpErr, closeErr, e.step("after_"+phase+"_close"))
}
func (e *Engine) backup(ctx context.Context, j store.Job, v store.Volume, l *sim.Lease, reporter *progress.Reporter) (resultErr error) {
	var options BackupOptions
	if err := decodeOptions(j.Options, &options); err != nil {
		return err
	}
	if options.SourceID == "" || v.Kind != "MANAGED" || v.State != domain.Managed {
		return ErrPolicy
	}
	if options.SourceLabel == "" {
		options.SourceLabel = options.SourceID
	}
	scan, err := volume.Scan(ctx, l, volume.Visitor{})
	if err != nil {
		return err
	}
	if scan.Volume.VolumeID != v.ID {
		return store.ErrWaitingMedia
	}
	if scan.Proof == nil || scan.Header.SHA256 != v.HeaderSHA256 {
		return store.ErrEvidence
	}
	if err = e.Store.CheckEnrollment(ctx, v.ID, scan.Proof.Observation.Drive, scan.Proof.Observation.Profile); err != nil {
		return err
	}
	if err = reporter.Phase(ctx, progress.Planning, nil); err != nil {
		return err
	}
	plan, err := source.Create(ctx, e.Sources, options.SourceID, options.Selection, e.Spool, options.Exclusions, source.DefaultLimits())
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, plan.Close()) }()
	status, err := l.Status(ctx)
	if err != nil {
		return err
	}
	if status.WriteProtected || status.Uncertain {
		return ErrPolicy
	}
	budget := status.CapacityBytes/10*9 + status.CapacityBytes%10*9/10
	remaining := budget - status.UsedBytes
	// Reserve the complete permitted metadata bound for the commit. Actual EOM
	// still remains a failure even when the conservative estimate fits.
	if remaining < 1048576 || plan.EstimatedBytes > remaining-1048576 {
		return ErrCapacity
	}
	previous := scan.LastCommit.SHA256
	selection := options.Selection
	if selection == "" {
		selection = "."
	}
	archiveID := uuid.NewString()
	sequence := scan.Pairs + 1
	var prev *string
	if previous != "" {
		prev = &previous
	}
	commit := media.Commit{Magic: tape.CommitMagic, FormatMajor: 1, FormatMinor: 0, VolumeID: v.ID, VolumeHeaderSHA256: scan.Header.SHA256, Sequence: sequence, ArchiveID: archiveID, JobID: j.ID, SourceLabel: options.SourceLabel, SourceSelection: selection, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), DataFileIndex: int64(scan.Proof.NextIndex), CommitFileIndex: int64(scan.Proof.NextIndex) + 1, DataStreamBytes: tape.RecordBytes, DataRecordCount: 1, DataSHA256: strings.Repeat("0", 64), PreviousCommitSHA256: prev, ArchiveFormat: "pax-tar", RecordBytes: tape.RecordBytes, SourceConsistency: "live-best-effort"}
	if err = commit.Validate(); err != nil {
		return err
	}
	j, err = e.running(ctx, j, "streaming")
	if err != nil {
		return err
	}
	intent := uuid.NewString()
	if err = e.step("before_intent"); err != nil {
		return err
	}
	if err = e.Store.CheckEnrollment(ctx, v.ID, scan.Proof.Observation.Drive, scan.Proof.Observation.Profile); err != nil {
		return err
	}
	if err = e.Store.BeginWriteIntent(ctx, store.WriteIntent{ID: intent, JobID: j.ID, VolumeID: v.ID, DriveID: scan.Proof.Observation.Drive, PreviousCommitSHA256: previous, DataIndex: int64(scan.Proof.NextIndex), CommitIndex: int64(scan.Proof.NextIndex) + 1, Phase: "streaming"}); err != nil {
		return err
	}
	if err = e.step("after_intent"); err != nil {
		return err
	}
	if err = l.PositionAppend(ctx, *scan.Proof); err != nil {
		return err
	}
	if err = e.Store.CheckEnrollment(ctx, v.ID, scan.Proof.Observation.Drive, scan.Proof.Observation.Profile); err != nil {
		return err
	}
	if err = plan.Validate(ctx); err != nil {
		return err
	}
	if err = e.Store.MarkWriteOpened(ctx, j.ID); err != nil {
		return err
	}
	permit, err := l.AuthorizeAppend(ctx, intent)
	if err != nil {
		return err
	}
	if err = e.step("before_data_open"); err != nil {
		return err
	}
	w, err := l.OpenWriter(ctx, permit)
	if err != nil {
		return err
	}
	if err = e.step("after_data_open"); err != nil {
		return errors.Join(err, w.CloseAndFlush())
	}
	if err = reporter.Phase(ctx, progress.Streaming, &plan.EstimatedBytes); err != nil {
		return errors.Join(err, w.CloseAndFlush())
	}
	written, produceErr := stream.Produce(ctx, plan, reporter.Writer(w))
	phaseErr := reporter.Phase(ctx, progress.ClosingData, nil)
	closeErr := w.CloseAndFlush()
	if err = errors.Join(produceErr, closeErr, phaseErr, e.step("after_data_close")); err != nil {
		return err
	}
	commit.DataStreamBytes = written.Bytes
	commit.DataRecordCount = written.Bytes / tape.RecordBytes
	commit.DataSHA256 = hex.EncodeToString(written.SHA256[:])
	raw, err := media.EncodeCommit(commit)
	if err != nil {
		return err
	}
	if err = e.Store.RecordExpectedWrite(ctx, j.ID, commit.DataSHA256, written.Bytes, digest(raw)); err != nil {
		return err
	}
	if err = e.step("before_commit"); err != nil {
		return err
	}
	if err = e.Store.CheckEnrollment(ctx, v.ID, scan.Proof.Observation.Drive, scan.Proof.Observation.Profile); err != nil {
		return err
	}
	permit, err = l.AuthorizeCommit(ctx, intent)
	if err != nil {
		return err
	}
	if err = reporter.Phase(ctx, progress.WritingCommit, nil); err != nil {
		return err
	}
	if err = e.writeMetadata(ctx, l, permit, raw, "commit", reporter); err != nil {
		return err
	}
	j, err = e.verifying(ctx, j)
	if err != nil {
		return err
	}
	if err = e.step("before_readback"); err != nil {
		return err
	}
	members := make([]store.Member, 0, 500)
	flush := func() error {
		if len(members) == 0 {
			return nil
		}
		err := e.Store.StageMembers(ctx, j.ID, members)
		members = members[:0]
		return err
	}
	var actual *volume.Pair
	readbackTotal := status.UsedBytes + written.Bytes + int64(len(raw))
	if err = reporter.Phase(ctx, progress.Readback, &readbackTotal); err != nil {
		return err
	}
	readback, err := volume.Scan(ctx, reporter.Adapter(l), volume.Visitor{Member: func(seq int64, h *tar.Header) error {
		if seq != sequence {
			return nil
		}
		name := strings.TrimSuffix(h.Name, "/")
		parent := path.Dir(name)
		if parent == "." {
			parent = ""
		}
		kind := h.Typeflag
		if kind == tar.TypeRegA {
			kind = tar.TypeReg
		}
		members = append(members, store.Member{Path: name, RawPath: h.Name, ParentPath: parent, Type: string([]byte{kind}), Size: h.Size, Mtime: h.ModTime.UTC().Format(time.RFC3339Nano), Mode: h.Mode, LinkTarget: h.Linkname})
		if len(members) == cap(members) {
			return flush()
		}
		return nil
	}, Pair: func(pair volume.Pair) error {
		if pair.Commit.Sequence == sequence {
			copy := pair
			actual = &copy
		}
		return nil
	}})
	if err != nil {
		return err
	}
	if err = flush(); err != nil {
		return err
	}
	if actual == nil || readback.Proof == nil || readback.Volume.VolumeID != v.ID || actual.Commit.ArchiveID != archiveID || actual.Commit.JobID != j.ID || actual.Data.SHA256 != commit.DataSHA256 || actual.Data.Bytes != written.Bytes || actual.CommitDigest.SHA256 != digest(raw) {
		return store.ErrEvidence
	}
	if err = reporter.Phase(ctx, progress.Publishing, nil); err != nil {
		return err
	}
	if err = e.step("before_publish"); err != nil {
		return err
	}
	index := commit.CommitFileIndex
	return e.Store.PublishArchive(ctx, j.ID, store.Archive{ID: archiveID, VolumeID: v.ID, JobID: j.ID, Sequence: sequence, DataIndex: commit.DataFileIndex, CommitIndex: &index, SourceLabel: options.SourceLabel, CreatedAt: commit.CreatedAt, StreamBytes: written.Bytes, SHA256: commit.DataSHA256, CommitSHA256: digest(raw), Integrity: domain.Verified, IntegrityBasis: "WRITE_READBACK", VerifiedAt: time.Now().UTC().Format(time.RFC3339Nano), RestoreEligible: false}, store.Verification{JobID: j.ID, ArchiveID: archiveID, Basis: "WRITE_READBACK", ExpectedSHA256: commit.DataSHA256, ActualSHA256: actual.Data.SHA256, ActualCommitSHA256: actual.CommitDigest.SHA256, ExpectedBytes: written.Bytes, ActualBytes: actual.Data.Bytes, IdentityValid: true, FormatValid: true})
}
