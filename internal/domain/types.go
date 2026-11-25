package domain

type VolumeID string
type ArchiveID string
type JobID string
type State string

const (
	Queued           State = "QUEUED"
	Preflight        State = "PREFLIGHT"
	WaitingMedia     State = "WAITING_MEDIA"
	Running          State = "RUNNING"
	Verifying        State = "VERIFYING"
	Succeeded        State = "SUCCEEDED"
	Failed           State = "FAILED"
	CancelRequested  State = "CANCEL_REQUESTED"
	Cancelled        State = "CANCELLED"
	RecoveryRequired State = "RECOVERY_REQUIRED"
)

type Kind string

const (
	Initialize Kind = "INITIALIZE"
	Backup     Kind = "BACKUP"
	Verify     Kind = "VERIFY"
	Import     Kind = "IMPORT"
	Restore    Kind = "RESTORE"
	Inspect    Kind = "INSPECT"
	Unload     Kind = "UNLOAD"
	Rewind     Kind = "REWIND"
)

type MediaState string

const (
	Unclassified   MediaState = "UNCLASSIFIED"
	BlankCandidate MediaState = "BLANK_CANDIDATE"
	Managed        MediaState = "MANAGED"
	LegacyReadOnly MediaState = "LEGACY_READ_ONLY"
	Sealed         MediaState = "SEALED"
	Quarantined    MediaState = "QUARANTINED"
	Unknown        MediaState = "UNKNOWN"
)

// Code is a stable machine-readable reason for an unsuccessful operation.
type Code string

const (
	MediaMismatch      Code = "MEDIA_MISMATCH"
	PositionUncertain  Code = "POSITION_UNCERTAIN"
	WriteNotArmed      Code = "WRITE_NOT_ARMED"
	VerificationFailed Code = "VERIFICATION_FAILED"
	StateConflict      Code = "STATE_CONFLICT"
)

// ArchiveAssessment keeps independent outcomes distinct. An empty field
// represents a dimension that has not been assessed.
type ArchiveAssessment struct {
	Integrity          Integrity
	Completeness       Completeness
	RestoreEligibility RestoreEligibility
}
type Integrity string

const (
	Committed        Integrity = "COMMITTED"
	Verified         Integrity = "VERIFIED"
	Observed         Integrity = "OBSERVED"
	IntegrityFailed  Integrity = "FAILED"
	IntegrityUnknown Integrity = "UNKNOWN"
)

type Completeness string

const (
	Complete            Completeness = "COMPLETE"
	Incomplete          Completeness = "INCOMPLETE"
	CompletenessUnknown Completeness = "UNKNOWN"
)

type RestoreEligibility string

const (
	Eligible           RestoreEligibility = "ELIGIBLE"
	Ineligible         RestoreEligibility = "INELIGIBLE"
	EligibilityUnknown RestoreEligibility = "UNKNOWN"
)
