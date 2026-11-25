package app

import (
	"context"
	"errors"

	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/store"
)

// Handlers are registered at startup by the composition root. The queue never
// dispatches arbitrary user-supplied executables or handlers.
type Handlers map[domain.Kind]func(context.Context, store.Job) error

func (h Handlers) Execute(ctx context.Context, j store.Job) error {
	f := h[j.Kind]
	if f == nil {
		return errors.New("UNSUPPORTED_JOB_KIND")
	}
	return f(ctx, j)
}
