// Package progress reports transfer observations without asserting completion.
package progress

import (
	"context"
	"errors"
	"math"
	"regexp"
	"sync"
	"time"
)

const Interval = 500 * time.Millisecond

var ErrInvalid = errors.New("INVALID_PROGRESS")
var jobID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

type Phase string

const (
	Planning      Phase = "planning"
	IdentityCheck Phase = "identity_check"
	Positioning   Phase = "positioning"
	Streaming     Phase = "streaming"
	ClosingData   Phase = "closing_data"
	WritingCommit Phase = "writing_commit"
	Readback      Phase = "readback"
	Indexing      Phase = "indexing"
	Spooling      Phase = "spooling"
	Extracting    Phase = "extracting"
	Publishing    Phase = "publishing"
)

func validPhase(p Phase) bool {
	switch p {
	case Planning, IdentityCheck, Positioning, Streaming, ClosingData, WritingCommit, Readback, Indexing, Spooling, Extracting, Publishing:
		return true
	}
	return false
}

// Snapshot contains bounded numeric observations and a fixed phase vocabulary.
// No source names, filesystem paths, credentials or archive membership is kept.
type Snapshot struct {
	JobID             string    `json:"job_id"`
	Phase             Phase     `json:"phase"`
	AcceptedBytes     int64     `json:"accepted_bytes"`
	ReadbackBytes     int64     `json:"readback_bytes"`
	PhaseTotalBytes   *int64    `json:"phase_total_bytes"`
	SampledAt         time.Time `json:"sampled_at"`
	StartedAt         time.Time `json:"started_at"`
	ElapsedSeconds    float64   `json:"elapsed_seconds"`
	SpeedMiBPerSecond float64   `json:"speed_mib_per_second"`
	EstimatedPercent  *float64  `json:"estimated_percent"`
	ETASeconds        *float64  `json:"eta_seconds"`
}

func (s Snapshot) Validate() error {
	if !jobID.MatchString(s.JobID) || !validPhase(s.Phase) || s.AcceptedBytes < 0 || s.ReadbackBytes < 0 || s.SampledAt.IsZero() || s.StartedAt.IsZero() || s.SampledAt.Before(s.StartedAt) || s.SampledAt.Location() != time.UTC || s.StartedAt.Location() != time.UTC {
		return ErrInvalid
	}
	for _, x := range []float64{s.ElapsedSeconds, s.SpeedMiBPerSecond} {
		if math.IsNaN(x) || math.IsInf(x, 0) || x < 0 {
			return ErrInvalid
		}
	}
	if s.PhaseTotalBytes != nil && *s.PhaseTotalBytes <= 0 {
		return ErrInvalid
	}
	if s.EstimatedPercent != nil && (math.IsNaN(*s.EstimatedPercent) || math.IsInf(*s.EstimatedPercent, 0) || *s.EstimatedPercent < 0 || *s.EstimatedPercent > 100) {
		return ErrInvalid
	}
	if s.ETASeconds != nil && (math.IsNaN(*s.ETASeconds) || math.IsInf(*s.ETASeconds, 0) || *s.ETASeconds < 0) {
		return ErrInvalid
	}
	return nil
}

type Sink interface {
	SaveProgress(context.Context, Snapshot) error
}
type Reporter struct {
	mu                       sync.Mutex
	sample                   Snapshot
	sink                     Sink
	now                      func() time.Time
	lastPersist              time.Time
	lastBroadcast            time.Time
	phaseStarted             time.Time
	phaseAccepted, phaseRead int64
	lastRateTime             time.Time
	lastRateBytes            int64
	rate                     float64
	subscribers              map[chan Snapshot]struct{}
}

func New(id string, sink Sink) (*Reporter, error) {
	now := time.Now().UTC()
	r := &Reporter{sample: Snapshot{JobID: id, Phase: Planning, SampledAt: now, StartedAt: now}, sink: sink, now: time.Now, phaseStarted: now, lastRateTime: now, subscribers: make(map[chan Snapshot]struct{})}
	if e := r.sample.Validate(); e != nil {
		return nil, e
	}
	return r, nil
}

// FromSnapshot preserves telemetry across waiting-media rechecks. It carries no
// job execution authority, media position, or instruction to resume any write.
func FromSnapshot(sample Snapshot, sink Sink) (*Reporter, error) {
	if e := sample.Validate(); e != nil {
		return nil, e
	}
	r, e := New(sample.JobID, sink)
	if e != nil {
		return nil, e
	}
	r.sample = clone(sample)
	r.phaseAccepted = sample.AcceptedBytes
	r.phaseRead = sample.ReadbackBytes
	r.sample.SpeedMiBPerSecond = 0
	r.sample.EstimatedPercent = nil
	r.sample.ETASeconds = nil
	r.lastPersist = sample.SampledAt
	r.lastBroadcast = sample.SampledAt
	return r, nil
}
func clone(s Snapshot) Snapshot {
	if s.PhaseTotalBytes != nil {
		x := *s.PhaseTotalBytes
		s.PhaseTotalBytes = &x
	}
	if s.EstimatedPercent != nil {
		x := *s.EstimatedPercent
		s.EstimatedPercent = &x
	}
	if s.ETASeconds != nil {
		x := *s.ETASeconds
		s.ETASeconds = &x
	}
	return s
}
func (r *Reporter) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refresh()
	return clone(r.sample)
}
func (r *Reporter) Phase(ctx context.Context, p Phase, total *int64) error {
	if !validPhase(p) || total != nil && *total <= 0 {
		return ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sample.Phase != p {
		r.sample.Phase = p
		r.phaseStarted = r.now().UTC()
		r.phaseAccepted = r.sample.AcceptedBytes
		r.phaseRead = r.sample.ReadbackBytes
		r.rate = 0
		r.lastRateTime = r.phaseStarted
		r.lastRateBytes = 0
	}
	r.sample.PhaseTotalBytes = nil
	if total != nil {
		x := *total
		r.sample.PhaseTotalBytes = &x
	}
	return r.checkpoint(ctx, false)
}
func (r *Reporter) Accepted(ctx context.Context, n int64) error { return r.add(ctx, n, false) }
func (r *Reporter) Read(ctx context.Context, n int64) error     { return r.add(ctx, n, true) }
func (r *Reporter) add(ctx context.Context, n int64, read bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if n < 0 {
		return ErrInvalid
	}
	if read {
		if r.sample.ReadbackBytes > math.MaxInt64-n {
			return ErrInvalid
		}
		r.sample.ReadbackBytes += n
	} else {
		if r.sample.AcceptedBytes > math.MaxInt64-n {
			return ErrInvalid
		}
		r.sample.AcceptedBytes += n
	}
	return r.checkpoint(ctx, false)
}
func (r *Reporter) refresh() {
	now := r.now().UTC()
	if now.Before(r.sample.SampledAt) {
		now = r.sample.SampledAt
	}
	r.sample.SampledAt = now
	r.sample.ElapsedSeconds = now.Sub(r.sample.StartedAt).Seconds()
	bytes := r.sample.AcceptedBytes - r.phaseAccepted
	transfer := r.sample.Phase == Streaming
	if r.sample.Phase == Readback || r.sample.Phase == Spooling {
		bytes = r.sample.ReadbackBytes - r.phaseRead
		transfer = true
	}
	elapsed := now.Sub(r.lastRateTime).Seconds()
	if elapsed >= Interval.Seconds() {
		instant := float64(bytes-r.lastRateBytes) / elapsed
		if r.rate == 0 {
			r.rate = instant
		} else {
			r.rate = 0.25*instant + 0.75*r.rate
		}
		r.lastRateTime = now
		r.lastRateBytes = bytes
	}
	r.sample.SpeedMiBPerSecond = 0
	r.sample.EstimatedPercent = nil
	r.sample.ETASeconds = nil
	if !transfer {
		return
	}
	r.sample.SpeedMiBPerSecond = r.rate / (1024 * 1024)
	if r.sample.PhaseTotalBytes == nil || bytes > *r.sample.PhaseTotalBytes {
		return
	}
	percent := 100 * float64(bytes) / float64(*r.sample.PhaseTotalBytes)
	r.sample.EstimatedPercent = &percent
	if r.rate > 0 {
		eta := float64(*r.sample.PhaseTotalBytes-bytes) / r.rate
		r.sample.ETASeconds = &eta
	}
}
func (r *Reporter) checkpoint(ctx context.Context, force bool) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	r.refresh()
	if r.lastBroadcast.IsZero() || r.sample.SampledAt.Sub(r.lastBroadcast) >= Interval {
		for ch := range r.subscribers {
			select {
			case ch <- clone(r.sample):
			default:
				select {
				case <-ch:
				default:
				}
				ch <- clone(r.sample)
			}
		}
		r.lastBroadcast = r.sample.SampledAt
	}
	if !force && !r.lastPersist.IsZero() && r.sample.SampledAt.Sub(r.lastPersist) < Interval {
		return nil
	}
	if r.sink != nil {
		if e := r.sink.SaveProgress(ctx, clone(r.sample)); e != nil {
			return e
		}
	}
	r.lastPersist = r.sample.SampledAt
	return nil
}

// Flush persists the final observation after the throttle interval. Cancellation
// interrupts the bounded wait. This method never sets a job's terminal state.
func (r *Reporter) Flush(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.lastPersist.IsZero() {
		wait := Interval - r.now().UTC().Sub(r.lastPersist)
		if wait > Interval {
			wait = Interval
		}
		if wait > 0 {
			timer := time.NewTimer(wait)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return r.checkpoint(ctx, true)
}

// Subscribe retains at most one pending sample per subscriber. Consumers obtain
// the current snapshot immediately and must call unsubscribe when finished.
// At most 64 subscribers are retained. Additional subscriptions are closed.
func (r *Reporter) Subscribe() (<-chan Snapshot, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ch := make(chan Snapshot, 1)
	if len(r.subscribers) >= 64 {
		close(ch)
		return ch, func() {}
	}
	r.refresh()
	ch <- clone(r.sample)
	r.subscribers[ch] = struct{}{}
	return ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if _, ok := r.subscribers[ch]; ok {
			delete(r.subscribers, ch)
			close(ch)
		}
	}
}
