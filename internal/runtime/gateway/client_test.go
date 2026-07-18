package gateway

import (
	"reflect"
	"testing"
)

func TestDetachedRunArgsDoNotLeakLifecycleFlags(t *testing.T) {
	want := []string{"gateway", "run"}
	if got := detachedRunArgs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("detachedRunArgs() = %v, want %v", got, want)
	}
}
