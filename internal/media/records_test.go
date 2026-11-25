package media

import (
	"context"
	"errors"
	"github.com/maxya/TapePilot/internal/tape"
	"io"
	"testing"
)

type records []tape.RecordKind

func (r *records) ReadRecord(ctx context.Context, b []byte) (int, tape.RecordKind, error) {
	if len(*r) == 0 {
		return 0, 0, io.ErrUnexpectedEOF
	}
	k := (*r)[0]
	*r = (*r)[1:]
	if k == tape.Data {
		return len(b), k, nil
	}
	return 0, k, nil
}
func (r *records) Close() error { return nil }
func TestFileBoundary(t *testing.T) {
	for _, tc := range []struct {
		steps records
		bytes int
		end   error
	}{{records{tape.EOD}, 0, ErrEOD}, {records{tape.Data, tape.EOD}, tape.RecordBytes, io.ErrUnexpectedEOF}, {records{tape.Data, tape.Filemark, tape.EOD}, tape.RecordBytes, nil}, {records{tape.Filemark}, 0, nil}} {
		data, err := io.ReadAll(NewFileReader(context.Background(), &tc.steps))
		if !errors.Is(err, tc.end) || len(data) != tc.bytes {
			t.Fatal(len(data), err)
		}
	}
}
