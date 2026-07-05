package httpapi

import (
	"strings"
	"testing"

	"selfmind/internal/gateway/api"
)

func TestDeliveryContent(t *testing.T) {
	if got := deliveryContent(api.MessageResponse{Error: "boom"}); !strings.HasPrefix(got, "SelfMind task failed: boom") || !strings.Contains(got, "/cancel") {
		t.Fatalf("error case = %q", got)
	}
	if got := deliveryContent(api.MessageResponse{}); got != "SelfMind task finished." {
		t.Fatalf("empty case = %q", got)
	}
	if got := deliveryContent(api.MessageResponse{Content: "  daily summary  "}); got != "daily summary" {
		t.Fatalf("content case = %q", got)
	}
}

func TestCronRunFailed(t *testing.T) {
	if !cronRunFailed(api.MessageResponse{Error: "x"}) {
		t.Fatal("error should be a failure")
	}
	if !cronRunFailed(api.MessageResponse{Turn: &api.TurnStatus{Status: "failed"}}) {
		t.Fatal("failed turn should be a failure")
	}
	if !cronRunFailed(api.MessageResponse{Content: "   "}) {
		t.Fatal("empty content should be a failure")
	}
	if cronRunFailed(api.MessageResponse{Content: "READY", Turn: &api.TurnStatus{Status: "completed"}}) {
		t.Fatal("healthy response should not be a failure")
	}
}
