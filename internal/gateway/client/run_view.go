package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"selfmind/internal/gateway/api"
)

func (c *Client) ViewRun(ctx context.Context, runID string) (api.RunView, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	query := url.Values{"platform": {"cli"}, "platform_user_id": {clientUserID()}, "run_id": {runID}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/runs/view?"+query.Encode(), nil)
	if err != nil {
		return api.RunView{}, err
	}
	c.auth(req)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return api.RunView{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return api.RunView{}, fmt.Errorf("Run view is unavailable (HTTP %d)", resp.StatusCode)
	}
	var view api.RunView
	err = json.NewDecoder(resp.Body).Decode(&view)
	return view, err
}
