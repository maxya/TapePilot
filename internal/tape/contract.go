// Package tape defines record-level contracts and simulator-only write authority.
package tape

import (
	"context"
	"time"
)

const (
	RecordBytes    = 262144
	BlockingFactor = 512
	VolumeMember   = "tapepilot/volume.json"
	RecoveryMember = "tapepilot/RECOVERY.txt"
	CommitMember   = "tapepilot/commit.json"
	VolumeMagic    = "tapepilot-volume"
	CommitMagic    = "tapepilot-commit"
)

type Observation struct {
	Drive, Profile, Lease, MediaGeneration, Nonce string
	ObservedAt                                    time.Time
}
type AppendProof struct {
	Observation    Observation
	VolumeID       string
	HeaderDigest   [32]byte
	PreviousCommit *[32]byte
	NextIndex      uint64
}

// WritePermit is sealed. Only simulated write authority is currently issuable.
type WritePermit interface{ writePermit() }
type RecordKind uint8

const (
	Data RecordKind = iota + 1
	Filemark
	EOD
)

type RecordReader interface {
	ReadRecord(context.Context, []byte) (int, RecordKind, error)
	Close() error
}
type RecordWriter interface {
	WriteRecord(context.Context, []byte) error
	CloseAndFlush() error
}
type Adapter interface {
	Identity(context.Context) (string, error)
	Observe(context.Context) (Observation, error)
	Rewind(context.Context) error
	PositionFile(context.Context, uint64) error
	OpenReader(context.Context) (RecordReader, error)
	PositionAppend(context.Context, AppendProof) error
	OpenWriter(context.Context, WritePermit) (RecordWriter, error)
	Unload(context.Context) error
}
