package linux

import (
	"context"
	"errors"
	"io"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/maxya/TapePilot/internal/tape"
)

type intentCall struct {
	kind   writeKind
	index  uint64
	opened bool
}
type syntheticQualification struct {
	request                                            QualificationRequest
	observation                                        writeObservation
	policy                                             writePolicy
	clock                                              *time.Time
	blank, chain                                       bool
	pairs                                              int64
	intents                                            []intentCall
	opens, writes, closes, releases                    int
	quarantine                                         []string
	openErr, writeErr, closeErr, intentErr, observeErr error
	short                                              bool
	proofChange                                        func(*writeWitness)
}

func (f *syntheticQualification) Observe(context.Context) (writeObservation, error) {
	if f.observeErr != nil {
		return writeObservation{}, f.observeErr
	}
	observed := f.observation
	observed.ObservedAt = *f.clock
	return observed, nil
}
func (f *syntheticQualification) Prove(ctx context.Context, kind writeKind) (writeWitness, error) {
	observed, e := f.Observe(ctx)
	if e != nil {
		return writeWitness{}, e
	}
	proof := writeWitness{Observation: observed, PositiveBlank: f.blank, NoRecordedData: f.blank, BOT: f.blank, ChainVerified: f.chain, Pairs: f.pairs}
	if f.proofChange != nil {
		f.proofChange(&proof)
	}
	return proof, nil
}
func (f *syntheticQualification) Policy(context.Context, QualificationRequest) (writePolicy, error) {
	return f.policy, nil
}
func (f *syntheticQualification) Intent(_ context.Context, request QualificationRequest, kind writeKind, index uint64, opened bool) error {
	if request != f.request {
		return ErrWritePermit
	}
	if f.intentErr != nil {
		return f.intentErr
	}
	f.intents = append(f.intents, intentCall{kind, index, opened})
	return nil
}
func (f *syntheticQualification) OpenPinned(_ context.Context, observed writeObservation) (qualificationRecordFD, error) {
	f.opens++
	if len(f.intents) == 0 || !f.intents[len(f.intents)-1].opened || f.intents[len(f.intents)-1].index != observed.FileIndex {
		return nil, errors.New("writer opened before durable intent")
	}
	if f.openErr != nil {
		return nil, f.openErr
	}
	return &syntheticFD{host: f, kind: f.intents[len(f.intents)-1].kind}, nil
}
func (f *syntheticQualification) Quarantine(_ context.Context, request QualificationRequest, code string) error {
	if request != f.request {
		return ErrWritePermit
	}
	f.quarantine = append(f.quarantine, code)
	return nil
}
func (f *syntheticQualification) Release() error { f.releases++; return nil }

type syntheticFD struct {
	host *syntheticQualification
	kind writeKind
}

func (fd *syntheticFD) WriteRecord(record []byte) (int, error) {
	f := fd.host
	f.writes++
	if len(record) != tape.RecordBytes {
		return 0, ErrProfile
	}
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.short {
		return len(record) - 512, nil
	}
	return len(record), nil
}
func (fd *syntheticFD) Close() error {
	f := fd.host
	f.closes++
	if f.closeErr != nil {
		return f.closeErr
	}
	f.observation.FileIndex++
	if fd.kind == writeData {
		f.observation.CleanEOD = false
	} else {
		f.observation.CleanEOD = true
	}
	return nil
}
func qualificationFixture(t *testing.T, blank bool) (*QualificationSession, *syntheticQualification, *time.Time) {
	t.Helper()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	request := QualificationRequest{JobID: "2f2d805c-e530-4f68-ab29-5cb4c8f63319", IntentID: "11111111-1111-4111-8111-111111111111", VolumeID: "c8dc1492-b9f0-4e65-8c5b-24b2d89e37ab", OperatorRef: "synthetic-only", DriveIdentity: "synthetic-drive", ProfileFingerprint: strings.Repeat("a", 64), LeaseID: "synthetic-lease", MediaGeneration: "synthetic-media-epoch", NotAfter: now.Add(5 * time.Minute)}
	observation := writeObservation{Drive: request.DriveIdentity, Profile: request.ProfileFingerprint, Lease: request.LeaseID, Generation: request.MediaGeneration, Nonce: "fresh-synthetic-observation", VolumeID: request.VolumeID, Header: [32]byte{1}, FileIndex: 1, BlockIndex: 0, PositionKnown: true, Online: true, CleanEOD: true, ObservedAt: now}
	if blank {
		observation.VolumeID = ""
		observation.Header = [32]byte{}
		observation.FileIndex = 0
	}
	provider := &syntheticQualification{request: request, observation: observation, policy: writePolicy{OperatorAuthorized: true, Enrolled: true}, clock: &now, blank: blank, chain: !blank}
	session, e := newQualificationSession(request, provider, func() time.Time { return now })
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { session.Close() })
	return session, provider, &now
}
func TestNoExecutableHardwareWriteAuthorityExists(t *testing.T) {
	if len(QualifiedWriteProfiles()) != 0 {
		t.Fatal("hardware write profile promoted without qualification")
	}
	if _, e := BeginWriteQualification(testContext, Config{Device: "/dev/nst999"}, QualificationRequest{}); !errors.Is(e, ErrWritesDisabled) {
		t.Fatal("configuration armed hardware", e)
	}
	if _, e := openQualifiedRecordFile(testContext, Config{Device: "/dev/nst999"}, Identity{}, &hardwareWriteAuthority{profile: "claimed-qualified"}); !errors.Is(e, ErrWritesDisabled) {
		t.Fatal("forged authority reached physical open", e)
	}
	var zero QualificationSession
	if _, e := zero.AuthorizeAppend(testContext); !errors.Is(e, ErrWritesDisabled) {
		t.Fatal("zero session granted authority", e)
	}
	// P5's existing physical read-only adapter remains permanently non-writing.
	var readOnly Lease
	if _, e := readOnly.OpenWriter(testContext, nil); !errors.Is(e, ErrWritesDisabled) {
		t.Fatal(e)
	}
}
func TestSacrificialConfirmationAndPositiveBlankProofBothRequired(t *testing.T) {
	session, provider, _ := qualificationFixture(t, true)
	if _, e := session.AuthorizeInitialization(testContext, false); !errors.Is(e, ErrWritePermit) {
		t.Fatal("confirmation bypassed", e)
	}
	provider.blank = false
	if _, e := session.AuthorizeInitialization(testContext, true); !errors.Is(e, ErrWritePermit) {
		t.Fatal("blank proof bypassed", e)
	}
	provider.blank = true
	permit, e := session.AuthorizeInitialization(testContext, true)
	if e != nil {
		t.Fatal(e)
	}
	writer, e := session.Open(testContext, permit)
	if e != nil {
		t.Fatal(e)
	}
	if e = writer.WriteRecord(testContext, make([]byte, tape.RecordBytes)); e != nil {
		t.Fatal(e)
	}
	if e = writer.CloseAndFlush(); e != nil {
		t.Fatal(e)
	}
	state := session.State()
	if !state.HeaderClosed || state.DataClosed || state.CommitClosed || provider.opens != 1 {
		t.Fatal(state)
	}
	if _, e = session.AuthorizeCommit(testContext); !errors.Is(e, ErrWritePermit) {
		t.Fatal("header close permitted commit", e)
	}
}
func TestAppendProofEnrollmentAndFreshScopePreventWriterOpen(t *testing.T) {
	tests := []string{"wrong_uuid", "header", "previous", "index", "generation", "profile", "drive", "lease", "nonce", "unknown_position", "protected", "sealed", "revoked", "unclean_tail", "expired"}
	for _, which := range tests {
		t.Run(which, func(t *testing.T) {
			session, provider, now := qualificationFixture(t, false)
			permit, e := session.AuthorizeAppend(testContext)
			if e != nil {
				t.Fatal(e)
			}
			switch which {
			case "wrong_uuid":
				provider.observation.VolumeID = "other"
			case "header":
				provider.observation.Header[0]++
			case "previous":
				x := [32]byte{2}
				provider.observation.PreviousCommit = &x
			case "index":
				provider.observation.FileIndex += 2
			case "generation":
				provider.observation.Generation = "changed"
			case "profile":
				provider.observation.Profile = "changed"
			case "drive":
				provider.observation.Drive = "changed"
			case "lease":
				provider.observation.Lease = "changed"
			case "nonce":
				provider.observation.Nonce = "changed"
			case "unknown_position":
				provider.observation.PositionKnown = false
			case "protected":
				provider.observation.WriteProtected = true
			case "sealed":
				provider.policy.Sealed = true
			case "revoked":
				provider.policy.Enrolled = false
			case "unclean_tail":
				provider.observation.CleanEOD = false
			case "expired":
				*now = now.Add(2 * time.Minute)
			}
			if _, e = session.Open(testContext, permit); e == nil || provider.opens != 0 {
				t.Fatal("unsafe scope opened writer", provider.opens, e)
			}
		})
	}
}
func TestDataCommitAreDistinctAndEveryOpenRechecksEnrollment(t *testing.T) {
	session, provider, _ := qualificationFixture(t, false)
	permit, e := session.AuthorizeAppend(testContext)
	if e != nil {
		t.Fatal(e)
	}
	writer, e := session.Open(testContext, permit)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = session.AuthorizeCommit(testContext); !errors.Is(e, ErrBusy) {
		t.Fatal("commit authorized before checked data close", e)
	}
	if e = writer.WriteRecord(testContext, make([]byte, tape.RecordBytes)); e != nil {
		t.Fatal(e)
	}
	if e = writer.CloseAndFlush(); e != nil {
		t.Fatal(e)
	}
	commit, e := session.AuthorizeCommit(testContext)
	if e != nil {
		t.Fatal(e)
	}
	provider.policy.Enrolled = false
	if _, e = session.Open(testContext, commit); e == nil || provider.opens != 1 || !session.State().Quarantined {
		t.Fatal("revoked commit opened", e)
	}
}
func TestSuccessfulWritesOnlyRecordAcceptanceAndCheckedClose(t *testing.T) {
	session, provider, _ := qualificationFixture(t, false)
	data, e := session.AuthorizeAppend(testContext)
	if e != nil {
		t.Fatal(e)
	}
	writer, e := session.Open(testContext, data)
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		if e = writer.WriteRecord(testContext, make([]byte, tape.RecordBytes)); e != nil {
			t.Fatal(e)
		}
	}
	if e = writer.CloseAndFlush(); e != nil {
		t.Fatal(e)
	}
	commit, e := session.AuthorizeCommit(testContext)
	if e != nil {
		t.Fatal(e)
	}
	writer, e = session.Open(testContext, commit)
	if e != nil {
		t.Fatal(e)
	}
	if e = writer.WriteRecord(testContext, make([]byte, tape.RecordBytes)); e != nil {
		t.Fatal(e)
	}
	if e = writer.CloseAndFlush(); e != nil {
		t.Fatal(e)
	}
	if e = writer.CloseAndFlush(); e != nil {
		t.Fatal(e)
	}
	state := session.State()
	if !state.DataClosed || !state.CommitClosed || state.Quarantined || state.AcceptedBytes != 3*tape.RecordBytes || provider.opens != 2 || provider.writes != 3 || provider.closes != 2 {
		t.Fatal("record or close replay", state, provider.opens, provider.writes, provider.closes)
	}
	if e = session.Close(); e != nil {
		t.Fatal(e)
	}
	if e = session.Close(); e != nil || provider.releases != 1 {
		t.Fatal("lease released twice", e)
	}
}
func TestWriteFailuresQuarantineWithoutRetries(t *testing.T) {
	for _, which := range []string{"short", "eom", "write_error", "close_error", "cancel_before_record", "cancel_after_record", "invalid_record", "open_error", "zero_records"} {
		t.Run(which, func(t *testing.T) {
			session, provider, _ := qualificationFixture(t, false)
			permit, e := session.AuthorizeAppend(testContext)
			if e != nil {
				t.Fatal(e)
			}
			if which == "open_error" {
				provider.openErr = errors.New("synthetic open failure")
				if _, e = session.Open(testContext, permit); e == nil || !session.State().Quarantined {
					t.Fatal("open uncertainty lost", e)
				}
				return
			}
			ctx, cancel := context.WithCancel(testContext)
			defer cancel()
			writer, e := session.Open(ctx, permit)
			if e != nil {
				t.Fatal(e)
			}
			switch which {
			case "short":
				provider.short = true
			case "eom":
				provider.writeErr = syscall.ENOSPC
			case "write_error":
				provider.writeErr = io.ErrClosedPipe
			case "close_error":
				provider.closeErr = errors.New("synthetic close failure")
			case "cancel_before_record":
				cancel()
			}
			record := make([]byte, tape.RecordBytes)
			if which == "invalid_record" {
				record = record[:512]
			}
			var writeErr error
			if which != "zero_records" {
				writeErr = writer.WriteRecord(ctx, record)
			}
			if which == "cancel_after_record" {
				cancel()
			}
			if closeErr := writer.CloseAndFlush(); closeErr == nil && writeErr == nil {
				t.Fatal("uncertain write reported clean")
			}
			before := provider.writes
			if e = writer.WriteRecord(testContext, make([]byte, tape.RecordBytes)); e == nil || provider.writes != before {
				t.Fatal("uncertain write retried", e)
			}
			if _, e = session.AuthorizeCommit(testContext); e == nil {
				t.Fatal("commit issued for failed data")
			}
			writer.CloseAndFlush()
			if !session.State().Quarantined || provider.closes != 1 || len(provider.quarantine) == 0 {
				t.Fatal("failure or close replay", session.State(), provider.closes)
			}
		})
	}
}
func TestCrashTailAndCommitFailureNeverReplayData(t *testing.T) {
	session, provider, _ := qualificationFixture(t, false)
	permit, e := session.AuthorizeAppend(testContext)
	if e != nil {
		t.Fatal(e)
	}
	writer, e := session.Open(testContext, permit)
	if e != nil {
		t.Fatal(e)
	}
	if e = writer.WriteRecord(testContext, make([]byte, tape.RecordBytes)); e != nil {
		t.Fatal(e)
	}
	if e = writer.CloseAndFlush(); e != nil {
		t.Fatal(e)
	}
	if e = session.Close(); e != nil {
		t.Fatal(e)
	}
	if !session.State().Quarantined || provider.opens != 1 {
		t.Fatal("uncommitted tail remained appendable")
	}
	fresh, other, _ := qualificationFixture(t, false)
	other.observation.CleanEOD = false
	other.chain = false
	if _, e = fresh.AuthorizeAppend(testContext); e == nil || other.opens != 0 {
		t.Fatal("restart appended beyond uncommitted tail", e)
	}
}
func TestWitnessDigestCopiesAndExpiredCommitAreFailClosed(t *testing.T) {
	session, provider, now := qualificationFixture(t, false)
	previous := [32]byte{3}
	provider.observation.PreviousCommit = &previous
	provider.observation.FileIndex = 3
	provider.pairs = 1
	permit, e := session.AuthorizeAppend(testContext)
	if e != nil {
		t.Fatal(e)
	}
	previous[0]++
	if _, e = session.Open(testContext, permit); e == nil || provider.opens != 0 {
		t.Fatal("mutable digest changed sealed proof", e)
	}
	session, provider, now = qualificationFixture(t, false)
	permit, e = session.AuthorizeAppend(testContext)
	if e != nil {
		t.Fatal(e)
	}
	writer, e := session.Open(testContext, permit)
	if e != nil {
		t.Fatal(e)
	}
	writer.WriteRecord(testContext, make([]byte, tape.RecordBytes))
	if e = writer.CloseAndFlush(); e != nil {
		t.Fatal(e)
	}
	*now = now.Add(6 * time.Minute)
	if _, e = session.AuthorizeCommit(testContext); e == nil || !session.State().Quarantined || provider.opens != 1 {
		t.Fatal("expired uncommitted tail left eligible", e)
	}
}

func TestStaleProofAndReusedPermitNeverOpenAnotherWriter(t *testing.T) {
	session, provider, now := qualificationFixture(t, false)
	provider.proofChange = func(proof *writeWitness) { proof.Observation.ObservedAt = now.Add(-2 * time.Minute) }
	if _, e := session.AuthorizeAppend(testContext); e == nil || provider.opens != 0 {
		t.Fatal("stale full-scan proof accepted", e)
	}
	provider.proofChange = nil
	old, e := session.AuthorizeAppend(testContext)
	if e != nil {
		t.Fatal(e)
	}
	current, e := session.AuthorizeAppend(testContext)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = session.Open(testContext, old); e == nil || provider.opens != 0 {
		t.Fatal("replaced permit remained usable", e)
	}
	writer, e := session.Open(testContext, current)
	if e != nil {
		t.Fatal(e)
	}
	if e = writer.WriteRecord(testContext, make([]byte, tape.RecordBytes)); e != nil {
		t.Fatal(e)
	}
	if e = writer.CloseAndFlush(); e != nil {
		t.Fatal(e)
	}
	if _, e = session.Open(testContext, current); e == nil || provider.opens != 1 {
		t.Fatal("single-use permit replayed", e)
	}
}
func TestCommitUncertaintyAndPolicyRechecksBlockReplay(t *testing.T) {
	for _, which := range []string{"short_commit", "commit_close", "observation_error", "revoked_before_issue"} {
		t.Run(which, func(t *testing.T) {
			session, provider, _ := qualificationFixture(t, false)
			data, e := session.AuthorizeAppend(testContext)
			if e != nil {
				t.Fatal(e)
			}
			writer, e := session.Open(testContext, data)
			if e != nil {
				t.Fatal(e)
			}
			if e = writer.WriteRecord(testContext, make([]byte, tape.RecordBytes)); e != nil {
				t.Fatal(e)
			}
			if e = writer.CloseAndFlush(); e != nil {
				t.Fatal(e)
			}
			if which == "revoked_before_issue" {
				provider.policy.Enrolled = false
				if _, e = session.AuthorizeCommit(testContext); e == nil || provider.opens != 1 || !session.State().Quarantined {
					t.Fatal("revoked commit issued", e)
				}
				return
			}
			permit, e := session.AuthorizeCommit(testContext)
			if e != nil {
				t.Fatal(e)
			}
			if which == "observation_error" {
				provider.observeErr = errors.New("synthetic media disappeared")
				if _, e = session.Open(testContext, permit); e == nil || provider.opens != 1 || !session.State().Quarantined {
					t.Fatal("lost observation allowed commit", e)
				}
				return
			}
			writer, e = session.Open(testContext, permit)
			if e != nil {
				t.Fatal(e)
			}
			if which == "short_commit" {
				provider.short = true
			} else {
				provider.closeErr = errors.New("synthetic commit close failed")
			}
			writeErr := writer.WriteRecord(testContext, make([]byte, tape.RecordBytes))
			closeErr := writer.CloseAndFlush()
			if writeErr == nil && closeErr == nil {
				t.Fatal("commit failure ignored")
			}
			if _, e = session.AuthorizeCommit(testContext); e == nil || !session.State().Quarantined {
				t.Fatal("uncertain commit retried", e)
			}
		})
	}
}

func TestFreshSessionRequiresNewFullChainProofForSecondAppend(t *testing.T) {
	first, provider, now := qualificationFixture(t, false)
	data, e := first.AuthorizeAppend(testContext)
	if e != nil {
		t.Fatal(e)
	}
	writer, e := first.Open(testContext, data)
	if e != nil {
		t.Fatal(e)
	}
	if e = writer.WriteRecord(testContext, make([]byte, tape.RecordBytes)); e != nil {
		t.Fatal(e)
	}
	if e = writer.CloseAndFlush(); e != nil {
		t.Fatal(e)
	}
	commit, e := first.AuthorizeCommit(testContext)
	if e != nil {
		t.Fatal(e)
	}
	writer, e = first.Open(testContext, commit)
	if e != nil {
		t.Fatal(e)
	}
	if e = writer.WriteRecord(testContext, make([]byte, tape.RecordBytes)); e != nil {
		t.Fatal(e)
	}
	if e = writer.CloseAndFlush(); e != nil {
		t.Fatal(e)
	}
	if e = first.Close(); e != nil {
		t.Fatal(e)
	}
	request := provider.request
	request.JobID = "22222222-2222-4222-8222-222222222222"
	request.IntentID = "33333333-3333-4333-8333-333333333333"
	request.LeaseID = "fresh-second-lease"
	provider.request = request
	provider.observation.Lease = request.LeaseID
	provider.observation.Nonce = "fresh-second-observation"
	second, e := newQualificationSession(request, provider, func() time.Time { return *now })
	if e != nil {
		t.Fatal(e)
	}
	defer second.Close()
	// The first session's raw close does not certify a new chain. A separately
	// observed full scan must supply the new count and previous commit digest.
	if _, e = second.AuthorizeAppend(testContext); e == nil || provider.opens != 2 {
		t.Fatal("old scan position permitted another append", e)
	}
	previous := [32]byte{7}
	provider.observation.PreviousCommit = &previous
	provider.pairs = 1
	provider.chain = true
	data, e = second.AuthorizeAppend(testContext)
	if e != nil {
		t.Fatal(e)
	}
	writer, e = second.Open(testContext, data)
	if e != nil {
		t.Fatal(e)
	}
	if e = writer.WriteRecord(testContext, make([]byte, tape.RecordBytes)); e != nil {
		t.Fatal(e)
	}
	if e = writer.CloseAndFlush(); e != nil {
		t.Fatal(e)
	}
	commit, e = second.AuthorizeCommit(testContext)
	if e != nil {
		t.Fatal(e)
	}
	writer, e = second.Open(testContext, commit)
	if e != nil {
		t.Fatal(e)
	}
	if e = writer.WriteRecord(testContext, make([]byte, tape.RecordBytes)); e != nil {
		t.Fatal(e)
	}
	if e = writer.CloseAndFlush(); e != nil {
		t.Fatal(e)
	}
	if !second.State().CommitClosed || provider.opens != 4 || provider.writes != 4 || provider.closes != 4 {
		t.Fatal("two-pair record lifecycle mismatch", second.State())
	}
}
