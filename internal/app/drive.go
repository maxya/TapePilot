package app

import (
	"context"
	"errors"
	"io"

	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/media"
	"github.com/maxya/TapePilot/internal/store"
	"github.com/maxya/TapePilot/internal/tape/sim"
	"github.com/maxya/TapePilot/internal/volume"
)

// DriveJob handles operator-requested simulated motion and verification. Each
// operation has a scheduler claim and uses the same lease as backup and restore.
func (r *Runtime) DriveJob(ctx context.Context, j store.Job) (result error) {
	l, err := r.Drive.Acquire(ctx)
	if errors.Is(err, sim.ErrNoMedia) {
		return store.ErrWaitingMedia
	}
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, l.Close()) }()
	if j.Kind == domain.Verify {
		v, err := r.Store.GetVolume(ctx, j.VolumeID)
		if err != nil {
			return err
		}
		if err = l.Rewind(ctx); err != nil {
			return err
		}
		rr, err := l.OpenReader(ctx)
		if err != nil {
			return err
		}
		label, header, readErr := media.ReadVolume(media.NewFileReader(ctx, rr))
		if err = errors.Join(readErr, rr.Close()); err != nil {
			return err
		}
		if label.VolumeID != v.ID {
			return store.ErrWaitingMedia
		}
		if header.SHA256 != v.HeaderSHA256 {
			return store.ErrEvidence
		}
		if err = r.Store.BeginMediaAssessment(ctx, j.ID, store.MediaAssessment{VolumeID: v.ID, HeaderSHA256: header.SHA256}); err != nil {
			return err
		}
	}
	j, err = r.Store.Transition(ctx, j.ID, j.Version, domain.Running, "device_operation", "")
	if err != nil {
		return err
	}
	switch j.Kind {
	case domain.Rewind:
		err = l.Rewind(ctx)
	case domain.Unload:
		err = l.Unload(ctx)
		if err == nil {
			err = r.clearSelection()
		}
	case domain.Inspect:
		_, err = l.Status(ctx)
	case domain.Verify:
		j, err = r.Store.Transition(ctx, j.ID, j.Version, domain.Verifying, "readback", "")
		if err != nil {
			return err
		}
		expected, err := r.Store.GetVolume(ctx, j.VolumeID)
		if err != nil {
			return err
		}
		catalogMismatch := false
		scanned, scanErr := volume.Scan(ctx, l, volume.Visitor{
			Header: func(v media.Volume, h media.Digest) error {
				if v.VolumeID != expected.ID || h.SHA256 != expected.HeaderSHA256 {
					return volume.ErrIdentity
				}
				return nil
			},
			Pair: func(pair volume.Pair) error {
				a, e := r.Store.GetArchive(ctx, pair.Commit.ArchiveID)
				if e != nil {
					if errors.Is(e, store.ErrNotFound) {
						catalogMismatch = true
					}
					return e
				}
				if a.VolumeID != j.VolumeID || a.Sequence != pair.Commit.Sequence || a.DataIndex != pair.Commit.DataFileIndex || (a.CommitIndex == nil || *a.CommitIndex != pair.Commit.CommitFileIndex) || a.SHA256 != pair.Data.SHA256 || a.StreamBytes != pair.Data.Bytes || a.CommitSHA256 != pair.CommitDigest.SHA256 {
					catalogMismatch = true
					return store.ErrEvidence
				}
				return r.Store.CheckpointMediaAssessment(ctx, j.ID, a.ID, a.DataIndex, pair.Data.SHA256, pair.Data.Bytes, pair.CommitDigest.SHA256)
			},
		})
		if scanErr != nil {
			if errors.Is(scanErr, volume.ErrIdentity) || errors.Is(scanErr, volume.ErrFinalization) || scanned.Volume.VolumeID != expected.ID || scanned.Header.SHA256 != expected.HeaderSHA256 {
				return errors.Join(scanErr, r.Store.InvalidateMediaAssessment(context.WithoutCancel(ctx), j.ID))
			}
			confirmed := catalogMismatch || ((errors.Is(scanErr, media.ErrFormat) || errors.Is(scanErr, io.ErrUnexpectedEOF)) && !errors.Is(scanErr, media.ErrRead) && !errors.Is(scanErr, context.Canceled) && !errors.Is(scanErr, context.DeadlineExceeded))
			// A malformed header cannot identify an affected archive. Only a
			// scan that matched the bound header can directly disprove a pair.
			if scanned.Volume.VolumeID != expected.ID || scanned.Header.SHA256 != expected.HeaderSHA256 {
				confirmed = false
			}
			recordErr := r.Store.FailMediaAssessment(context.WithoutCancel(ctx), j.ID, 2*scanned.Pairs+1, confirmed)
			return errors.Join(scanErr, recordErr)
		}
		count, err := r.Store.ArchiveCount(ctx, j.VolumeID)
		if err != nil {
			return err
		}
		if count != scanned.Pairs {
			recordErr := r.Store.FailMediaAssessment(context.WithoutCancel(ctx), j.ID, 2*scanned.Pairs+1, true)
			return errors.Join(store.ErrEvidence, recordErr)
		}
		// The scheduler publishes this staged result only after lease.Close.
		return r.Store.CompleteMediaAssessment(ctx, j.ID)

	default:
		return errors.New("unsupported drive operation")
	}
	if err != nil {
		return err
	}
	_, err = r.Store.Transition(ctx, j.ID, j.Version, domain.Succeeded, "complete", "")
	return err
}
