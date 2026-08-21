package cliapp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"selfmind/internal/gateway/api"
	"selfmind/internal/platform/config"
	gatewayrt "selfmind/internal/runtime/gateway"
	"selfmind/internal/tools"
)

func (a *App) runGatewayClientIfRequested() (bool, int) {
	if len(a.args) > 1 {
		switch a.args[1] {
		case "status":
			return true, a.sendGatewayMessage("/status")
		case "usage":
			return true, a.sendGatewayMessage("/report daily --since 24h")
		case "report":
			if len(a.args) < 3 || a.args[2] != "daily" {
				fmt.Fprintln(a.stderr, "usage: selfmind report daily [--since 24h]")
				return true, 2
			}
			return true, a.sendGatewayMessage("/report " + strings.Join(a.args[2:], " "))
		case "watchers":
			return true, a.sendGatewayMessage(strings.TrimSpace("/watchers " + strings.Join(a.args[2:], " ")))
		case "tasks":
			// Forward the view variant (done|archived|all) so `selfmind tasks
			// done` matches the gateway /tasks grammar instead of dropping it.
			if len(a.args) > 2 {
				return true, a.sendGatewayMessage("/tasks " + strings.Join(a.args[2:], " "))
			}
			return true, a.sendGatewayMessage("/tasks")
		case "task":
			// Keep task detail and management commands available as short-lived
			// CLI calls instead of falling through to the interactive TUI.
			return true, a.sendGatewayMessage(strings.TrimSpace("/task " + strings.Join(a.args[2:], " ")))
		case "workspaces":
			return true, a.sendGatewayMessage("/workspaces")
		case "ws":
			// Short alias for `workspace` (unified verb: bare lists, arg selects,
			// add/use manage).
			return true, a.handleWorkspaceCommand(a.args[2:])
		case "approvals":
			// Subcommands (grants | revoke <n>) forward verbatim so CLI and IM
			// share one grammar and one resolver.
			if len(a.args) > 2 {
				return true, a.sendGatewayMessage("/approvals " + strings.Join(a.args[2:], " "))
			}
			return true, a.sendGatewayMessage("/approvals")
		case "approve":
			// Token is optional: the daemon resolves ordinals ("1"), unique apr_
			// prefixes, full ids, and — with a single pending approval — no token
			// at all. Keeping resolution server-side means CLI and IM behave
			// identically.
			token := ""
			if len(a.args) >= 3 {
				token = a.args[2]
			}
			return true, a.sendGatewayMessage(strings.TrimSpace("/approve " + token))
		case "reject":
			token := ""
			if len(a.args) >= 3 {
				token = a.args[2]
			}
			return true, a.sendGatewayMessage(strings.TrimSpace("/reject " + token))
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
		opts := sendOptions{}
		args := a.args[2:]
		usage := "usage: selfmind send [--async] [--mode on-request|read-only|auto-edit|full-auto|smart] <message>"
	flagLoop:
		for len(args) > 0 {
			switch {
			case args[0] == "--async":
				opts.async = true
				args = args[1:]
			case args[0] == "--mode":
				if len(args) < 2 {
					fmt.Fprintln(a.stderr, usage)
					return true, 2
				}
				opts.approvalMode = args[1]
				args = args[2:]
			case strings.HasPrefix(args[0], "--mode="):
				opts.approvalMode = strings.TrimPrefix(args[0], "--mode=")
				args = args[1:]
			default:
				break flagLoop
			}
		}
		if opts.approvalMode != "" && !isValidApprovalMode(opts.approvalMode) {
			fmt.Fprintf(a.stderr, "invalid --mode %q; valid modes: on-request, read-only, auto-edit, full-auto, smart\n", opts.approvalMode)
			return true, 2
		}
		content := strings.TrimSpace(strings.Join(args, " "))
		if content == "" {
			fmt.Fprintln(a.stderr, usage)
			return true, 2
		}
		return true, a.sendGatewayMessageWithOptions(content, opts)
	}
	// Positional arguments that are not recognized above are not interactive
	// input. Let the root dispatcher report them as unknown commands instead of
	// silently starting a long-lived TUI process.
	if len(a.args) > 1 && a.args[1] != "--daemon" {
		return false, 0
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

// sendOptions carries per-send flags from CLI parsing to the message request.
type sendOptions struct {
	async bool
	// approvalMode is the codex-style per-request approval policy
	// (MessageRequest.ApprovalMode); empty keeps the daemon default.
	approvalMode string
}

// isValidApprovalMode gates the CLI --mode flag to the canonical approval
// modes so a typo fails fast instead of silently degrading to the default
// policy. The flag is intentionally stricter than the /mode command (it rejects
// aliases like "yolo"), so it checks the canonical set from internal/tools —
// the single source — rather than keeping its own copy.
func isValidApprovalMode(mode string) bool {
	m := strings.ToLower(strings.TrimSpace(mode))
	for _, canonical := range tools.CanonicalApprovalModes() {
		if m == canonical {
			return true
		}
	}
	return false
}

func (a *App) sendGatewayMessage(content string) int {
	return a.sendGatewayMessageWithOptions(content, sendOptions{})
}

func (a *App) sendGatewayMessageWithOptions(content string, opts sendOptions) int {
	a.ensureLocalGateway()
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
		Async:          opts.async,
		ApprovalMode:   strings.ToLower(strings.TrimSpace(opts.approvalMode)),
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
		// The daemon returns {"error": "..."} payloads with human-readable
		// messages; show that single line instead of dumping raw JSON.
		fmt.Fprintln(a.stderr, gatewayErrorLine(httpResp.Status, data))
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
		if strings.TrimSpace(content) == "/status" {
			a.printPromptCustomizationHint()
		}
		return 0
	}
	if resp.Task != nil {
		fmt.Fprintf(a.stdout, "%s [%s]\n", resp.Task.Title, resp.Task.Status)
		return 0
	}
	return 0
}

// gatewayErrorLine reduces a gateway error response to one human-readable
// line. The daemon replies with JSON carrying an "error" field; anything else
// falls back to the raw (trimmed) body.
func gatewayErrorLine(status string, body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &payload) == nil && strings.TrimSpace(payload.Error) != "" {
		return strings.TrimSpace(payload.Error)
	}
	if trimmed == "" {
		return status
	}
	return status + ": " + trimmed
}

// handleWorkspaceCommand backs both `selfmind workspace ...` and its short
// alias `selfmind ws ...`. Unified verb: bare lists, `add`/`use` are the
// management subcommands, and a bare number/id selects — so
// `ws`, `ws 2`, `ws add <path>`, `ws use <id>` all read the same way.
func (a *App) handleWorkspaceCommand(args []string) int {
	if len(args) == 0 {
		// Bare `workspace`/`ws` lists, mirroring the gateway's unified verb.
		return a.sendGatewayMessage("/workspaces")
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
			fmt.Fprintln(a.stderr, "usage: selfmind ws use <workspace_id>")
			return 2
		}
		return a.sendGatewayMessage("/workspace " + args[1])
	case "list":
		return a.sendGatewayMessage("/workspaces")
	case "trust", "untrust":
		workspaceID := ""
		if len(args) >= 2 {
			workspaceID = args[1]
		}
		level := "trusted"
		if args[0] == "untrust" {
			level = "untrusted"
		}
		return a.setWorkspaceTrust(workspaceID, level)
	case "grants":
		workspaceID := ""
		if len(args) >= 2 {
			workspaceID = args[1]
		}
		return a.listWorkspaceCapabilities(workspaceID)
	case "observe":
		return a.registerWorkspaceObservationProfile(args[1:])
	case "revoke":
		if len(args) < 2 {
			fmt.Fprintln(a.stderr, "usage: selfmind ws revoke <capability> [workspace_id]")
			return 2
		}
		workspaceID := ""
		if len(args) >= 3 {
			workspaceID = args[2]
		}
		return a.revokeWorkspaceCapability(args[1], workspaceID)
	default:
		// A bare number or id selects the workspace (ws 2, ws ws_abc123).
		return a.sendGatewayMessage("/workspace " + args[0])
	}
}

func (a *App) registerWorkspaceObservationProfile(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(a.stderr, "usage: selfmind ws observe <script> [--network] [--credentials] [--all-args | -- <argv-prefix...>] [--workspace <id>]")
		return 2
	}
	req := api.WorkspaceObservationProfileRequest{
		TenantID: os.Getenv("SELF_TENANT_ID"), Platform: "cli", PlatformUserID: platformUserID(), ScriptPath: args[0],
	}
	separator := -1
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--network":
			req.AllowNetwork = true
		case "--credentials":
			req.AllowCredentials = true
		case "--all-args":
			req.AllowTrailing = true
		case "--workspace":
			if i+1 >= len(args) {
				fmt.Fprintln(a.stderr, "--workspace requires an id")
				return 2
			}
			i++
			req.WorkspaceID = args[i]
		case "--":
			separator = i
			i = len(args)
		default:
			fmt.Fprintf(a.stderr, "unknown observe option %q; put the script argument prefix after --\n", args[i])
			return 2
		}
	}
	if separator >= 0 {
		req.ArgvPrefix = append([]string{}, args[separator+1:]...)
		req.AllowTrailing = true
	}
	if req.AllowTrailing && separator < 0 && !containsString(args[1:], "--all-args") {
		fmt.Fprintln(a.stderr, "use --all-args explicitly to allow arbitrary script arguments")
		return 2
	}

	a.ensureLocalGateway()
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(a.ctx, http.MethodPost, a.gatewayURL()+"/v1/workspaces/observation-profiles", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	httpReq.Header.Set("Content-Type", "application/json")
	a.attachGatewayAuth(httpReq)
	a.attachLocalControlAuth(httpReq)
	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode >= 400 {
		data, _ := io.ReadAll(httpResp.Body)
		fmt.Fprintln(a.stderr, gatewayErrorLine(httpResp.Status, data))
		return 1
	}
	var payload struct {
		WorkspaceID string `json:"workspace_id"`
		Profile     struct {
			Label string `json:"label"`
		} `json:"profile"`
	}
	if err := json.NewDecoder(httpResp.Body).Decode(&payload); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Observation profile added for workspace %s: %s.\nIt becomes invalid if the script changes; review or revoke it with `/approvals grants`.\n", payload.WorkspaceID, payload.Profile.Label)
	return 0
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (a *App) listWorkspaceCapabilities(workspaceID string) int {
	a.ensureLocalGateway()
	query := url.Values{
		"tenant_id":        []string{os.Getenv("SELF_TENANT_ID")},
		"platform":         []string{"cli"},
		"platform_user_id": []string{platformUserID()},
	}
	if workspaceID = strings.TrimSpace(workspaceID); workspaceID != "" {
		query.Set("workspace_id", workspaceID)
	}
	httpReq, err := http.NewRequestWithContext(
		a.ctx,
		http.MethodGet,
		a.gatewayURL()+"/v1/workspaces/capabilities?"+query.Encode(),
		nil,
	)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	a.attachGatewayAuth(httpReq)
	a.attachLocalControlAuth(httpReq)
	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode >= 400 {
		data, _ := io.ReadAll(httpResp.Body)
		fmt.Fprintln(a.stderr, gatewayErrorLine(httpResp.Status, data))
		return 1
	}
	var payload struct {
		WorkspaceID  string                    `json:"workspace_id"`
		Capabilities []api.WorkspaceCapability `json:"capabilities"`
	}
	if err := json.NewDecoder(httpResp.Body).Decode(&payload); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	if len(payload.Capabilities) == 0 {
		fmt.Fprintf(a.stdout, "No active execution capabilities for workspace %s.\n", payload.WorkspaceID)
		return 0
	}
	fmt.Fprintf(a.stdout, "Execution capabilities for workspace %s:\n", payload.WorkspaceID)
	for _, grant := range payload.Capabilities {
		fmt.Fprintf(a.stdout, "- %s (expires %s", grant.Capability, grant.ExpiresAt.Local().Format("2006-01-02 15:04 MST"))
		if strings.TrimSpace(grant.GrantedBy) != "" {
			fmt.Fprintf(a.stdout, ", granted by %s", grant.GrantedBy)
		}
		fmt.Fprintln(a.stdout, ")")
	}
	return 0
}

func (a *App) revokeWorkspaceCapability(capability, workspaceID string) int {
	a.ensureLocalGateway()
	req := api.WorkspaceCapabilityRequest{
		TenantID:       os.Getenv("SELF_TENANT_ID"),
		Platform:       "cli",
		PlatformUserID: platformUserID(),
		WorkspaceID:    strings.TrimSpace(workspaceID),
		Capability:     strings.TrimSpace(capability),
	}
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(
		a.ctx,
		http.MethodDelete,
		a.gatewayURL()+"/v1/workspaces/capabilities",
		bytes.NewReader(body),
	)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	httpReq.Header.Set("Content-Type", "application/json")
	a.attachGatewayAuth(httpReq)
	a.attachLocalControlAuth(httpReq)
	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode >= 400 {
		data, _ := io.ReadAll(httpResp.Body)
		fmt.Fprintln(a.stderr, gatewayErrorLine(httpResp.Status, data))
		return 1
	}
	var payload struct {
		Revoked     string `json:"revoked"`
		WorkspaceID string `json:"workspace_id"`
	}
	if err := json.NewDecoder(httpResp.Body).Decode(&payload); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Revoked %s for workspace %s.\n", payload.Revoked, payload.WorkspaceID)
	return 0
}

func (a *App) setWorkspaceTrust(workspaceID, trustLevel string) int {
	a.ensureLocalGateway()
	req := api.WorkspaceTrustRequest{
		TenantID:       os.Getenv("SELF_TENANT_ID"),
		Platform:       "cli",
		PlatformUserID: platformUserID(),
		WorkspaceID:    strings.TrimSpace(workspaceID),
		TrustLevel:     trustLevel,
	}
	body, _ := json.Marshal(req)
	httpReq, err := http.NewRequestWithContext(a.ctx, http.MethodPost, a.gatewayURL()+"/v1/workspaces/trust", bytes.NewReader(body))
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	httpReq.Header.Set("Content-Type", "application/json")
	a.attachGatewayAuth(httpReq)
	a.attachLocalControlAuth(httpReq)
	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode >= 400 {
		data, _ := io.ReadAll(httpResp.Body)
		fmt.Fprintln(a.stderr, gatewayErrorLine(httpResp.Status, data))
		return 1
	}
	var payload struct {
		Workspace struct {
			Name       string `json:"name"`
			LocalPath  string `json:"local_path"`
			TrustLevel string `json:"trust_level"`
		} `json:"workspace"`
	}
	if err := json.NewDecoder(httpResp.Body).Decode(&payload); err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	fmt.Fprintf(a.stdout, "Workspace %s is now %s.\n%s\n", payload.Workspace.Name, payload.Workspace.TrustLevel, payload.Workspace.LocalPath)
	return 0
}

func (a *App) attachLocalControlAuth(req *http.Request) {
	if req == nil {
		return
	}
	if token, err := gatewayrt.ReadLocalControlToken(a.gatewayDataDir()); err == nil {
		req.Header.Set(api.LocalControlTokenHeader, token)
	}
}

func (a *App) registerWorkspace(path, name string) int {
	a.ensureLocalGateway()
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
	a.attachLocalControlAuth(httpReq)
	httpResp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		fmt.Fprintln(a.stderr, err)
		return 1
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode >= 400 {
		data, _ := io.ReadAll(httpResp.Body)
		fmt.Fprintln(a.stderr, gatewayErrorLine(httpResp.Status, data))
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
