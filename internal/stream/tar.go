// Package stream supervises fixed GNU tar production into exact tape records.
package stream

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"github.com/maxya/TapePilot/internal/source"
	"github.com/maxya/TapePilot/internal/tape"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

var ErrRecord = errors.New("PARTIAL_RECORD")
var ErrProducer = errors.New("TAR_FAILED")

type Result struct {
	Bytes  int64
	SHA256 [32]byte
}

// Produce transfers the stream once. It neither closes the writer nor interprets
// successful transfer as readback. The engine must check close and verify later.
func Produce(ctx context.Context, plan *source.Plan, writer tape.RecordWriter) (Result, error) {
	buffer := ""
	if info, err := os.Stat("/usr/bin/mbuffer"); err == nil && info.Mode().IsRegular() {
		buffer = "/usr/bin/mbuffer"
	}
	return produce(ctx, plan, writer, buffer)
}
func produce(ctx context.Context, plan *source.Plan, writer tape.RecordWriter, buffer string) (Result, error) {
	root, list, e := plan.OpenFiles(ctx)
	if e != nil {
		return Result{}, e
	}
	defer root.Close()
	defer list.Close()
	cmd := exec.CommandContext(ctx, "/usr/bin/tar", "--create", "--format=pax", "--blocking-factor=512", "--file=-", "--directory=/proc/self/fd/3", "--no-recursion", "--null", "--verbatim-files-from", "--files-from=/proc/self/fd/4")
	cmd.ExtraFiles = []*os.File{root, list}
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin"}

	configure(cmd)
	var logs boundedLog
	cmd.Stderr = &logs
	var consumer *exec.Cmd
	var consumerLogs boundedLog
	var pipe io.ReadCloser
	if buffer != "" {
		consumer = exec.CommandContext(ctx, buffer, "-q", "-s", "256k", "-m", "128M")
		home, e := os.MkdirTemp("", "tapepilot-buffer-")
		if e != nil {
			return Result{}, e
		}
		defer os.RemoveAll(home)
		consumer.Env = append(cmd.Env, "HOME="+home)
		configure(consumer)
		consumer.Stderr = &consumerLogs
		input, output, err := os.Pipe()
		if err != nil {
			return Result{}, err
		}
		consumer.Stdin = input
		cmd.Stdout = output
		pipe, e = consumer.StdoutPipe()
		if e != nil {
			input.Close()
			output.Close()
			return Result{}, e
		}
		if e = consumer.Start(); e != nil {
			input.Close()
			output.Close()
			pipe.Close()
			return Result{}, e
		}
		input.Close()
		e = cmd.Start()
		output.Close()
		if e != nil {
			kill(consumer)
			consumer.Wait()
			pipe.Close()
			return Result{}, e
		}
	} else {
		pipe, e = cmd.StdoutPipe()
		if e != nil {
			return Result{}, e
		}
		if e = cmd.Start(); e != nil {
			pipe.Close()
			return Result{}, e
		}
	}
	result, pumpErr := Pump(ctx, pipe, writer)
	if pumpErr != nil {
		kill(cmd)
		if consumer != nil {
			kill(consumer)
		}
	}
	pipe.Close()
	waitErr := cmd.Wait()
	var consumerErr error
	if consumer != nil {
		consumerErr = consumer.Wait()
		if consumerLogs.nonempty() && consumerErr == nil {
			consumerErr = ErrProducer

		}
	}
	// Even tar warnings can indicate a source mutation or incomplete membership.
	if waitErr != nil || consumerErr != nil || logs.nonempty() {
		return result, errors.Join(pumpErr, plan.Validate(ctx), fmt.Errorf("%w: %v", ErrProducer, errors.Join(waitErr, consumerErr)))
	}
	if pumpErr != nil {
		return result, pumpErr
	}
	if e = plan.Validate(ctx); e != nil {
		return result, e
	}
	return result, nil
}

// Pump uses one bounded 256 KiB buffer. It never retries a rejected record.
func Pump(ctx context.Context, r io.Reader, w tape.RecordWriter) (Result, error) {
	var result Result
	hash := sha256.New()
	record := make([]byte, tape.RecordBytes)
	for {
		if e := ctx.Err(); e != nil {
			return result, e
		}
		n, e := io.ReadFull(r, record)
		if e == io.EOF {
			copy(result.SHA256[:], hash.Sum(nil))
			return result, nil
		}
		if e != nil {
			return result, errors.Join(ErrRecord, e)
		}
		if n != len(record) {
			return result, ErrRecord
		}
		if e = w.WriteRecord(ctx, record); e != nil {
			return result, e
		}
		hash.Write(record)
		result.Bytes += int64(len(record))
	}
}

type boundedLog struct {
	mu   sync.Mutex
	data []byte
	seen bool
}

func (l *boundedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen = true
	left := 16384 - len(l.data)
	if left > len(p) {
		left = len(p)
	}
	l.data = append(l.data, p[:left]...)
	return len(p), nil
}
func (l *boundedLog) nonempty() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.seen }

func configure(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
}
func kill(cmd *exec.Cmd) { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
