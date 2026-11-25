package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/restore"
	"github.com/maxya/TapePilot/internal/source"
	"github.com/maxya/TapePilot/internal/store"
)

func TestSchedulerPersistsPublicFailureCauseAcrossJoinedErrors(t *testing.T) {
	tests := []struct {
		name  string
		cause error
		code  string
	}{
		{"unsafe archive", restore.ErrUnsafe, "UNSAFE_ARCHIVE"},
		{"changed source", source.ErrChanged, "SOURCE_CHANGED"},
		{"insufficient space", restore.ErrSpace, "INSUFFICIENT_SPACE"},
		{"unsafe existing destination", restore.ErrDestination, "RESTORE_DESTINATION_UNAVAILABLE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := testStore(t)
			job := enqueue(t, s, domain.Inspect, "")
			executor := executorFunc(func(ctx context.Context, j store.Job) error {
				if _, e := s.Transition(ctx, j.ID, j.Version, domain.Running, "read_only_validation", ""); e != nil {
					return e
				}
				return errors.Join(fmt.Errorf("operation under /private/source/name: %w", test.cause), errors.New("cleanup under /private/spool/path"))
			})
			scheduler := New(s, executor)
			if worked, e := scheduler.RunOne(context.Background()); e != nil || !worked {
				t.Fatal(worked, e)
			}
			actual := get(t, s, job.ID)
			if actual.State != domain.Failed || actual.ErrorCode != test.code {
				t.Fatalf("state=%s code=%s", actual.State, actual.ErrorCode)
			}
			raw, e := json.Marshal(actual)
			if e != nil {
				t.Fatal(e)
			}
			if strings.Contains(string(raw), "/private/") {
				t.Fatal("private diagnostic leaked into job response")
			}
		})
	}
}
