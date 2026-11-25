package media

import (
	"context"
	"errors"
	"io"

	"github.com/maxya/TapePilot/internal/tape"
)

// ErrEOD signifies a clean end before the next logical file begins.
var ErrEOD = errors.New("end of recorded data")

// FileReader exposes one logical tape file to the TAR scanner. The caller owns
// the underlying reader and must inspect its Close error.
type FileReader struct {
	ctx     context.Context
	records tape.RecordReader
	buffer  []byte
	unread  []byte
	started bool
	end     error
}

func NewFileReader(ctx context.Context, r tape.RecordReader) *FileReader {
	return &FileReader{ctx: ctx, records: r, buffer: make([]byte, tape.RecordBytes)}
}
func (f *FileReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if f.end != nil {
		return 0, f.end
	}
	if len(f.unread) == 0 {
		n, kind, err := f.records.ReadRecord(f.ctx, f.buffer)
		if err != nil {
			f.end = err
			return 0, err
		}
		switch {
		case kind == tape.Data && n == len(f.buffer):
			f.unread = f.buffer
			f.started = true
		case kind == tape.Filemark && n == 0:
			f.end = io.EOF
		case kind == tape.EOD && n == 0 && !f.started:
			f.end = ErrEOD
		default:
			f.end = io.ErrUnexpectedEOF
		}
		if f.end != nil {
			return 0, f.end
		}
	}
	n := copy(p, f.unread)
	f.unread = f.unread[n:]
	return n, nil
}
