package linux

import (
	"context"
	"errors"
	"io"
	"math"

	"github.com/maxya/TapePilot/internal/tape"
)

type qualifiedWriter struct {
	session *QualificationSession
	fd      qualificationRecordFD
	permit  *QualificationPermit
	// CloseAndFlush has no context parameter. Retain the operation context only
	// to detect cancellation between the last record and the checked close.
	operationContext context.Context
	records          int64
	closed           bool
	failure          error
	closeResult      error
}

func (w *qualifiedWriter) WriteRecord(ctx context.Context, record []byte) error {
	s := w.session
	s.mu.Lock()
	defer s.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	if w.failure != nil || s.state.Quarantined {
		return ErrQuarantined
	}
	if e := errors.Join(ctx.Err(), w.operationContext.Err()); e != nil {
		w.failure = e
		return errors.Join(e, s.quarantine("QUALIFICATION_CANCELLED"))
	}
	if len(record) != tape.RecordBytes {
		w.failure = ErrProfile
		return errors.Join(ErrProfile, s.quarantine("QUALIFICATION_RECORD_SIZE"))
	}
	// Exactly one provider call represents exactly one physical record syscall.
	n, e := w.fd.WriteRecord(record)
	if e != nil || n != len(record) {
		if e == nil {
			e = io.ErrShortWrite
		}
		w.failure = e
		return errors.Join(e, s.quarantine("QUALIFICATION_WRITE_FAILED"))
	}
	if s.state.AcceptedBytes > math.MaxInt64-int64(n) {
		w.failure = ErrLimit
		return errors.Join(ErrLimit, s.quarantine("QUALIFICATION_BYTE_OVERFLOW"))
	}
	s.state.AcceptedBytes += int64(n)
	w.records++
	if e := errors.Join(ctx.Err(), w.operationContext.Err()); e != nil {
		w.failure = e
		return errors.Join(e, s.quarantine("QUALIFICATION_CANCELLED"))
	}
	return nil
}
func (w *qualifiedWriter) CloseAndFlush() error {
	s := w.session
	s.mu.Lock()
	defer s.mu.Unlock()
	if w.closed {
		return w.closeResult
	}
	w.closed = true
	closeErr := w.fd.Close()
	w.closeResult = errors.Join(w.failure, w.operationContext.Err(), closeErr)
	if w.records == 0 {
		w.closeResult = errors.Join(w.closeResult, ErrProfile)
	}
	s.active = nil
	if w.closeResult != nil || s.state.Quarantined {
		w.closeResult = errors.Join(w.closeResult, s.quarantine("QUALIFICATION_CLOSE_FAILED"))
		if w.closeResult == nil {
			w.closeResult = ErrQuarantined
		}
		return w.closeResult
	}
	switch w.permit.kind {
	case writeHeader:
		s.state.HeaderClosed = true
	case writeData:
		s.state.DataClosed = true
		s.pending = w.permit
	case writeCommit:
		s.state.CommitClosed = true
		s.pending = nil
	default:
		w.closeResult = errors.Join(ErrWritePermit, s.quarantine("QUALIFICATION_OPERATION_UNKNOWN"))
	}
	return w.closeResult
}
