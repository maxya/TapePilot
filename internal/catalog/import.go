// Package catalog reconstructs read-only archive evidence without write enrollment.
package catalog

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/legacy"
	"github.com/maxya/TapePilot/internal/media"
	"github.com/maxya/TapePilot/internal/progress"
	"github.com/maxya/TapePilot/internal/store"
	"github.com/maxya/TapePilot/internal/tape/sim"
	"github.com/maxya/TapePilot/internal/volume"
)

var ErrPolicy = errors.New("IMPORT_POLICY_REQUIRED")

// ImportOptions carries stable identifiers only. Sources are resolved from the
// host registry, and no media operation accepts an HTTP filesystem pathname.
type ImportOptions struct {
	Mode             string `json:"mode"`
	ExpectedVolumeID string `json:"expected_volume_id,omitempty"`
	SourceID         string `json:"source_id,omitempty"`
	Profile          string `json:"profile,omitempty"`
	Label            string `json:"label,omitempty"`
	VolumeID         string `json:"volume_id,omitempty"`
	Confirmed        bool   `json:"confirmed,omitempty"`
}
type Importer struct {
	Store       *store.Store
	Drive       *sim.Drive
	LegacyRoots []legacy.Root
}

func decodeOptions(raw []byte) (ImportOptions, error) {
	var options ImportOptions
	if len(raw) > 16384 {
		return options, ErrPolicy
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, e := decoder.Token()
	if e != nil || token != json.Delim('{') {
		return options, ErrPolicy
	}
	seen := map[string]bool{}
	for decoder.More() {
		key, e := decoder.Token()
		if e != nil {
			return options, ErrPolicy
		}
		name, ok := key.(string)
		if !ok || seen[name] {
			return options, ErrPolicy
		}
		seen[name] = true
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return options, ErrPolicy
		}
	}
	if token, e = decoder.Token(); e != nil || token != json.Delim('}') {
		return options, ErrPolicy
	}
	if _, e = decoder.Token(); e != io.EOF {
		return options, ErrPolicy
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&options) != nil {
		return options, ErrPolicy
	}
	// Reject case-insensitive aliases accepted by encoding/json.
	for name := range seen {
		switch name {
		case "mode", "expected_volume_id", "source_id", "profile", "label", "volume_id", "confirmed":
		default:
			return options, ErrPolicy
		}
	}
	return options, nil
}
func (i *Importer) Execute(ctx context.Context, job store.Job) (err error) {
	if i.Store == nil || job.Kind != domain.Import || job.State != domain.Preflight {
		return ErrPolicy
	}
	options, e := decodeOptions(job.Options)
	if e != nil {
		return e
	}
	switch options.Mode {
	case "managed":
		if i.Drive == nil || options.SourceID != "" || options.Profile != "" || options.Label != "" || options.VolumeID != "" || options.Confirmed {
			return ErrPolicy
		}
		if options.ExpectedVolumeID != "" {
			if _, e = uuid.Parse(options.ExpectedVolumeID); e != nil {
				return ErrPolicy
			}
		}
	case "legacy":
		if options.ExpectedVolumeID != "" || !options.Confirmed || options.Profile != legacy.SimulatorProfile || options.Label == "" || len(options.Label) > 256 || !utf8.ValidString(options.Label) || strings.ContainsRune(options.Label, 0) {
			return ErrPolicy
		}
		if options.VolumeID != "" {
			if _, e = uuid.Parse(options.VolumeID); e != nil {
				return ErrPolicy
			}
		}
	default:
		return ErrPolicy
	}
	reporter, e := progress.New(job.ID, i.Store)
	if e != nil {
		return e
	}
	if e = reporter.Phase(ctx, progress.IdentityCheck, nil); e != nil {
		return e
	}
	job, e = i.Store.Transition(ctx, job.ID, job.Version, domain.Running, "read_only_import", "")
	if e != nil {
		return e
	}
	if options.Mode == "managed" {
		err = i.managed(ctx, job, options, reporter)
	} else {
		err = i.importLegacy(ctx, job, options, reporter)
	}
	flushCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	err = errors.Join(err, reporter.Flush(flushCtx))
	if err != nil {
		return err
	}
	_, err = i.Store.Transition(ctx, job.ID, job.Version, domain.Succeeded, "import_complete", "")
	return err
}
func (i *Importer) managed(ctx context.Context, job store.Job, options ImportOptions, reporter *progress.Reporter) (err error) {
	lease, e := i.Drive.Acquire(ctx)
	if e != nil {
		return e
	}
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, lease.Close())
		}
	}()
	stage := newStage(i.Store, job.ID)
	defer func() { err = errors.Join(err, stage.discard(ctx)) }()
	var imported string
	if e = reporter.Phase(ctx, progress.Readback, nil); e != nil {
		return e
	}
	scanned, e := volume.Scan(ctx, reporter.Adapter(lease), volume.Visitor{
		Header: func(label media.Volume, sum media.Digest) error {
			if options.ExpectedVolumeID != "" && options.ExpectedVolumeID != label.VolumeID || job.VolumeID != "" && job.VolumeID != label.VolumeID {
				return ErrPolicy
			}
			metadata, e := json.Marshal(label)
			if e != nil {
				return e
			}
			e = i.Store.RegisterImportedVolume(ctx, job.ID, store.Volume{ID: label.VolumeID, Kind: "MANAGED", Label: label.LabelAtInitialization, Generation: label.MediaGeneration, State: domain.Managed, HeaderSHA256: sum.SHA256, CreatedAt: label.CreatedAt}, metadata)
			if e == nil {
				imported = label.VolumeID
			}
			return e
		},
		Member: func(_ int64, h *tar.Header) error { return stage.member(ctx, h, false) },
		Pair: func(pair volume.Pair) error {
			if e := stage.flush(ctx); e != nil {
				return e
			}
			index := pair.Commit.CommitFileIndex
			a := store.Archive{ID: pair.Commit.ArchiveID, VolumeID: pair.Commit.VolumeID, JobID: job.ID, Sequence: pair.Commit.Sequence, DataIndex: pair.Commit.DataFileIndex, CommitIndex: &index, SourceLabel: pair.Commit.SourceLabel, CreatedAt: pair.Commit.CreatedAt, StreamBytes: pair.Data.Bytes, SHA256: pair.Data.SHA256, CommitSHA256: pair.CommitDigest.SHA256, Integrity: domain.Verified, IntegrityBasis: "ON_MEDIA_MANIFEST", VerifiedAt: time.Now().UTC().Format(time.RFC3339Nano), RestoreEligible: false}
			if e := i.Store.PublishImportedArchive(ctx, job.ID, stage.attempt, a); e != nil {
				return e
			}
			stage.reset()
			return nil
		},
	})
	// Compare the observed contiguous prefix to an existing catalog as well.
	// An early clean EOD can remove a whole pair without leaving a malformed TAR.
	if imported != "" {
		if e == nil {
			count, countErr := i.Store.ArchiveCount(ctx, imported)
			if countErr != nil {
				e = countErr
			} else if count > scanned.Pairs {
				e = store.ManagedReadMismatch(job.ID, imported, scanned.Header.SHA256, 2*scanned.Pairs+1, true, store.ErrEvidence)
			}
		} else if (errors.Is(e, volume.ErrTail) && errors.Is(e, media.ErrFormat) && !errors.Is(e, media.ErrRead)) || errors.Is(e, store.ErrCollision) {
			e = store.ManagedReadMismatch(job.ID, imported, scanned.Header.SHA256, 2*scanned.Pairs+1, true, e)
		}
	}
	closeErr := lease.Close()
	closed = true
	err = errors.Join(e, closeErr)
	if err != nil && imported != "" {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		current, getErr := i.Store.GetVolume(cleanup, imported)
		if getErr == nil && current.State != domain.Sealed && current.State != domain.Quarantined {
			err = errors.Join(err, i.Store.SetVolumeState(cleanup, imported, domain.Quarantined))
		}
	}
	return err
}
func (i *Importer) importLegacy(ctx context.Context, job store.Job, options ImportOptions, reporter *progress.Reporter) (err error) {
	binding, e := legacy.Identify(ctx, i.LegacyRoots, options.SourceID, options.Profile, options.Confirmed)
	if e != nil {
		return e
	}
	count, e := legacy.Count(ctx, i.LegacyRoots, binding)
	if e != nil {
		return e
	}
	metadata, e := json.Marshal(binding)
	if e != nil {
		return e
	}
	id := options.VolumeID
	if id == "" {
		id = uuid.NewSHA1(uuid.NameSpaceOID, metadata).String()
	}
	if job.VolumeID != "" && job.VolumeID != id {
		return ErrPolicy
	}
	registered := false
	existing, getErr := i.Store.GetVolume(ctx, id)
	if getErr == nil && existing.Kind == "LEGACY" {
		previous, metadataErr := i.Store.ImportedVolumeMetadata(ctx, id)
		if metadataErr == nil {
			var known legacy.Binding
			if json.Unmarshal(previous, &known) == nil && known == binding {
				registered = true
			}
		} else if !errors.Is(metadataErr, store.ErrNotFound) {
			return metadataErr
		}
	} else if getErr != nil && !errors.Is(getErr, store.ErrNotFound) {
		return getErr
	}
	defer func() {
		if err == nil || !registered {
			return
		}
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		current, getErr := i.Store.GetVolume(cleanup, id)
		if getErr != nil {
			err = errors.Join(err, getErr)
			return
		}
		if current.State != domain.Sealed && current.State != domain.Quarantined {
			err = errors.Join(err, i.Store.SetVolumeState(cleanup, id, domain.Quarantined))
		}
	}()
	stage := newStage(i.Store, job.ID)
	defer func() { err = errors.Join(err, stage.discard(ctx)) }()
	for index := int64(0); index < count; index++ {
		if e = reporter.Phase(ctx, progress.Readback, nil); e != nil {
			return e
		}
		file, e := legacy.Open(ctx, i.LegacyRoots, binding, index)
		if e != nil {
			return e
		}
		sum, scanErr := legacy.Scan(&observedFile{ctx: ctx, reader: file, reporter: reporter}, func(header *tar.Header) error { return stage.member(ctx, header, true) })
		closeErr := file.Close()
		if e = errors.Join(scanErr, closeErr); e != nil {
			return e
		}
		// Classification follows the first complete recognized archive and its
		// checked close. A malformed first file cannot create a legacy volume.
		if index == 0 {
			if e = i.Store.RegisterImportedVolume(ctx, job.ID, store.Volume{ID: id, Kind: "LEGACY", Label: options.Label, State: domain.LegacyReadOnly}, metadata); e != nil {
				return e
			}
			registered = true
		}
		if e = stage.flush(ctx); e != nil {
			return e
		}
		archiveID := uuid.NewSHA1(uuid.MustParse(id), []byte(strings.Join([]string{binding.SourceID, formatIndex(index), sum.SHA256}, ":"))).String()
		archive := store.Archive{ID: archiveID, VolumeID: id, JobID: job.ID, Sequence: index + 1, DataIndex: index, SourceLabel: options.Label, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano), StreamBytes: sum.Bytes, SHA256: sum.SHA256, Integrity: domain.Observed, IntegrityBasis: "OBSERVED_NOW", RestoreEligible: false}
		if e = i.Store.PublishImportedArchive(ctx, job.ID, stage.attempt, archive); e != nil {
			return e
		}
		stage.reset()
	}
	final, e := legacy.Identify(ctx, i.LegacyRoots, options.SourceID, options.Profile, true)
	if e != nil {
		return e
	}
	if final != binding {
		return legacy.ErrIdentity
	}
	finalCount, e := legacy.Count(ctx, i.LegacyRoots, binding)
	if e != nil {
		return e
	}
	if finalCount != count {
		return legacy.ErrIdentity
	}
	return nil
}

type observedFile struct {
	ctx      context.Context
	reader   io.Reader
	reporter *progress.Reporter
}

func (r *observedFile) Read(b []byte) (int, error) {
	n, e := r.reader.Read(b)
	if n > 0 {
		if observed := r.reporter.Read(r.ctx, int64(n)); observed != nil {
			return n, observed
		}
	}
	return n, e
}

type staging struct {
	s            *store.Store
	job, attempt string
	batch        []store.Member
	persisted    bool
}

func newStage(s *store.Store, job string) *staging {
	return &staging{s: s, job: job, attempt: uuid.NewString()}
}
func (s *staging) member(ctx context.Context, h *tar.Header, old bool) error {
	name := strings.TrimSuffix(h.Name, "/")
	if old {
		for strings.HasPrefix(name, "./") {
			name = strings.TrimPrefix(name, "./")
		}
		if name == "" {
			name = "."
		}
	}
	parent := path.Dir(name)
	if parent == "." {
		parent = ""
	}
	kind := h.Typeflag
	if kind == tar.TypeRegA {
		kind = tar.TypeReg
	}
	s.batch = append(s.batch, store.Member{Path: name, RawPath: h.Name, ParentPath: parent, Type: string([]byte{kind}), Size: h.Size, Mtime: h.ModTime.UTC().Format(time.RFC3339Nano), Mode: h.Mode, LinkTarget: h.Linkname})
	if len(s.batch) >= 500 {
		return s.flush(ctx)
	}
	return nil
}
func (s *staging) flush(ctx context.Context) error {
	if len(s.batch) == 0 {
		return nil
	}
	if e := s.s.StageImportMembers(ctx, s.job, s.attempt, s.batch); e != nil {
		return e
	}
	s.persisted = true
	s.batch = s.batch[:0]
	return nil
}
func (s *staging) reset() { s.attempt = uuid.NewString(); s.batch = nil; s.persisted = false }
func (s *staging) discard(ctx context.Context) error {
	if s.persisted {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		defer cancel()
		return s.s.DiscardImportAttempt(cleanup, s.job, s.attempt)
	}
	return nil
}

func formatIndex(index int64) string { return strconv.FormatInt(index, 10) }
