package progress

import (
	"context"
	"github.com/maxya/TapePilot/internal/tape"
)

type observedWriter struct {
	reporter *Reporter
	writer   tape.RecordWriter
}

func (r *Reporter) Writer(writer tape.RecordWriter) tape.RecordWriter {
	return &observedWriter{r, writer}
}
func (w *observedWriter) WriteRecord(ctx context.Context, b []byte) error {
	if e := w.writer.WriteRecord(ctx, b); e != nil {
		return e
	}
	return w.reporter.Accepted(ctx, int64(len(b)))
}
func (w *observedWriter) CloseAndFlush() error { return w.writer.CloseAndFlush() }

type observedAdapter struct {
	tape.Adapter
	reporter *Reporter
}

func (r *Reporter) Adapter(adapter tape.Adapter) tape.Adapter { return &observedAdapter{adapter, r} }
func (a *observedAdapter) OpenReader(ctx context.Context) (tape.RecordReader, error) {
	reader, e := a.Adapter.OpenReader(ctx)
	if e != nil {
		return nil, e
	}
	return &observedReader{reader, a.reporter}, nil
}

type observedReader struct {
	tape.RecordReader
	reporter *Reporter
}

func (r *observedReader) ReadRecord(ctx context.Context, b []byte) (int, tape.RecordKind, error) {
	n, kind, e := r.RecordReader.ReadRecord(ctx, b)
	if e != nil || kind != tape.Data {
		return n, kind, e
	}
	if e = r.reporter.Read(ctx, int64(n)); e != nil {
		return n, kind, e
	}
	return n, kind, nil
}
