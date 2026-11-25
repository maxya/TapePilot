package stream

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"github.com/maxya/TapePilot/internal/domain"
	"github.com/maxya/TapePilot/internal/source"
	"github.com/maxya/TapePilot/internal/tape"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

type sink struct {
	data  bytes.Buffer
	calls int
	fail  bool
}

func (s *sink) WriteRecord(ctx context.Context, b []byte) error {
	s.calls++
	if s.fail {
		return errors.New("injected EOM")
	}
	if len(b) != tape.RecordBytes {
		return ErrRecord
	}
	_, e := s.data.Write(b)
	return e
}
func (s *sink) CloseAndFlush() error { return nil }
func TestGNUtarLiteralNames(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"-leading", "line\nname", "café"} {
		if e := os.WriteFile(filepath.Join(root, name), []byte("payload"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	p, e := source.Create(context.Background(), []source.Root{{ID: "s", Path: root}}, "s", "", t.TempDir(), nil, source.DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	var out sink
	result, e := Produce(context.Background(), p, &out)
	if e != nil {
		t.Fatal(e)
	}
	if result.Bytes != int64(out.data.Len()) || result.Bytes%tape.RecordBytes != 0 || result.SHA256 != sha256.Sum256(out.data.Bytes()) {
		t.Fatal("exact stream digest mismatch")
	}
	reader := tar.NewReader(bytes.NewReader(out.data.Bytes()))
	var names []string
	for {
		h, e := reader.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		names = append(names, h.Name)
		data, e := io.ReadAll(reader)
		if e != nil || string(data) != "payload" {
			t.Fatal("invalid payload")
		}
	}
	if len(names) != 3 || names[0] != "-leading" || names[2] != "line\nname" {
		t.Fatalf("names %q", names)
	}
	out = sink{fail: true}
	if _, e = Produce(context.Background(), p, &out); e == nil || out.calls != 1 {
		t.Fatal("downstream failure ignored or retried")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = Produce(ctx, p, &sink{}); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
func TestPartialRecordAndShortChunks(t *testing.T) {
	var out sink
	if _, e := Pump(context.Background(), bytes.NewReader(make([]byte, tape.RecordBytes-1)), &out); !errors.Is(e, ErrRecord) || out.calls != 0 {
		t.Fatal("partial record accepted")
	}
	data := make([]byte, 2*tape.RecordBytes)
	for i := range data {
		data[i] = byte(i)
	}
	result, e := Pump(context.Background(), smallReader{bytes.NewReader(data)}, &out)
	if e != nil || result.SHA256 != sha256.Sum256(data) || out.calls != 2 {
		t.Fatal(e)
	}
}

type smallReader struct{ io.Reader }

func (r smallReader) Read(b []byte) (int, error) {
	if len(b) > 19 {
		b = b[:19]
	}
	return r.Reader.Read(b)
}

func TestMbufferExactBytes(t *testing.T) {
	binary := os.Getenv("TAPEPILOT_TEST_MBUFFER")
	if binary == "" {
		binary = "/usr/bin/mbuffer"
	}
	if _, e := os.Stat(binary); e != nil {
		t.Skip("mbuffer is unavailable")
	}
	raw := make([]byte, 3*tape.RecordBytes)
	for i := range raw {
		raw[i] = byte(i * 17)
	}
	cmd := exec.Command(binary, "-q", "-s", "256k", "-m", "128M")
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	cmd.Stdin = bytes.NewReader(raw)
	actual, e := cmd.Output()
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(raw, actual) {
		t.Fatal("mbuffer altered complete-record bytes")
	}
	root := t.TempDir()
	if e = os.WriteFile(filepath.Join(root, "sample"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	p, e := source.Create(context.Background(), []source.Root{{ID: "s", Path: root}}, "s", "", t.TempDir(), nil, source.DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	var out sink
	if _, e = produce(context.Background(), p, &out, binary); e != nil {
		t.Fatal(e)
	}
	reader := tar.NewReader(bytes.NewReader(out.data.Bytes()))
	if _, e = reader.Next(); e != nil {
		t.Fatal(e)
	}
	data, e := io.ReadAll(reader)
	if e != nil || !bytes.Equal(data, raw) {
		t.Fatal("buffered payload mismatch")
	}
	out = sink{fail: true}
	if _, e = produce(context.Background(), p, &out, binary); e == nil || out.calls != 1 {
		t.Fatal("downstream failure ignored")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := cancelSink{cancel}
	if _, e = produce(ctx, p, writer, binary); e == nil {
		t.Fatal("cancellation ignored")
	}
	// A fixed test executable that rejects mbuffer's arguments must be reaped,
	// and its nonzero status must prevent a successful result.
	if _, e = produce(context.Background(), p, &sink{}, "/usr/bin/false"); !errors.Is(e, ErrProducer) {
		t.Fatal("child failure ignored", e)
	}
}

type cancelSink struct{ cancel context.CancelFunc }

func (s cancelSink) WriteRecord(ctx context.Context, b []byte) error { s.cancel(); return ctx.Err() }
func (s cancelSink) CloseAndFlush() error                            { return nil }

func init() {
	if filepath.Base(os.Args[0]) == "tapepilot-test-stderr-flood" {
		if e := os.WriteFile(os.Args[0]+".pid", []byte(strconv.Itoa(os.Getpid())), 0600); e != nil {
			os.Exit(10)
		}
		chunk := bytes.Repeat([]byte("x"), 32768)
		for i := 0; i < 256; i++ {
			if _, e := os.Stderr.Write(chunk); e != nil {
				os.Exit(11)
			}
		}
		os.Exit(9)
	}
	if filepath.Base(os.Args[0]) == "tapepilot-test-stalled-buffer" {
		time.Sleep(time.Hour)
		os.Exit(0)
	}
}

func TestStderrFloodBoundedFailureAndReaping(t *testing.T) {
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	helper := filepath.Join(t.TempDir(), "tapepilot-test-stderr-flood")
	if e = os.Symlink(executable, helper); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, helper)
	configure(cmd)
	var logs boundedLog
	cmd.Stderr = &logs
	e = cmd.Run()
	var exit *exec.ExitError
	if !errors.As(e, &exit) || exit.ExitCode() != 9 {
		t.Fatalf("unchecked child failure: %v", e)
	}
	if ctx.Err() != nil {
		t.Fatal("stderr flood blocked until timeout")
	}
	if len(logs.data) != 16384 || !logs.nonempty() {
		t.Fatalf("retained stderr length %d", len(logs.data))
	}
	if e = syscall.Kill(cmd.Process.Pid, 0); !errors.Is(e, syscall.ESRCH) {
		t.Fatalf("child still present after wait: %v", e)
	}
	root := t.TempDir()
	if e = os.WriteFile(filepath.Join(root, "file"), []byte("payload"), 0600); e != nil {
		t.Fatal(e)
	}
	p, e := source.Create(ctx, []source.Root{{ID: "s", Path: root}}, "s", "", t.TempDir(), nil, source.DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	if _, e = produce(ctx, p, &sink{}, helper); !errors.Is(e, ErrProducer) {
		t.Fatalf("pipeline accepted stderr flood: %v", e)
	}
	if ctx.Err() != nil {
		t.Fatal("pipeline stderr flood blocked until timeout")
	}
	raw, e := os.ReadFile(helper + ".pid")
	if e != nil {
		t.Fatal(e)
	}
	pid, e := strconv.Atoi(string(raw))
	if e != nil {
		t.Fatal(e)
	}
	if e = syscall.Kill(pid, 0); !errors.Is(e, syscall.ESRCH) {
		t.Fatalf("pipeline child still present after wait: %v", e)
	}
}
func TestStalledBufferCancellation(t *testing.T) {
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	stalled := filepath.Join(t.TempDir(), "tapepilot-test-stalled-buffer")
	if e = os.Symlink(executable, stalled); e != nil {
		t.Fatal(e)
	}
	root := t.TempDir()
	if e = os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0600); e != nil {
		t.Fatal(e)
	}
	p, e := source.Create(context.Background(), []source.Root{{ID: "s", Path: root}}, "s", "", t.TempDir(), nil, source.DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, e = produce(ctx, p, &sink{}, stalled); e == nil {
		t.Fatal("stalled child accepted")
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("stalled child cancellation delayed")
	}
}

type mutateSink struct {
	sink
	path string
	done bool
}

func (s *mutateSink) WriteRecord(ctx context.Context, b []byte) error {
	if !s.done {
		s.done = true
		if e := os.Truncate(s.path, 0); e != nil {
			return e
		}
	}
	return s.sink.WriteRecord(ctx, b)
}
func TestTarNonzeroAfterSourceMutation(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "changing")
	if e := os.WriteFile(file, make([]byte, 4*1024*1024), 0600); e != nil {
		t.Fatal(e)
	}
	p, e := source.Create(context.Background(), []source.Root{{ID: "s", Path: root}}, "s", "", t.TempDir(), nil, source.DefaultLimits())
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	out := &mutateSink{path: file}
	if _, e = produce(context.Background(), p, out, ""); !errors.Is(e, ErrProducer) {
		t.Fatal("tar producer failure lost", e)
	}
	if domain.ExecutionFailureCode(e) != domain.SourceChanged {
		t.Fatal("producer mutation cause lost", e)
	}
}
