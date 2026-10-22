package tools

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"selfmind/internal/kernel"
)

// stagedRunner writes its steps to stdout and stderr in turn, pausing after
// each so that the scanners read it before the next, which fixes the order in
// which the lines of the two streams reach the capture.
type stagedRunner struct {
	stdoutR, stdoutW *os.File
	stderrR, stderrW *os.File
	steps            []stagedWrite
	pause            time.Duration
	done             chan struct{}
}

type stagedWrite struct {
	stderr bool
	text   string
}

func (r *stagedRunner) StdoutPipe() (io.ReadCloser, error) {
	var err error
	r.stdoutR, r.stdoutW, err = os.Pipe()
	return r.stdoutR, err
}

func (r *stagedRunner) StderrPipe() (io.ReadCloser, error) {
	var err error
	r.stderrR, r.stderrW, err = os.Pipe()
	return r.stderrR, err
}

func (r *stagedRunner) Start() error {
	r.done = make(chan struct{})
	go func() {
		defer close(r.done)
		for _, step := range r.steps {
			target := r.stdoutW
			if step.stderr {
				target = r.stderrW
			}
			_, _ = target.WriteString(step.text)
			time.Sleep(r.pause)
		}
		_ = r.stdoutW.Close()
		_ = r.stderrW.Close()
	}()
	return nil
}

func (r *stagedRunner) Wait() error {
	<-r.done
	_ = r.stdoutR.Close()
	_ = r.stderrR.Close()
	return nil
}

// A registered multi-line secret that one stream prints with the other
// stream's lines between its own is masked in the captured result, as the
// live stream masks it: each stream is read on its own.
func TestInterleavedStreamsDoNotHideARegisteredSecret(t *testing.T) {
	RegisterSensitiveValue("r3-ml-one-7d102e\nr3-ml-two-7d102e\nr3-ml-three-7d102e")
	hidden := []string{"r3-ml-one-7d102e", "r3-ml-two-7d102e", "r3-ml-three-7d102e"}
	for name, steps := range map[string][]stagedWrite{
		"stdout with stderr between": {
			{false, "r3-ml-one-7d102e\n"}, {true, "warning: retrying\n"}, {false, "r3-ml-two-7d102e\n"},
			{true, "warning: still retrying\n"}, {false, "r3-ml-three-7d102e\n"},
		},
		"stderr with stdout between": {
			{true, "r3-ml-one-7d102e\n"}, {false, "progress 1\n"}, {true, "r3-ml-two-7d102e\n"},
			{false, "progress 2\n"}, {true, "r3-ml-three-7d102e\n"},
		},
		"one gap": {
			{false, "r3-ml-one-7d102e\n"}, {false, "r3-ml-two-7d102e\n"}, {true, "warning: retrying\n"}, {false, "r3-ml-three-7d102e\n"},
		},
	} {
		var out string
		for attempt := 0; ; attempt++ {
			runner := &stagedRunner{steps: steps, pause: 40 * time.Millisecond}
			var err error
			if out, err = runCommandStreaming(context.Background(), runner, "stage", "terminal", "call"); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			// The capture is in arrival order: the other stream's line lies
			// between the secret's lines, not before or after all of them.
			lines := strings.Split(out, "\n")
			noise := -1
			for i, line := range lines {
				if strings.HasPrefix(line, "warning: ") || strings.HasPrefix(line, "progress ") {
					noise = i
				}
			}
			if noise > 0 && noise < len(lines)-1 {
				break
			}
			if attempt == 2 {
				t.Fatalf("%s: the lines of the two streams did not interleave: %q", name, out)
			}
		}
		for _, secret := range hidden {
			if strings.Contains(out, secret) || strings.Contains(RedactSensitive(out), secret) {
				t.Errorf("%s: the captured result shows %q:\n%s", name, secret, out)
			}
		}
	}
	// Output that is not interleaved is left to the result middleware.
	runner := &stagedRunner{steps: []stagedWrite{{false, "r3-ml-one-7d102e\nr3-ml-two-7d102e\nr3-ml-three-7d102e\n"}, {true, "warning: done\n"}}, pause: 40 * time.Millisecond}
	out, err := runCommandStreaming(context.Background(), runner, "stage", "terminal", "call")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(RedactSensitive(out), "r3-ml-two-7d102e") {
		t.Errorf("a contiguous secret leaked: %s", out)
	}
}

// The command a run shows is masked once, not again with every line of its
// output: a long command on a long output costs no more than a short one.
func TestShownCommandIsMaskedOncePerCall(t *testing.T) {
	RegisterSensitiveValue("r3-command-secret-7d102e")
	var seen atomic.Int64
	kernel.SetAgentEventRedactor(func(value string) string {
		seen.Add(int64(len(value)))
		return RedactSensitive(value)
	})
	t.Cleanup(func() { kernel.SetAgentEventRedactor(nil) })
	command := "echo r3-command-secret-7d102e " + strings.Repeat("x", 100<<10)
	var payload strings.Builder
	const lines = 2000
	for i := 0; i < lines; i++ {
		fmt.Fprintf(&payload, "line %05d of the output\n", i)
	}
	events := make(chan string, 8192)
	ctx := kernel.WithEventChannel(context.Background(), events)
	if _, err := runCommandStreaming(ctx, &asyncWriterRunner{payload: payload.String()}, command, "terminal", "call"); err != nil {
		t.Fatal(err)
	}
	close(events)
	shown, count := "", 0
	for raw := range events {
		event, ok := kernel.DecodeAgentEvent(raw)
		if !ok || event.Type != "tool.output" {
			continue
		}
		count++
		shown, _ = event.Payload["command"].(string)
	}
	if count == 0 {
		t.Fatal("no output was streamed")
	}
	if strings.Contains(shown, "r3-command-secret-7d102e") || !strings.Contains(shown, "[REDACTED]") {
		t.Errorf("the shown command was not masked: %.80s", shown)
	}
	// Masking the command again on every line is 2000 times its size; each
	// line and the events around it are small.
	if total := seen.Load(); total > int64(10*len(command)) {
		t.Errorf("the event redactor was given %d bytes for a %d byte command and %d lines", total, len(command), lines)
	}
}

// A plural name's value that counts something is no credential, also as a
// JSON string in text that is not a document of its own.
func TestQuotedCountsUnderPluralNamesAreKept(t *testing.T) {
	for _, in := range []string{
		`secrets: "3"`,
		`"secrets": "3",`,
		`  "passwords": "12",`,
		`apiKeys: '2'`,
		`"api_keys": "0"`,
	} {
		assertChainShows(t, in, in)
	}
	// A value that is not a count is one.
	for _, tc := range []struct{ in, hidden string }{
		{`secrets: "r3count-7d102e"`, "r3count-7d102e"},
		{`"passwords": "r3count2-7d102e",`, "r3count2-7d102e"},
		{`"passwords": "12r3count3",`, "12r3count3"},
	} {
		assertChainHides(t, tc.in, tc.in, tc.hidden)
	}
}
