// Package jobs serializes durable jobs and owns cancellation through resource close.
package jobs

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/maxya/TapePilot/internal/store"
)

type Executor interface {
	Execute(context.Context, store.Job) error
}

type Scheduler struct {
	store    *store.Store
	executor Executor
	runMu    sync.Mutex
	mu       sync.Mutex
	active   string
	cancel   context.CancelFunc
	wake     chan struct{}
}

func New(s *store.Store, executor Executor) *Scheduler {
	return &Scheduler{store: s, executor: executor, wake: make(chan struct{}, 1)}
}
func (s *Scheduler) Wake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// RunOne claims at most one ready job. Executor errors are persisted as outcomes.
// Returned errors indicate claim or persistence failure and must be surfaced.
func (s *Scheduler) RunOne(ctx context.Context) (bool, error) {
	if !s.runMu.TryLock() {
		return false, store.ErrConflict
	}
	defer s.runMu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	j, err := s.store.Claim(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	jobCtx, cancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.active = j.ID
	s.cancel = cancel
	s.mu.Unlock()
	defer func() {
		cancel()
		s.mu.Lock()
		s.active = ""
		s.cancel = nil
		s.mu.Unlock()
	}()
	requested, err := s.store.CancellationRequested(ctx, j.ID)
	if requested {
		cancel()
	}
	var runErr error
	if err != nil {
		runErr = err
	} else if jobCtx.Err() != nil {
		runErr = jobCtx.Err()
	} else {
		runErr = s.executor.Execute(jobCtx, j)
	}
	// Cancellation does not relinquish the store's active slot until Execute has
	// returned after closing every child, stream and tape lease it owns.
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer finishCancel()
	if err = s.store.FinishExecution(finishCtx, j.ID, runErr); err != nil {
		return true, err
	}
	return true, nil
}
func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		ran, err := s.RunOne(ctx)
		if err != nil {
			if ctx.Err() != nil && !ran {
				return nil
			}
			return err
		}
		if ran {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-s.wake:
		case <-ticker.C:
		}
	}
}
func (s *Scheduler) Cancel(ctx context.Context, id string) error {
	if err := s.store.RequestCancel(ctx, id); err != nil {
		return err
	}
	s.mu.Lock()
	if s.active == id && s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.Wake()
	return nil
}
func (s *Scheduler) Recheck(ctx context.Context, id string) error {
	if err := s.store.RecheckMedia(ctx, id); err != nil {
		return err
	}
	s.Wake()
	return nil
}
