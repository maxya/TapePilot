package linux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/maxya/TapePilot/internal/tape"
)

var ErrWritePermit = errors.New("QUALIFICATION_WRITE_PERMIT_REQUIRED")
var ErrQuarantined = errors.New("QUALIFICATION_MEDIA_QUARANTINED")

// QualificationRequest describes one future operator-supervised job. Neither
// this request nor a read-only profile grants physical write authority.
type QualificationRequest struct {
	JobID, IntentID, VolumeID, OperatorRef                      string
	DriveIdentity, ProfileFingerprint, LeaseID, MediaGeneration string
	NotAfter                                                    time.Time
}

// QualifiedWriteProfiles stays empty until separately reviewed hardware evidence
// and an explicit release decision add a supported write implementation.
func QualifiedWriteProfiles() []string { return []string{} }
func releasedWriteProfile(string) bool { return false }

// BeginWriteQualification cannot create hardware authority in this release.
// There is no configuration switch, environment flag or HTTP bypass.
func BeginWriteQualification(context.Context, Config, QualificationRequest) (*QualificationSession, error) {
	return nil, ErrWritesDisabled
}

type writeKind uint8

const (
	writeHeader writeKind = iota + 1
	writeData
	writeCommit
)

type writeObservation struct {
	Drive, Profile, Lease, Generation, Nonce        string
	VolumeID                                        string
	Header                                          [32]byte
	PreviousCommit                                  *[32]byte
	FileIndex                                       uint64
	BlockIndex                                      int64
	PositionKnown, Online, WriteProtected, CleanEOD bool
	ObservedAt                                      time.Time
}
type writeWitness struct {
	Observation                                       writeObservation
	PositiveBlank, NoRecordedData, BOT, ChainVerified bool
	Pairs                                             int64
}
type writePolicy struct{ OperatorAuthorized, Enrolled, Sealed, Quarantined bool }
type qualificationRecordFD interface {
	WriteRecord([]byte) (int, error)
	Close() error
}

// The future provider must own the exclusive physical lease and use fresh
// observations from the retained drive. It is private, with fake providers used
// by synthetic tests. No production provider currently constructs a session.
type qualificationProvider interface {
	Observe(context.Context) (writeObservation, error)
	Prove(context.Context, writeKind) (writeWitness, error)
	Policy(context.Context, QualificationRequest) (writePolicy, error)
	Intent(context.Context, QualificationRequest, writeKind, uint64, bool) error
	OpenPinned(context.Context, writeObservation) (qualificationRecordFD, error)
	Quarantine(context.Context, QualificationRequest, string) error
	Release() error
}
type QualificationPermit struct {
	session  *QualificationSession
	kind     writeKind
	request  QualificationRequest
	witness  writeWitness
	nonce    string
	expires  time.Time
	consumed bool
}
type QualificationWriteState struct {
	MayHaveOpened bool
	Quarantined   bool
	HeaderClosed  bool
	DataClosed    bool
	CommitClosed  bool
	AcceptedBytes int64
}
type QualificationSession struct {
	mu       sync.Mutex
	request  QualificationRequest
	provider qualificationProvider
	now      func() time.Time
	sealed   bool
	closed   bool
	active   *qualifiedWriter
	permit   *QualificationPermit
	pending  *QualificationPermit
	state    QualificationWriteState
}

func newQualificationSession(request QualificationRequest, provider qualificationProvider, now func() time.Time) (*QualificationSession, error) {
	if provider == nil || now == nil {
		return nil, ErrWritesDisabled
	}
	for _, value := range []string{request.JobID, request.IntentID, request.VolumeID} {
		id, e := uuid.Parse(value)
		if e != nil || id.String() != value {
			return nil, ErrWritePermit
		}
	}
	for _, value := range []string{request.OperatorRef, request.DriveIdentity, request.ProfileFingerprint, request.LeaseID, request.MediaGeneration} {
		if !text(value) {
			return nil, ErrWritePermit
		}
	}
	fingerprint, e := hex.DecodeString(request.ProfileFingerprint)
	if e != nil || len(fingerprint) != 32 {
		return nil, ErrWritePermit
	}
	if request.NotAfter.IsZero() || !request.NotAfter.After(now()) || request.NotAfter.Sub(now()) > 24*time.Hour {
		return nil, ErrWritePermit
	}
	return &QualificationSession{request: request, provider: provider, now: now, sealed: true}, nil
}
func (s *QualificationSession) check(ctx context.Context) error {
	if s == nil || !s.sealed || s.provider == nil || s.now == nil {
		return ErrWritesDisabled
	}
	if s.closed {
		return ErrClosed
	}
	if s.state.Quarantined {
		return ErrQuarantined
	}
	return ctx.Err()
}
func (s *QualificationSession) observationMatches(observation writeObservation) bool {
	request := s.request
	return observation.Drive == request.DriveIdentity && observation.Profile == request.ProfileFingerprint && observation.Lease == request.LeaseID && observation.Generation == request.MediaGeneration && text(observation.Nonce) && observation.PositionKnown && observation.BlockIndex == 0 && observation.Online && !observation.WriteProtected && !observation.ObservedAt.IsZero() && !observation.ObservedAt.After(s.now()) && s.now().Sub(observation.ObservedAt) <= time.Minute
}
func sameWriteEpoch(a, b writeObservation) bool {
	return a.Drive == b.Drive && a.Profile == b.Profile && a.Lease == b.Lease && a.Generation == b.Generation && a.Nonce == b.Nonce && a.VolumeID == b.VolumeID && a.Header == b.Header && samePrevious(a.PreviousCommit, b.PreviousCommit)
}
func samePrevious(a, b *[32]byte) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
func cloneWitness(witness writeWitness) writeWitness {
	if witness.Observation.PreviousCommit != nil {
		previous := *witness.Observation.PreviousCommit
		witness.Observation.PreviousCommit = &previous
	}
	return witness
}
func (s *QualificationSession) policy(ctx context.Context, kind writeKind) error {
	policy, e := s.provider.Policy(ctx, s.request)
	if e != nil {
		return e
	}
	if !policy.OperatorAuthorized || policy.Sealed || policy.Quarantined || kind != writeHeader && !policy.Enrolled {
		return ErrWritePermit
	}
	return nil
}
func (s *QualificationSession) AuthorizeInitialization(ctx context.Context, confirmedSacrificial bool) (*QualificationPermit, error) {
	if s == nil {
		return nil, ErrWritesDisabled
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !confirmedSacrificial {
		return nil, ErrWritePermit
	}
	return s.authorize(ctx, writeHeader)
}
func (s *QualificationSession) AuthorizeAppend(ctx context.Context) (*QualificationPermit, error) {
	if s == nil {
		return nil, ErrWritesDisabled
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authorize(ctx, writeData)
}
func (s *QualificationSession) AuthorizeCommit(ctx context.Context) (*QualificationPermit, error) {
	if s == nil {
		return nil, ErrWritesDisabled
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.authorize(ctx, writeCommit)
}
func (s *QualificationSession) authorize(ctx context.Context, kind writeKind) (permit *QualificationPermit, err error) {
	defer func() {
		if err != nil && kind == writeCommit && s.active == nil && s.state.DataClosed && !s.state.CommitClosed && !s.state.Quarantined {
			err = errors.Join(err, s.quarantine("QUALIFICATION_COMMIT_BLOCKED"))
		}
	}()
	if e := s.check(ctx); e != nil {
		return nil, e
	}
	if s.active != nil {
		return nil, ErrBusy
	}
	if kind == writeCommit {
		if !s.state.DataClosed || s.state.CommitClosed || s.pending == nil {
			return nil, ErrWritePermit
		}
	} else if s.state.MayHaveOpened {
		return nil, ErrWritePermit
	}
	if !s.request.NotAfter.After(s.now()) {
		return nil, ErrWritePermit
	}
	if e := s.policy(ctx, kind); e != nil {
		return nil, e
	}
	var witness writeWitness
	if kind == writeCommit {
		witness = s.pending.witness
		witness.Observation.FileIndex++
		// This boundary derives from this session's checked data close. The
		// fresh provider observation below must still match its entire epoch.
		witness.Observation.ObservedAt = s.now()
	} else {
		var e error
		witness, e = s.provider.Prove(ctx, kind)
		if e != nil {
			return nil, e
		}
	}
	observation, e := s.provider.Observe(ctx)
	if e != nil {
		return nil, e
	}
	if !s.observationMatches(observation) || !s.observationMatches(witness.Observation) || !sameWriteEpoch(observation, witness.Observation) || observation.FileIndex != witness.Observation.FileIndex {
		return nil, ErrWritePermit
	}
	switch kind {
	case writeHeader:
		if !witness.PositiveBlank || !witness.NoRecordedData || !witness.BOT || !observation.CleanEOD || observation.FileIndex != 0 || observation.VolumeID != "" || observation.Header != ([32]byte{}) || observation.PreviousCommit != nil {
			return nil, ErrWritePermit
		}
	case writeData:
		if !witness.ChainVerified || !observation.CleanEOD || observation.VolumeID != s.request.VolumeID || observation.Header == ([32]byte{}) || witness.Pairs < 0 || witness.Pairs > (1<<62)-2 || observation.FileIndex != uint64(witness.Pairs)*2+1 || (witness.Pairs == 0) != (observation.PreviousCommit == nil) {
			return nil, ErrWritePermit
		}
	case writeCommit:
		if observation.VolumeID != s.request.VolumeID || observation.FileIndex%2 != 0 {
			return nil, ErrWritePermit
		}
	default:
		return nil, ErrWritePermit
	}
	if e = s.provider.Intent(ctx, s.request, kind, observation.FileIndex, false); e != nil {
		return nil, e
	}
	nonce := make([]byte, 16)
	if _, e = rand.Read(nonce); e != nil {
		return nil, e
	}
	expires := s.now().Add(time.Minute)
	if s.request.NotAfter.Before(expires) {
		expires = s.request.NotAfter
	}
	permit = &QualificationPermit{session: s, kind: kind, request: s.request, witness: cloneWitness(witness), nonce: hex.EncodeToString(nonce), expires: expires}
	s.permit = permit
	return permit, nil
}

// Open consumes authority once, checks current enrollment and positioning, then
// persists possible-open intent before asking the provider for a write handle.
func (s *QualificationSession) Open(ctx context.Context, permit *QualificationPermit) (record tape.RecordWriter, err error) {
	if s == nil {
		return nil, ErrWritesDisabled
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if err != nil && s.active == nil && s.state.DataClosed && !s.state.CommitClosed && !s.state.Quarantined {
			err = errors.Join(err, s.quarantine("QUALIFICATION_COMMIT_BLOCKED"))
		}
	}()
	if e := s.check(ctx); e != nil {
		return nil, e
	}
	if s.active != nil {
		return nil, ErrBusy
	}
	if permit == nil || permit != s.permit || permit.session != s || permit.consumed || permit.nonce == "" || permit.request != s.request || !permit.expires.After(s.now()) {
		return nil, ErrWritePermit
	}
	permit.consumed = true
	if e := s.policy(ctx, permit.kind); e != nil {
		if s.state.MayHaveOpened {
			return nil, errors.Join(e, s.quarantine("QUALIFICATION_POLICY_CHANGED"))
		}
		return nil, e
	}
	observation, e := s.provider.Observe(ctx)
	if e != nil {
		return nil, e
	}
	if !s.observationMatches(observation) || !sameWriteEpoch(observation, permit.witness.Observation) || observation.FileIndex != permit.witness.Observation.FileIndex || permit.kind != writeCommit && !observation.CleanEOD {
		if s.state.MayHaveOpened {
			return nil, errors.Join(ErrWritePermit, s.quarantine("QUALIFICATION_MEDIA_CHANGED"))
		}
		return nil, ErrWritePermit
	}
	if e = s.provider.Intent(ctx, s.request, permit.kind, observation.FileIndex, true); e != nil {
		return nil, e
	}
	s.state.MayHaveOpened = true
	fd, e := s.provider.OpenPinned(ctx, observation)
	if e != nil || fd == nil {
		if fd != nil {
			e = errors.Join(e, fd.Close())
		}
		if e == nil {
			e = ErrWritePermit
		}
		return nil, errors.Join(e, s.quarantine("QUALIFICATION_OPEN_UNCERTAIN"))
	}
	writer := &qualifiedWriter{session: s, fd: fd, permit: permit, operationContext: ctx}
	s.active = writer
	return writer, nil
}
func (s *QualificationSession) quarantine(reason string) error {
	s.state.Quarantined = true
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.provider.Quarantine(ctx, s.request, reason)
}
func (s *QualificationSession) State() QualificationWriteState {
	if s == nil {
		return QualificationWriteState{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}
func (s *QualificationSession) Close() error {
	if s == nil {
		return ErrWritesDisabled
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sealed || s.provider == nil {
		return ErrWritesDisabled
	}
	if s.closed {
		return nil
	}
	if s.active != nil {
		return ErrBusy
	}
	var e error
	if s.state.DataClosed && !s.state.CommitClosed && !s.state.Quarantined {
		e = s.quarantine("QUALIFICATION_INCOMPLETE")
	}
	s.closed = true
	return errors.Join(e, s.provider.Release())
}
