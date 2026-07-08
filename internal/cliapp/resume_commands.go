package cliapp

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"selfmind/internal/gateway/api"
)

func (a *App) extractTaskResumeCommand() (bool, int) {
	if len(a.args) < 2 || a.args[1] != "resume" {
		return false, 0
	}
	if len(a.args) != 3 || strings.TrimSpace(a.args[2]) == "" {
		fmt.Fprintln(a.stderr, "usage: selfmind resume <n|task_id>")
		return true, 2
	}
	a.resumeTaskRef = strings.TrimSpace(a.args[2])
	a.args = []string{a.args[0]}
	return true, 0
}

func (a *App) pinResumeTask(processor func(context.Context, api.MessageRequest) (api.MessageResponse, int)) error {
	ref := strings.TrimSpace(a.resumeTaskRef)
	if ref == "" || processor == nil {
		return nil
	}
	userID := platformUserID()
	clientCWD, _ := os.Getwd()
	resp, status := processor(a.ctx, api.MessageRequest{
		TenantID:       os.Getenv("SELF_TENANT_ID"),
		Platform:       "cli",
		PlatformUserID: userID,
		DisplayName:    userID,
		Channel:        "cli",
		Content:        "/resume " + ref,
		WorkspaceID:    os.Getenv("SELF_WORKSPACE_ID"),
		ClientCWD:      clientCWD,
	})
	if status != http.StatusOK {
		if resp.Error != "" {
			return fmt.Errorf("%s", resp.Error)
		}
		return fmt.Errorf("resume request failed with status %d", status)
	}
	if resp.Error != "" {
		return fmt.Errorf("%s", resp.Error)
	}
	if content := strings.TrimSpace(resp.Content); content != "" {
		fmt.Fprintln(a.stdout, content)
	}
	return nil
}
