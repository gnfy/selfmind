package eval

import (
	"context"
	"net/http"

	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/kernel/task/cron"
)

// fireScheduledTurn fires a scheduled turn's input as the prompt of a job of
// the turn's account through the daemon's cron executor: the path stored text
// takes into the gateway when its schedule fires. The answer is the one the
// executor delivers to the job's chat.
func (h *runtimeHarness) fireScheduledTurn(ctx context.Context, req api.MessageRequest) (api.MessageResponse, int) {
	creator, err := h.server.Control.ResolveAccount(ctx, req.TenantID, req.Platform, req.PlatformUserID)
	if err != nil {
		return api.MessageResponse{Error: err.Error()}, http.StatusInternalServerError
	}
	if creator == nil {
		return api.MessageResponse{Error: "the scheduled turn's creating account is not bound"}, http.StatusNotFound
	}
	resp, err := httpapi.NewCronExecutor(h.server, nil).Fire(ctx, cron.CronJob{
		Name: "eval scheduled turn", TenantID: req.TenantID, CreatorPersonID: creator.PersonID, Platform: req.Platform,
		DeliverTo: req.PlatformUserID, Prompt: req.Content,
	})
	switch {
	case err != nil:
		if resp.Error == "" {
			resp.Error = err.Error()
		}
		return resp, http.StatusInternalServerError
	case resp.Error != "":
		return resp, http.StatusInternalServerError
	default:
		return resp, http.StatusOK
	}
}
