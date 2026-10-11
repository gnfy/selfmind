package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"selfmind/internal/kernel"
)

// benchmarkToolOutput is about 1 MB of ordinary command output with a
// credential every few hundred lines, the shape a large build or log dump has.
func benchmarkToolOutput() string {
	var out strings.Builder
	for i := 0; out.Len() < 1<<20; i++ {
		switch i % 400 {
		case 7:
			fmt.Fprintf(&out, "export API_TOKEN=bench-%06d-secret-value\n", i)
		case 211:
			fmt.Fprintf(&out, "commit 3b18e512dba79e4c8300dd08aeb37f8e728b%04d Merge pull request #%d\n", i%10000, i)
		default:
			fmt.Fprintf(&out, "%06d INFO worker=%d processed item-%07d in 12.5ms status=ok path=/srv/app/data/%d.json\n", i, i%8, i, i)
		}
	}
	return out.String()
}

// benchmarkJSONOutput is about 1 MB of JSON with a credential field per item.
func benchmarkJSONOutput() string {
	items := []map[string]interface{}{}
	size := 0
	for i := 0; size < 1<<20; i++ {
		item := map[string]interface{}{"id": i, "name": fmt.Sprintf("item-%07d", i), "note": "plain text with no credential", "password": fmt.Sprintf("pw-%07d", i)}
		items = append(items, item)
		size += 110
	}
	data, _ := json.Marshal(map[string]interface{}{"items": items})
	return string(data)
}

func BenchmarkRedactionMiddleware1MBText(b *testing.B) {
	benchmarkRedactionMiddleware(b, benchmarkToolOutput())
}

func BenchmarkRedactionMiddleware1MBJSON(b *testing.B) {
	benchmarkRedactionMiddleware(b, benchmarkJSONOutput())
}

func benchmarkRedactionMiddleware(b *testing.B, output string) {
	RegisterSensitiveValue("bench-registered-secret-7d102e")
	b.SetBytes(int64(len(output)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := redactResult(output, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStream10kLines streams ten thousand output lines the way the
// terminal tool does, through the production event redactor, to a consumer.
func BenchmarkStream10kLines(b *testing.B) {
	RegisterSensitiveValue("bench-registered-secret-7d102e")
	kernel.SetAgentEventRedactor(RedactSensitive)
	b.Cleanup(func() { kernel.SetAgentEventRedactor(nil) })
	var payload strings.Builder
	for i := 0; i < 10000; i++ {
		if i%500 == 3 {
			fmt.Fprintf(&payload, "export API_TOKEN=bench-%06d-secret-value\n", i)
			continue
		}
		fmt.Fprintf(&payload, "%06d INFO worker=%d processed item-%07d in 12.5ms status=ok\n", i, i%8, i)
	}
	b.SetBytes(int64(payload.Len()))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		events := make(chan string, 64)
		received := make(chan int)
		go func() {
			n := 0
			for range events {
				n++
			}
			received <- n
		}()
		ctx := kernel.WithEventChannel(context.Background(), events)
		runner := &asyncWriterRunner{payload: payload.String()}
		if _, err := runCommandStreaming(ctx, runner, "bench", "terminal", "bench"); err != nil {
			b.Fatal(err)
		}
		close(events)
		if n := <-received; n == 0 {
			b.Fatal("no events streamed")
		}
	}
}

// asyncWriterRunner writes its payload from a goroutine, so output larger than
// a pipe buffer reaches the scanners the way a real process's output does.
type asyncWriterRunner struct {
	stdoutR, stdoutW *os.File
	stderrR, stderrW *os.File
	payload          string
	done             chan struct{}
}

func (r *asyncWriterRunner) StdoutPipe() (io.ReadCloser, error) {
	var err error
	r.stdoutR, r.stdoutW, err = os.Pipe()
	return r.stdoutR, err
}

func (r *asyncWriterRunner) StderrPipe() (io.ReadCloser, error) {
	var err error
	r.stderrR, r.stderrW, err = os.Pipe()
	return r.stderrR, err
}

func (r *asyncWriterRunner) Start() error {
	r.done = make(chan struct{})
	go func() {
		defer close(r.done)
		_, _ = r.stdoutW.WriteString(r.payload)
		_ = r.stdoutW.Close()
		_ = r.stderrW.Close()
	}()
	return nil
}

func (r *asyncWriterRunner) Wait() error {
	<-r.done
	_ = r.stdoutR.Close()
	_ = r.stderrR.Close()
	return nil
}
