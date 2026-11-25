package volume

import (
	"errors"
	"testing"
)

func TestScanCloseFailureInvalidatesStagedEvidence(t *testing.T) {
	files, _ := testFiles(t)
	cause := errors.New("synthetic read descriptor close failure")
	adapter := &fixtureAdapter{files: files, closeErr: cause}
	staged := 0
	result, err := Scan(testContext, adapter, Visitor{Pair: func(Pair) error { staged++; return nil }})
	if staged == 0 || !errors.Is(err, cause) || !errors.Is(err, ErrFinalization) || result.Proof != nil {
		t.Fatal(staged, result, err)
	}
}
