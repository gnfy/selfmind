package gateway

import (
	"context"
	"reflect"
	"testing"

	"selfmind/internal/platform/config"
)

func TestLocalControlStopKeepsAnotherRuntimePrivateState(t *testing.T) {
	first := startLocalControl(context.Background(), shortDataDir(t), &config.Config{}, "127.0.0.1:0")
	t.Cleanup(first.stop)
	before := first.execution.PrivateState()
	second := startLocalControl(context.Background(), shortDataDir(t), &config.Config{}, "127.0.0.1:0")
	second.stop()
	if !reflect.DeepEqual(before, first.execution.PrivateState()) {
		t.Fatal("stopping another runtime changed this daemon's private state")
	}
	if first.execution.HostStateRecordsDir() == "" || first.execution == second.execution {
		t.Fatal("each daemon must retain an owned execution runtime")
	}
}
