package cliapp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"selfmind/internal/gateway/api"
	"selfmind/internal/platform/config"
)

func (a *App) runGatewayClientIfRequested() (bool, int) {
	if len(a.args) > 1 {
		switch a.args[1] {
		case "status":
			return true, a.sendGatewayMessage("/status")
		case "tasks":
			return true, a.sendGatewayMessage("/tasks")
		case "workspaces":
			return true, a.sendGatewayMessage("/workspaces")
		case "approvals":
			return true, a.sendGatewayMessage("/approvals")
		case "approve":
			if len(a.args) < 3 {
				fmt.Fprintln(a.stderr, "usage: selfmind approve <approval_id>")
				return true, 2
			}
			return true, a.sendGatewayMessage("/approve " + a.args[2])
		case "reject":
			if len(a.args) < 3 {
				fmt.Fprintln(a.stderr, "usage: selfmind reject <approval_id>")
				return true, 2
			}
			return true, a.sendGatewayMessage("/reject " + a.args[2])
		case "stop":
			return true, a.sendGatewayMessage("/stop")
		case "id":
			return true, a.sendGatewayMessage("/id")
		case "new":
			title := strings.TrimSpace(strings.Join(a.args[2:], " "))
			if title == "" {
				title = "New task"
			}
			return true, a.sendGatewayMessage("/new " + title)
		case "workspace":
			return true, a.handleWorkspaceCommand(a.args[2:])
		}
	}
	if len(a.args) > 1 && a.args[1] == "send" {
		async := false
		args := a.args[2:]
		if len(args) > 0 && args[0] == "--async" {
			async = true
			args = args[1:]
		}
		content := strings.TrimSpace(strings.Join(args, " "))
		if content == "" {
			fmt.Fprintln(a.stderr, "usage: selfmind send [--async] <message>")
			return true, 2
		}
		return true, a.sendGatewayMessageWithOptions(content, async)
	}
	if os.Getenv("SELF_USE_GATEWAY") != "1" && os.Getenv("SELF_USE_DAEMON") != "1" && !(len(a.args) > 1 && a.args[1] == "--daemon") {
		return false, 0
	}
	fmt.Fprintln(a.stdout, "SelfMind gateway client. Type /exit to quit.")
	scanner := bufio.NewScanner(a.stdin)
	for {
		fmt.Fprint(a.stdout, "> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "/exit" || line == "/quit" {
			break
		}
		_ = a.sendGatewayMessage(line)
	}
	if err := scanner.Err(); err != nil {
		fmt.Fprintln(a.stderr, err)
		return true, 1
	}
	return true, 0
}

func (a *App) sendGatewayMessage(content string) int {
	return a.sendGatewayMessageWithOptions(content, false)
}

func (a *App) sendGatewayMessageWithOptions(content string, async bool) int {
	url := a.gatewayURL()
	userID := platformUserID()
	clientCWD, _ := os.Getwd()

	req := api.MessageRequest{
		TenantID:       os.Getenv("SELF_TENANT_ID"),
		Platform:       "cli",
		PlatformUserID: userID,
		DisplayName:    userID,
		Channel:        "cli",
		Content:        content,
		WorkspaceID:    os.Getenv("SELF_WORKSPACE_ID"),
		ClientCWD:      clientCWD,
		TaskID:         os.Getenv("SELF_TASK_ID"),
		Async:          async,
	}
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(a.ctx, http.MethodPost, url+"/v1/message", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	httpReq.Header.Set("Content-Type", "application/json")
	a.attachGatewayAuth(httpReq)
	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode >= 400 {
		data, _ := io.ReadAll(httpResp.Body)
		fmt.Fprintf(a.stderr, "%s: %s\n", httpResp.Status, strings.TrimSpace(string(data)))
		return 1
	}

	var resp api.MessageResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if resp.Error != "" {
		fmt.Fprintln(a.stderr, resp.Error)
		return 1
	}
	if strings.TrimSpace(resp.Content) != "" {
		fmt.Fprintln(a.stdout, resp.Content)
		return 0
	}
	if resp.Task != nil {
		fmt.Fprintf(a.stdout, "%s [%s]\n", resp.Task.Title, resp.Task.Status)
		return 0
	}
	return 0
}

func (a *App) handleWorkspaceCommand(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.stderr, "usage: selfmind workspace [add|use] ...")
		return 2
	}
	switch args[0] {
	case "add":
		path := "."
		if len(args) >= 2 {
			path = args[1]
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			fmt.Fprintln(a.stderr, err)
			return 1
		}
		name := filepath.Base(abs)
		if len(args) >= 3 {
			name = strings.Join(args[2:], " ")
		}
		return a.registerWorkspace(abs, name)
	case "use":
		if len(args) < 2 {
			fmt.Fprintln(a.stderr, "usage: selfmind workspace use <workspace_id>")
			return 2
		}
		return a.sendGatewayMessage("/workspace " + args[1])
	default:
		fmt.Fprintln(a.stderr, "usage: selfmind workspace [add|use] ...")
		return 2
	}
}

func (a *App) registerWorkspace(path, name string) int {
	req := api.WorkspaceRegisterRequest{
		TenantID:       os.Getenv("SELF_TENANT_ID"),
		Platform:       "cli",
		PlatformUserID: platformUserID(),
		DisplayName:    platformUserID(),
		Name:           name,
		LocalPath:      path,
		AllowedRoots:   []string{path},
	}
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(a.ctx, http.MethodPost, a.gatewayURL()+"/v1/workspaces/register", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	httpReq.Header.Set("Content-Type", "application/json")
	a.attachGatewayAuth(httpReq)
	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode >= 400 {
		data, _ := io.ReadAll(httpResp.Body)
		fmt.Fprintf(a.stderr, "%s: %s\n", httpResp.Status, strings.TrimSpace(string(data)))
		return 1
	}
	var payload struct {
		Workspace struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			LocalPath string `json:"local_path"`
		} `json:"workspace"`
	}
	if err := json.NewDecoder(httpResp.Body).Decode(&payload); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Registered workspace: %s (%s)\n%s\n", payload.Workspace.Name, payload.Workspace.ID, payload.Workspace.LocalPath)
	return 0
}

func (a *App) gatewayURL() string {
	url := strings.TrimRight(os.Getenv("SELF_GATEWAY_URL"), "/")
	if url == "" {
		url = strings.TrimRight(os.Getenv("SELF_DAEMON_URL"), "/")
	}
	if url == "" {
		addr := strings.TrimSpace(os.Getenv("SELF_GATEWAY_ADDR"))
		if addr == "" {
			addr = strings.TrimSpace(os.Getenv("SELF_DAEMON_ADDR"))
		}
		if addr == "" {
			if cfg, err := config.LoadConfig(config.Options{Path: a.configPath}); err == nil {
				if cfg.Gateway.URL != "" {
					return strings.TrimRight(cfg.Gateway.URL, "/")
				}
				addr = strings.TrimSpace(cfg.Gateway.Addr)
			}
		}
		if addr == "" {
			addr = "127.0.0.1:8765"
		}
		url = "http://" + addr
	}
	return url
}

func platformUserID() string {
	userID := os.Getenv("SELF_PLATFORM_USER_ID")
	if userID == "" {
		userID = os.Getenv("USERNAME")
	}
	if userID == "" {
		userID = os.Getenv("USER")
	}
	if userID == "" {
		userID = "local"
	}
	return userID
}

func (a *App) attachGatewayAuth(req *http.Request) {
	token := os.Getenv("SELF_GATEWAY_TOKEN")
	if token == "" {
		token = os.Getenv("SELF_DAEMON_TOKEN")
	}
	if token == "" {
		if cfg, err := config.LoadConfig(config.Options{Path: a.configPath}); err == nil {
			token = strings.TrimSpace(cfg.Gateway.Token)
		}
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}
