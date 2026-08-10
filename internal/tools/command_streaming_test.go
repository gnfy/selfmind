package tools

import (
	"context"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// drainOrderRunner imitates the one property of os/exec that made the real
// defect possible: Wait tears the pipes down. It records whether Wait was
// called before or after the child's output reached EOF.
type drainOrderRunner struct {
	stdoutR, stdoutW *os.File
	stderrR, stderrW *os.File
	writersClosed    atomic.Bool
	waitAfterEOF     atomic.Bool
	waitCalled       atomic.Bool
	holdWriters      time.Duration
	payload          string
}

func (r *drainOrderRunner) StdoutPipe() (io.ReadCloser, error) {
	var err error
	r.stdoutR, r.stdoutW, err = os.Pipe()
	return r.stdoutR, err
}

func (r *drainOrderRunner) StderrPipe() (io.ReadCloser, error) {
	var err error
	r.stderrR, r.stderrW, err = os.Pipe()
	return r.stderrR, err
}

// Start writes the child's whole output immediately but keeps the write ends
// open for a moment, the way a real process does between its last write and
// its exit. A reader that has not drained by then is exactly the reader the
// old ordering raced.
func (r *drainOrderRunner) Start() error {
	if _, err := r.stdoutW.WriteString(r.payload); err != nil {
		return err
	}
	go func() {
		time.Sleep(r.holdWriters)
		r.writersClosed.Store(true)
		_ = r.stdoutW.Close()
		_ = r.stderrW.Close()
	}()
	return nil
}

func (r *drainOrderRunner) Wait() error {
	r.waitCalled.Store(true)
	r.waitAfterEOF.Store(r.writersClosed.Load())
	// os/exec closes the pipes here. Anything still buffered is gone.
	_ = r.stdoutR.Close()
	_ = r.stderrR.Close()
	return nil
}

// Wait must not run until both scanners have finished. os/exec closes the
// pipes returned by StdoutPipe/StderrPipe once the process exits, so reaping
// first races the in-flight read: the command succeeds, the error is nil, and
// the output is empty. A one-shot `printf READY` has the widest window, which
// is how it reached CI as "the watch never completed" and "the durable request
// produced nothing".
func TestRunCommandStreamingDrainsBeforeReaping(t *testing.T) {
	runner := &drainOrderRunner{payload: "READY", holdWriters: 50 * time.Millisecond}

	output, err := runCommandStreaming(context.Background(), runner, "printf READY", "terminal", "call-1")
	if err != nil {
		t.Fatalf("streaming failed: %v", err)
	}
	if !runner.waitCalled.Load() {
		t.Fatal("the process was never reaped")
	}
	if !runner.waitAfterEOF.Load() {
		t.Fatal("Wait ran while the scanners were still reading; its pipe teardown can drop the output")
	}
	if !strings.Contains(output, "READY") {
		t.Fatalf("output = %q, want it to contain READY", output)
	}
}

// The unterminated final line is the shape that loses data first, so pin that
// it survives on its own.
func TestRunCommandStreamingKeepsUnterminatedFinalLine(t *testing.T) {
	runner := &drainOrderRunner{payload: "no-trailing-newline", holdWriters: time.Millisecond}

	output, err := runCommandStreaming(context.Background(), runner, "printf x", "terminal", "call-2")
	if err != nil {
		t.Fatalf("streaming failed: %v", err)
	}
	if output != "no-trailing-newline" {
		t.Fatalf("output = %q, want the unterminated line preserved", output)
	}
}
