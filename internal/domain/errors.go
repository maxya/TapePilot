package domain

import "errors"

const (
	UnsafeArchive                 Code = "UNSAFE_ARCHIVE"
	SourceChanged                 Code = "SOURCE_CHANGED"
	InsufficientSpace             Code = "INSUFFICIENT_SPACE"
	RestoreDestinationUnavailable Code = "RESTORE_DESTINATION_UNAVAILABLE"
)

// Shared sentinels let low-level packages expose a typed cause while the store
// persists a fixed public code without importing its own execution callers.
var (
	ErrUnsafeArchive                 = errors.New("UNSAFE_ARCHIVE")
	ErrSourceChanged                 = errors.New("SOURCE_CHANGED")
	ErrInsufficientSpace             = errors.New("INSUFFICIENT_RESTORE_SPACE")
	ErrRestoreDestinationUnavailable = errors.New("DESTINATION_EXISTS_OR_UNSAFE")
)

// ExecutionFailureCode follows ordinary wrapping and joined errors. Arbitrary
// error strings, source paths and subprocess diagnostics never become codes.
func ExecutionFailureCode(err error) Code {
	switch {
	case errors.Is(err, ErrUnsafeArchive):
		return UnsafeArchive
	case errors.Is(err, ErrSourceChanged):
		return SourceChanged
	case errors.Is(err, ErrInsufficientSpace):
		return InsufficientSpace
	case errors.Is(err, ErrRestoreDestinationUnavailable):
		return RestoreDestinationUnavailable
	default:
		return ""
	}
}
