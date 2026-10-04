package httpapi

import (
	"context"
	"fmt"
	"strings"

	"selfmind/internal/control"
)

// inboundDiagReply exposes only this person's unresolved receipts. A claimed
// input may already have run a tool, so the diagnostic deliberately has no
// retry button; the person must inspect the exact work/effect before resending.
func (d *Server) inboundDiagReply(ctx context.Context, identity *control.IdentityContext) (string, error) {
	rows, err := d.Control.ListUncertainInboundForPerson(ctx, identity.TenantID, identity.PersonID, 20)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "No unresolved inbound messages.", nil
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Unresolved inbound messages: %d", len(rows))
	if len(rows) == 20 {
		out.WriteString(" or more")
	}
	out.WriteByte('\n')
	for _, row := range rows {
		fmt.Fprintf(&out, "- %s/%s · %s · %s\n", row.Platform, shortOpaqueID(row.MessageID), row.State,
			truncate(toOneLine(row.Preview), 100))
	}
	out.WriteString("Pending input may be retried by its source. Dispatching input may already have caused effects; inspect the related work before sending it again.")
	return out.String(), nil
}
