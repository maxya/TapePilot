// Package volume validates complete managed volumes without granting writes.
package volume

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/maxya/TapePilot/internal/media"
	"github.com/maxya/TapePilot/internal/tape"
)

var ErrTail = errors.New("INVALID_VOLUME_TAIL")
var ErrIdentity = errors.New("MEDIA_CHANGED")

const MaxPairs = 100000

type Pair struct {
	Commit             media.Commit
	Data, CommitDigest media.Digest
}

// Member may run before its commit has been validated. Consumers must stage
// these rows and publish only on Pair. A later tail error preserves valid pairs.
type Visitor struct {
	Member func(sequence int64, header *tar.Header) error
	Pair   func(Pair) error
}
type Result struct {
	Volume     media.Volume
	Header     media.Digest
	Pairs      int64
	LastCommit media.Digest
	Archives   []media.ArchiveEntry
	Proof      *tape.AppendProof
}

// Scan reads from BOT with one retained reader and no write-permit interaction.
// An error returns the fully validated prefix and never returns an append proof.
func Scan(ctx context.Context, adapter tape.Adapter, visitor Visitor) (result Result, err error) {
	identity, e := adapter.Identity(ctx)
	if e != nil {
		return result, e
	}
	before, e := adapter.Observe(ctx)
	if e != nil {
		return result, e
	}
	if e = adapter.Rewind(ctx); e != nil {
		return result, e
	}
	reader, e := adapter.OpenReader(ctx)
	if e != nil {
		return result, e
	}
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, reader.Close())
		}
		if err != nil {
			result.Proof = nil
		}
	}()
	result.Volume, result.Header, e = media.ReadVolume(media.NewFileReader(ctx, reader))
	if e != nil {
		return result, e
	}
	archiveIDs := make(map[string]bool)
	jobIDs := make(map[string]bool)
	for {
		logical := bufio.NewReader(media.NewFileReader(ctx, reader))
		if _, e = logical.Peek(1); errors.Is(e, media.ErrEOD) {
			break
		}
		if e != nil {
			return result, fmt.Errorf("%w: %w", ErrTail, e)
		}
		if result.Pairs >= MaxPairs {
			return result, fmt.Errorf("%w: pair limit", ErrTail)
		}
		sequence := result.Pairs + 1
		data, e := media.Scan(logical, func(header *tar.Header) error {
			if visitor.Member != nil {
				return visitor.Member(sequence, header)
			}
			return nil
		})
		if e != nil {
			return result, fmt.Errorf("%w: data file %d: %w", ErrTail, 2*sequence-1, e)
		}
		commit, sum, e := media.ReadCommit(media.NewFileReader(ctx, reader))
		if e != nil {
			return result, fmt.Errorf("%w: commit file %d: %w", ErrTail, 2*sequence, e)
		}
		if e = commit.Match(result.Volume.VolumeID, result.Header.SHA256, sequence, result.LastCommit.SHA256, data); e != nil {
			return result, fmt.Errorf("%w: %w", ErrTail, e)
		}
		if archiveIDs[commit.ArchiveID] || jobIDs[commit.JobID] {
			return result, fmt.Errorf("%w: repeated archive or job identity", ErrTail)
		}
		archiveIDs[commit.ArchiveID] = true
		jobIDs[commit.JobID] = true
		pair := Pair{commit, data, sum}
		if visitor.Pair != nil {
			if e = visitor.Pair(pair); e != nil {
				return result, e
			}
		}
		result.Pairs = sequence
		result.LastCommit = sum
		verified := time.Now().UTC().Format(time.RFC3339Nano)
		result.Archives = append(result.Archives, media.ArchiveEntry{ArchiveID: commit.ArchiveID, Sequence: sequence, DataFileIndex: commit.DataFileIndex, CommitFileIndex: commit.CommitFileIndex, DataStreamBytes: data.Bytes, DataSHA256: data.SHA256, CommitSHA256: sum.SHA256, SourceLabel: commit.SourceLabel, CreatedAt: commit.CreatedAt, VerifiedAt: &verified, IntegrityBasis: "ON_MEDIA_MANIFEST"})
	}
	closed = true
	if e = reader.Close(); e != nil {
		return result, e
	}
	after, e := adapter.Observe(ctx)
	if e != nil {
		return result, e
	}
	finalIdentity, e := adapter.Identity(ctx)
	if e != nil {
		return result, e
	}
	if after != before || finalIdentity != identity {
		return result, ErrIdentity
	}
	header, e := decodeDigest(result.Header.SHA256)
	if e != nil {
		return result, e
	}
	proof := tape.AppendProof{Observation: after, VolumeID: result.Volume.VolumeID, HeaderDigest: header, NextIndex: uint64(2*result.Pairs + 1)}
	if result.Pairs > 0 {
		previous, e := decodeDigest(result.LastCommit.SHA256)
		if e != nil {
			return result, e
		}
		proof.PreviousCommit = &previous
	}
	result.Proof = &proof
	return result, nil
}
func decodeDigest(value string) ([32]byte, error) {
	var result [32]byte
	raw, e := hex.DecodeString(value)
	if e != nil || len(raw) != len(result) {
		return result, media.ErrFormat
	}
	copy(result[:], raw)
	return result, nil
}

// Descriptor exports only the scan's checked evidence. A damaged tail still
// permits exporting the earlier complete pairs with an explicit caveat.
func (result Result) Descriptor(now time.Time) (media.Descriptor, error) {
	descriptor, e := media.SimulatorDescriptor(result.Volume, result.Header.SHA256, result.Archives, now)
	if e != nil {
		return descriptor, e
	}
	if result.Proof == nil {
		descriptor.Caveats = append(descriptor.Caveats, "Scan did not establish clean end of data. Later data may be incomplete or unreadable. Appending is prohibited.")
	}
	return descriptor, nil
}
