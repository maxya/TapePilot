package api

import (
	"fmt"
	"github.com/maxya/TapePilot/internal/restore"
	"github.com/maxya/TapePilot/internal/source"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSafeOperationalGuidance(t *testing.T) {
	for _, c := range []struct {
		err          error
		code, action string
	}{
		{restore.ErrUnsafe, "UNSAFE_ARCHIVE", "Inspect"}, {restore.ErrSpace, "INSUFFICIENT_SPACE", "Free space"}, {restore.ErrDestination, "RESTORE_DESTINATION_UNAVAILABLE", "fresh destination"}, {source.ErrChanged, "SOURCE_CHANGED", "new plan"}} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/", nil)
		fail(w, r, fmt.Errorf("private fixture filename: %w", c.err))
		out := w.Body.String()
		if !strings.Contains(out, c.code) || !strings.Contains(out, c.action) || strings.Contains(out, "private fixture") {
			t.Fatal(out)
		}
	}
}
