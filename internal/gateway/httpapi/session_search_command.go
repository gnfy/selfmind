package httpapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/platform/textutil"
)

// searchCommandLimit bounds one /search page. History search is a reading
// surface, not a listing to page through: a person scanning for "that release"
// recognizes it in the first few hits or refines the words.
const searchCommandLimit = 10

// searchCommandReply backs the gateway /search command: find prior working
// sessions by keyword, or the most recent ones when no keyword is given.
//
// This used to be a TUI-local command while work-history search lived under
// `/tasks search`, which meant IM could search but the search itself was
// keyed on Task, and the terminal could search but only from the terminal.
// Retrieval is not endpoint-local — it is the thing that lets one person pick
// up work from wherever they are — so it is served here, once, for every
// endpoint.
func (d *Server) searchCommandReply(ctx context.Context, identity *control.IdentityContext, query string) (string, error) {
	if d == nil || d.Sessions == nil {
		return "History search is unavailable: the session index is not configured.", nil
	}
	query = strings.TrimSpace(query)
	sessions, err := d.Sessions.SearchSessions(identity.PersonID, query, searchCommandLimit)
	if query == "" {
		sessions, err = d.Sessions.ListRecentSessions(identity.PersonID, searchCommandLimit)
	}
	if err != nil {
		return "", err
	}
	if len(sessions) == 0 {
		if query == "" {
			return "No working sessions recorded yet.", nil
		}
		return fmt.Sprintf("No working sessions matched %q.", query), nil
	}

	var sb strings.Builder
	if query == "" {
		sb.WriteString("Recent working sessions:\n")
	} else {
		fmt.Fprintf(&sb, "Working sessions matching %q:\n", query)
	}
	for i, session := range sessions {
		line := strings.TrimSpace(session.Summary)
		if line == "" {
			line = toOneLine(session.Content)
		}
		fmt.Fprintf(&sb, "%d. %s\n", i+1, textutil.Truncate(toOneLine(line), 96))
		meta := make([]string, 0, 2)
		if channel := displayChannel(session.Channel); channel != "" {
			meta = append(meta, channel)
		}
		if session.Timestamp > 0 {
			meta = append(meta, time.Unix(session.Timestamp, 0).Format("2006-01-02 15:04"))
		}
		if len(meta) > 0 {
			fmt.Fprintf(&sb, "   %s\n", strings.Join(meta, " · "))
		}
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}
