package sim

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"

	"github.com/maxya/TapePilot/internal/tape"
)

type writer struct {
	purpose string
	intent  string
	lease   *Lease
	file    *os.File
	records int64
	failed  bool
	closed  bool
}

func (w *writer) WriteRecord(ctx context.Context, record []byte) error {
	l := w.lease
	l.drive.mu.Lock()
	defer l.drive.mu.Unlock()
	if w.closed || w.failed {
		return ErrUncertain
	}
	if err := l.check(ctx); err != nil {
		w.failed = true
		return err
	}
	if len(record) != tape.RecordBytes {
		w.failed = true
		return errors.New("INVALID_RECORD_SIZE")
	}
	var used int64
	for _, count := range l.data.Files {
		used += count * tape.RecordBytes
	}
	if used > l.data.Capacity-tape.RecordBytes-w.records*tape.RecordBytes {
		w.failed = true
		return ErrEndOfMedia
	}
	next := w.records + 1
	if l.drive.faults.EndOfMediaRecord == next {
		w.failed = true
		return ErrEndOfMedia
	}
	if l.drive.faults.FailWriteRecord == next {
		w.failed = true
		return ErrInjected
	}
	if l.drive.faults.ShortWriteRecord == next {
		_, err := w.file.Write(record[:len(record)/2])
		w.failed = true
		if err != nil {
			return err
		}
		return io.ErrShortWrite
	}
	n, err := w.file.Write(record)
	if err != nil || n != len(record) {
		w.failed = true
		if err != nil {
			return err
		}
		return io.ErrShortWrite
	}
	w.records++
	return nil
}
func (w *writer) CloseAndFlush() error {
	l := w.lease
	l.drive.mu.Lock()
	defer l.drive.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	w.closed = true
	defer func() { l.busy = false }()
	syncErr := w.file.Sync()
	closeErr := w.file.Close()
	if w.failed || syncErr != nil || closeErr != nil || l.drive.faults.FailClose || (l.drive.faults.FailCloseAtIndex != nil && *l.drive.faults.FailCloseAtIndex == l.file) || w.records == 0 {
		return errors.Join(ErrUncertain, syncErr, closeErr)
	}
	// Persist a completed physical filemark only after the file is durable and
	// close succeeded. Failure of this metadata commit leaves the intent pending.
	updated := l.data
	updated.Files = append(append([]int64{}, l.data.Files...), w.records)
	updated.Pending = nil
	updated.Uncertain = false
	if err := save(l.dir, updated); err != nil {
		return err
	}
	l.data = updated
	l.file++
	if w.purpose == "data" {
		l.commitReady = l.file
		l.commitIntent = w.intent
	} else {
		l.commitReady = 0
		l.commitIntent = ""
	}
	return nil
}

type reader struct {
	fileRecords int64
	lease       *Lease
	file        *os.File
	records     int64
	closed      bool
	failed      bool
}

func (r *reader) ReadRecord(ctx context.Context, dst []byte) (int, tape.RecordKind, error) {
	l := r.lease
	l.drive.mu.Lock()
	defer l.drive.mu.Unlock()
	if r.closed {
		return 0, 0, ErrClosed
	}
	if r.failed {
		return 0, 0, ErrUncertain
	}
	if err := l.check(ctx); err != nil {
		return 0, 0, err
	}
	if len(dst) < tape.RecordBytes {
		return 0, 0, errors.New("RECORD_BUFFER_TOO_SMALL")
	}
	count := uint64(len(l.data.Files))
	if l.file > count {
		return 0, 0, ErrPosition
	}
	pending := l.file == count && l.data.Pending != nil
	if l.file == count && !pending {
		return 0, tape.EOD, nil
	}
	if r.file == nil {
		f, err := os.OpenFile(filePath(l.dir, l.file), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			r.failed = true
			return 0, 0, err
		}
		r.file = f
	}
	next := r.records + 1
	if l.drive.faults.FailReadRecord == next {
		r.failed = true
		return 0, 0, ErrInjected
	}
	n, err := r.file.Read(dst[:tape.RecordBytes])
	if n == 0 && err == io.EOF {
		closeErr := r.file.Close()
		r.file = nil
		if closeErr != nil {
			r.failed = true
			return 0, 0, closeErr
		}
		if pending || r.fileRecords != l.data.Files[l.file] {
			r.failed = true
			return 0, 0, ErrUncertain
		}
		l.file++
		r.fileRecords = 0
		return 0, tape.Filemark, nil
	}
	if err != nil || n != tape.RecordBytes {
		r.failed = true
		if err != nil {
			return n, 0, err
		}
		return n, 0, io.ErrUnexpectedEOF
	}
	r.records++
	r.fileRecords++
	if !pending && r.fileRecords > l.data.Files[l.file] {
		r.failed = true
		return n, 0, ErrUncertain
	}
	if l.drive.faults.CorruptReadRecord == next {
		dst[0] ^= 1
	}
	return n, tape.Data, nil
}
func (r *reader) Close() error {
	l := r.lease
	l.drive.mu.Lock()
	defer l.drive.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	r.closed = true
	l.busy = false
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}
