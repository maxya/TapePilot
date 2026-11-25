package media

import (
	"archive/tar"
	"errors"
	"io"
	"testing"
)

type failingSource struct{ cause error }

func (s failingSource) Read([]byte) (int, error) { return 0, s.cause }
func TestScanDistinguishesTransportAndFramingFailure(t *testing.T) {
	for _, test := range []struct {
		cause     error
		transport bool
	}{{io.ErrClosedPipe, true}, {tar.ErrHeader, true}, {io.ErrUnexpectedEOF, false}} {
		_, err := Scan(failingSource{test.cause}, nil)
		if !errors.Is(err, test.cause) || errors.Is(err, ErrRead) != test.transport {
			t.Fatalf("source %v: %v", test.cause, err)
		}
	}
}
