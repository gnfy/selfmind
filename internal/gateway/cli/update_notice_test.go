package cli

import (
	"strings"
	"testing"
)

func updateNoticeText(m *uiModel) string {
	for _, msg := range m.messages {
		if msg.Role == "notice" && strings.Contains(msg.Content, "Update available") {
			return msg.Content
		}
	}
	return ""
}

// TestMaybeAnnounceUpdateIdle: an idle session consumes the one-shot result
// and commits exactly one compact notice line; the channel is dropped so
// later Update passes cannot announce again.
func TestMaybeAnnounceUpdateIdle(t *testing.T) {
	ch := make(chan UpdateNotice, 1)
	ch <- UpdateNotice{Version: "0.1.0-beta.9"}
	m := &uiModel{updateNotices: ch}

	m.maybeAnnounceUpdate()
	got := updateNoticeText(m)
	if !strings.Contains(got, "0.1.0-beta.9") || !strings.Contains(got, "selfmind update") {
		t.Fatalf("notice = %q, want version and default command", got)
	}
	if m.updateNotices != nil {
		t.Fatal("channel must be dropped after consumption")
	}
	count := len(m.messages)
	m.maybeAnnounceUpdate()
	if len(m.messages) != count {
		t.Fatal("second pass must not announce again")
	}
}

// TestMaybeAnnounceUpdateDefersWhileBusy: a streaming run or an armed
// approval panel leaves the buffered result in the channel (structural
// deferral); it announces once the session is idle again.
func TestMaybeAnnounceUpdateDefersWhileBusy(t *testing.T) {
	ch := make(chan UpdateNotice, 1)
	ch <- UpdateNotice{Version: "0.1.0-beta.9"}
	m := &uiModel{updateNotices: ch, thinking: true}

	m.maybeAnnounceUpdate()
	if updateNoticeText(m) != "" {
		t.Fatal("must not announce while a run is streaming")
	}
	if m.updateNotices == nil {
		t.Fatal("busy pass must keep the buffered result for later")
	}

	m.thinking = false
	m.maybeAnnounceUpdate()
	if updateNoticeText(m) == "" {
		t.Fatal("idle pass after busy must announce")
	}
}

// TestMaybeAnnounceUpdateDedupesStartupNotice: the startup cache-based notice
// already announced this version, so the in-session pass stays silent.
func TestMaybeAnnounceUpdateDedupesStartupNotice(t *testing.T) {
	ch := make(chan UpdateNotice, 1)
	ch <- UpdateNotice{Version: "0.1.0-beta.9"}
	m := &uiModel{updateNotices: ch, updateNoticeAnnounced: "0.1.0-beta.9"}

	m.maybeAnnounceUpdate()
	if updateNoticeText(m) != "" {
		t.Fatal("version announced at startup must not announce again in-session")
	}
}

// TestMaybeAnnounceUpdateNilChannel: sessions without a pending check (fresh
// cache, updates disabled, dev build) are a strict no-op.
func TestMaybeAnnounceUpdateNilChannel(t *testing.T) {
	m := &uiModel{}
	m.maybeAnnounceUpdate()
	if len(m.messages) != 0 {
		t.Fatal("nil channel must be a no-op")
	}
}
