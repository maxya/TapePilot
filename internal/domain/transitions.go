package domain

// Edge describes the draft scheduler contract. A listed edge still requires
// the job-specific evidence below before the future store may commit it.
type Edge struct {
	From, To    State
	Requirement string
}

func TransitionContract() []Edge {
	return []Edge{
		{Queued, Preflight, "scheduler claim"},
		{Queued, Cancelled, "cancel before claim"},
		{Preflight, Cancelled, "cancelled before writer-open possibility"},
		{Preflight, WaitingMedia, "required volume unavailable"},
		{WaitingMedia, Preflight, "fresh operator observation"},
		{Preflight, Running, "validated preflight and lease"},
		{Running, Verifying, "managed write closed, readback required"},
		{Running, Succeeded, "completed read-only operation or verified restore publication"},
		{Verifying, Succeeded, "exact readback, identity, format and durable publication"},
		{Preflight, Failed, "failed preflight"},
		{WaitingMedia, Cancelled, "pending job cancelled"},
		{Running, Failed, "known absence of uncertain effects"},
		{Verifying, RecoveryRequired, "uncertain write or integrity"},
		{Running, RecoveryRequired, "uncertain write"},
		{Running, CancelRequested, "cancellation requested"},
		{Verifying, CancelRequested, "cancellation requested"},
		{CancelRequested, Cancelled, "resources closed and effects known safe"},
		{CancelRequested, RecoveryRequired, "possible write or uncertain publication"},
	}
}
