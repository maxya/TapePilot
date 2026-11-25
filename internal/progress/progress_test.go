package progress

import (
	"context"
	"errors"
	"github.com/maxya/TapePilot/internal/tape"
	"math"
	"sync"
	"testing"
	"time"
)

const id = "2f2d805c-e530-4f68-ab29-5cb4c8f63319"

type capture struct {
	samples []Snapshot
	err     error
}

func (c *capture) SaveProgress(ctx context.Context, s Snapshot) error {
	if e := s.Validate(); e != nil {
		return e
	}
	c.samples = append(c.samples, s)
	return c.err
}
func fixed(t *testing.T, c Sink) (*Reporter, *time.Time) {
	t.Helper()
	r, e := New(id, c)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	r.sample.StartedAt = now
	r.sample.SampledAt = now
	r.phaseStarted = now
	r.lastRateTime = now
	return r, &now
}
func TestProgressThrottleAndIndependentCounters(t *testing.T) {
	c := &capture{}
	r, now := fixed(t, c)
	ctx := context.Background()
	total := int64(1024 * 1024)
	if e := r.Phase(ctx, Streaming, &total); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 1000; i++ {
		if e := r.Accepted(ctx, 512); e != nil {
			t.Fatal(e)
		}
	}
	if len(c.samples) != 1 {
		t.Fatal("unthrottled persistence", len(c.samples))
	}
	*now = now.Add(Interval)
	if e := r.Accepted(ctx, 512); e != nil {
		t.Fatal(e)
	}
	s := r.Snapshot()
	if len(c.samples) != 2 || s.AcceptedBytes != 1001*512 || s.ReadbackBytes != 0 || s.SpeedMiBPerSecond <= 0 || s.EstimatedPercent == nil || s.ETASeconds == nil {
		t.Fatalf("%#v", s)
	}
	if e := r.Phase(ctx, Readback, &total); e != nil {
		t.Fatal(e)
	}
	if e := r.Read(ctx, 262144); e != nil {
		t.Fatal(e)
	}
	s = r.Snapshot()
	if s.AcceptedBytes != 1001*512 || s.ReadbackBytes != 262144 || s.EstimatedPercent == nil || *s.EstimatedPercent != 25 {
		t.Fatalf("%#v", s)
	}
	*now = now.Add(Interval)
	if e := r.Phase(ctx, Indexing, nil); e != nil {
		t.Fatal(e)
	}
	s = r.Snapshot()
	if s.EstimatedPercent != nil || s.ETASeconds != nil || s.SpeedMiBPerSecond != 0 {
		t.Fatal("non-transfer estimate presented")
	}
}
func TestBoundedSubscriptionsAndIsolation(t *testing.T) {
	r, now := fixed(t, nil)
	ctx := context.Background()
	ch, unsubscribe := r.Subscribe()
	defer unsubscribe()
	<-ch
	for i := 0; i < 100; i++ {
		if e := r.Accepted(ctx, 1); e != nil {
			t.Fatal(e)
		}
	}
	if len(ch) > 1 {
		t.Fatal("unbounded samples")
	}
	*now = now.Add(Interval)
	if e := r.Accepted(ctx, 1); e != nil {
		t.Fatal(e)
	}
	if sample := <-ch; sample.AcceptedBytes != 101 {
		t.Fatal("stale pending snapshot")
	}
	var unsubscribeAll []func()
	for i := 0; i < 63; i++ {
		_, off := r.Subscribe()
		unsubscribeAll = append(unsubscribeAll, off)
	}
	rejected, _ := r.Subscribe()
	if _, ok := <-rejected; ok {
		t.Fatal("subscriber cap bypassed")
	}
	for _, off := range unsubscribeAll {
		off()
	}
	total := int64(100)
	r.Phase(ctx, Streaming, &total)
	total = 1
	s := r.Snapshot()
	*s.PhaseTotalBytes = 7
	if *r.Snapshot().PhaseTotalBytes != 100 {
		t.Fatal("snapshot aliases reporter memory")
	}
}
func TestConcurrentCountersAndCancellation(t *testing.T) {
	r, e := New(id, nil)
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if e := r.Accepted(context.Background(), 1); e != nil {
					t.Error(e)
				}
			}
		}()
	}
	wg.Wait()
	if r.Snapshot().AcceptedBytes != 800 {
		t.Fatal("lost increments")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(r.Flush(ctx), context.Canceled) {
		t.Fatal("flush ignored cancellation")
	}
	if e = r.Accepted(context.Background(), -1); !errors.Is(e, ErrInvalid) {
		t.Fatal("negative counter accepted")
	}
	r.mu.Lock()
	r.sample.AcceptedBytes = math.MaxInt64
	r.mu.Unlock()
	if e = r.Accepted(context.Background(), 1); !errors.Is(e, ErrInvalid) {
		t.Fatal("overflow accepted")
	}
}

type failWriter struct {
	fail   bool
	closed bool
}

func (w *failWriter) WriteRecord(context.Context, []byte) error {
	if w.fail {
		return errors.New("write failed")
	}
	return nil
}
func (w *failWriter) CloseAndFlush() error { w.closed = true; return errors.New("close failed") }
func TestWriterCountsOnlyAcceptedRecords(t *testing.T) {
	r, e := New(id, nil)
	if e != nil {
		t.Fatal(e)
	}
	base := &failWriter{fail: true}
	w := r.Writer(base)
	if w.WriteRecord(context.Background(), make([]byte, tape.RecordBytes)) == nil || r.Snapshot().AcceptedBytes != 0 {
		t.Fatal("failed write counted")
	}
	base.fail = false
	if e = w.WriteRecord(context.Background(), make([]byte, tape.RecordBytes)); e != nil {
		t.Fatal(e)
	}
	if r.Snapshot().AcceptedBytes != tape.RecordBytes || r.Snapshot().ReadbackBytes != 0 {
		t.Fatal("write confused with readback")
	}
	if w.CloseAndFlush() == nil || !base.closed {
		t.Fatal("close failure ignored")
	}
}
func TestInvalidSnapshot(t *testing.T) {
	r, e := New(id, nil)
	if e != nil {
		t.Fatal(e)
	}
	valid := r.Snapshot()
	for _, mutate := range []func(*Snapshot){func(s *Snapshot) { s.Phase = "/secret/path" }, func(s *Snapshot) { s.JobID = "file names" }, func(s *Snapshot) { s.SpeedMiBPerSecond = math.NaN() }, func(s *Snapshot) { s.ReadbackBytes = -1 }, func(s *Snapshot) { x := 101.0; s.EstimatedPercent = &x }} {
		s := valid
		mutate(&s)
		if s.Validate() == nil {
			t.Fatal("invalid sample accepted")
		}
	}
}

func TestTelemetryFromSnapshot(t *testing.T) {
	r, e := New(id, nil)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Accepted(context.Background(), 262144); e != nil {
		t.Fatal(e)
	}
	previous := r.Snapshot()
	next, e := FromSnapshot(previous, nil)
	if e != nil {
		t.Fatal(e)
	}
	snapshot := next.Snapshot()
	if snapshot.AcceptedBytes != previous.AcceptedBytes || snapshot.StartedAt != previous.StartedAt || snapshot.JobID != previous.JobID {
		t.Fatal("telemetry continuity lost")
	}
}

type fixedReader struct {
	n    int
	kind tape.RecordKind
	err  error
}

func (r fixedReader) ReadRecord(context.Context, []byte) (int, tape.RecordKind, error) {
	return r.n, r.kind, r.err
}
func (r fixedReader) Close() error { return errors.New("close failed") }
func TestReadbackCountsOnlySuccessfulData(t *testing.T) {
	r, e := New(id, nil)
	if e != nil {
		t.Fatal(e)
	}
	for _, base := range []fixedReader{{tape.RecordBytes, tape.Data, nil}, {0, tape.Filemark, nil}, {17, tape.Data, errors.New("short record")}} {
		reader := observedReader{base, r}
		reader.ReadRecord(context.Background(), make([]byte, tape.RecordBytes))
		if reader.Close() == nil {
			t.Fatal("reader close failure lost")
		}
	}
	sample := r.Snapshot()
	if sample.AcceptedBytes != 0 || sample.ReadbackBytes != tape.RecordBytes {
		t.Fatal("readback counters incorrect")
	}
}
