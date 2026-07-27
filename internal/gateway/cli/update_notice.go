package cli

import "strings"

// UpdateNotice is a completed background update-check result the CLI layer
// forwards into the live TUI session, so the user learns about a release in
// this session instead of at the next startup. Best-effort end to end: a nil
// channel, an empty result, or a version already announced at startup all
// no-op.
type UpdateNotice struct {
	// Version is the available version, e.g. "0.1.0-beta.9".
	Version string
	// Command is the suggested install command; empty falls back to
	// "selfmind update".
	Command string
}

// SetUpdateNotices wires the background update-check result channel into the
// session. alreadyAnnounced is the version the startup (cache-based) notice
// printed, if any — the in-session announcement dedupes against it so the
// same release is never announced twice in one sitting. Must be called before
// Start().
func (c *Controller) SetUpdateNotices(ch <-chan UpdateNotice, alreadyAnnounced string) {
	if c == nil || c.model == nil {
		return
	}
	c.model.updateNotices = ch
	c.model.updateNoticeAnnounced = strings.TrimSpace(alreadyAnnounced)
}

// maybeAnnounceUpdate consumes a pending update-check result when the session
// is idle and commits ONE compact notice line to the transcript. Deferral is
// structural: while a run is streaming or an approval panel is armed the
// buffered result simply stays in the channel, so no extra state is needed.
// The channel is one-shot — after the first receive it is dropped.
func (m *uiModel) maybeAnnounceUpdate() {
	if m.updateNotices == nil || m.thinking || m.approvalPrompt != nil {
		return
	}
	select {
	case n := <-m.updateNotices:
		m.updateNotices = nil
		version := strings.TrimSpace(n.Version)
		if version == "" || version == m.updateNoticeAnnounced {
			return
		}
		m.updateNoticeAnnounced = version
		command := strings.TrimSpace(n.Command)
		if command == "" {
			command = "selfmind update"
		}
		m.addMessage("notice", "⬆ Update available: SelfMind "+version+" → run `"+command+"`")
	default:
	}
}
