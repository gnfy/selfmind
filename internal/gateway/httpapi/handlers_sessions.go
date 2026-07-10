package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"selfmind/internal/gateway/api"
	"selfmind/internal/kernel/memory"
)

// SessionsBackend is the daemon-side session index boundary (satisfied by
// *memory.MemoryManager, wired by the gateway runner). It powers the
// structured /v1/sessions/* endpoints that give thin clients the same session
// search / browse capability the in-process TUI had, which is the last parity
// gap before the in-process path can be removed (ACTIVE PLAN P0-3).
type SessionsBackend interface {
	SearchSessions(tenantID, query string, limit int) ([]memory.FTS5Session, error)
	ListRecentSessions(tenantID string, limit int) ([]memory.FTS5Session, error)
	GetSessionMessages(tenantID, sessionID string, aroundMessageID, window int) ([]memory.SessionMessage, error)
}

// handleSessions serves GET /v1/sessions:
//
//	?q=<query>            FTS search (else: recent sessions)
//	?limit=N              bound (default 10, max 50)
//	?session_id=<id>      that session's message window instead
//	  &around=<msgid>&window=N
//
// The partition is ALWAYS the resolved person id — the agent's storage tenant
// is the person (AGENTS.md "Context & Memory"), matching what daemon runs
// write. Never the control tenant, and never client-supplied.
func (d *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if d.Sessions == nil {
		http.Error(w, "session index is not configured", http.StatusServiceUnavailable)
		return
	}
	identity, err := d.identityFromQuery(r)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	q := r.URL.Query()
	limit := clampQueryInt(q.Get("limit"), 10, 50)

	if sessionID := strings.TrimSpace(q.Get("session_id")); sessionID != "" {
		around := clampQueryInt(q.Get("around"), 0, 1<<30)
		window := clampQueryInt(q.Get("window"), 10, 100)
		msgs, err := d.Sessions.GetSessionMessages(identity.PersonID, sessionID, around, window)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		out := api.SessionMessagesResponse{Messages: make([]api.SessionMessage, 0, len(msgs))}
		for _, m := range msgs {
			out.Messages = append(out.Messages, api.SessionMessage{
				SessionID: m.SessionID, MessageID: m.MessageID, Channel: m.Channel,
				Role: m.Role, Content: m.Content, Timestamp: m.Timestamp,
			})
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	var sessions []memory.FTS5Session
	if query := strings.TrimSpace(q.Get("q")); query != "" {
		sessions, err = d.Sessions.SearchSessions(identity.PersonID, query, limit)
	} else {
		sessions, err = d.Sessions.ListRecentSessions(identity.PersonID, limit)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	out := api.SessionsResponse{Sessions: make([]api.SessionSummary, 0, len(sessions))}
	for _, s := range sessions {
		out.Sessions = append(out.Sessions, api.SessionSummary{
			SessionID: s.SessionID, Channel: s.Channel,
			Content: s.Content, Summary: s.Summary, Timestamp: s.Timestamp,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// clampQueryInt parses a query int with a fallback and an upper bound.
func clampQueryInt(raw string, fallback, max int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n <= 0 {
		return fallback
	}
	if n > max {
		return max
	}
	return n
}
