package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"selfmind/internal/gateway/api"
)

func (d *Server) handleIMWebhook(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	platform := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/im/"), "/")
	if platform == "" {
		platform = "webhook"
	}

	var payload map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}

	if challenge := mapString(payload, "challenge"); challenge != "" {
		writeJSON(w, http.StatusOK, map[string]string{"challenge": challenge})
		return
	}

	req := messageRequestFromIM(platform, payload)
	if boolFromMap(payload, "async") || (os.Getenv("SELF_IM_ASYNC") == "1" && !isControlCommand(req.Content)) {
		req.Async = true
	}
	resp, status := d.ProcessMessage(r.Context(), req)
	writeJSON(w, status, resp)
}

func (d *Server) handleAccountBind(w http.ResponseWriter, r *http.Request) {
	if !d.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req api.BindAccountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.PersonID) == "" {
		http.Error(w, "person_id is required", http.StatusBadRequest)
		return
	}
	identity, err := d.Control.BindAccount(
		r.Context(),
		d.tenantID(req.TenantID),
		req.PersonID,
		req.Platform,
		req.PlatformUserID,
		req.DisplayName,
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"identity": identity})
}

func messageRequestFromIM(platform string, payload map[string]interface{}) api.MessageRequest {
	req := api.MessageRequest{
		TenantID:       mapString(payload, "tenant_id"),
		Platform:       fallback(mapString(payload, "platform"), platform),
		PlatformUserID: firstNonEmpty(mapString(payload, "platform_user_id"), mapString(payload, "user_id"), mapString(payload, "open_id"), mapString(payload, "sender_id"), "local"),
		DisplayName:    firstNonEmpty(mapString(payload, "display_name"), mapString(payload, "user_name"), mapString(payload, "name")),
		Channel:        firstNonEmpty(mapString(payload, "channel"), mapString(payload, "chat_id"), mapString(payload, "conversation_id"), platform),
		Content:        firstNonEmpty(mapString(payload, "content"), mapString(payload, "text"), mapString(payload, "message")),
		WorkspaceID:    mapString(payload, "workspace_id"),
		TaskID:         mapString(payload, "task_id"),
		Async:          boolFromMap(payload, "async"),
	}

	if event := nestedMap(payload, "event"); event != nil {
		if sender := nestedMap(event, "sender"); sender != nil {
			if senderID := nestedMap(sender, "sender_id"); senderID != nil {
				req.PlatformUserID = firstNonEmpty(
					mapString(senderID, "union_id"),
					mapString(senderID, "user_id"),
					mapString(senderID, "open_id"),
					req.PlatformUserID,
				)
			}
			req.DisplayName = firstNonEmpty(mapString(sender, "sender_type"), req.DisplayName)
		}
		if msg := nestedMap(event, "message"); msg != nil {
			req.Channel = firstNonEmpty(mapString(msg, "chat_id"), mapString(msg, "root_id"), req.Channel)
			req.Content = firstNonEmpty(contentText(msg["content"]), mapString(msg, "text"), req.Content)
		}
	}

	if msg := nestedMap(payload, "message"); msg != nil {
		req.Content = firstNonEmpty(contentText(msg["content"]), mapString(msg, "content"), mapString(msg, "text"), req.Content)
		req.Channel = firstNonEmpty(mapString(msg, "chat_id"), mapString(msg, "group_id"), mapString(msg, "channel_id"), req.Channel)
	}
	if author := nestedMap(payload, "author"); author != nil {
		req.PlatformUserID = firstNonEmpty(mapString(author, "id"), mapString(author, "user_id"), req.PlatformUserID)
		req.DisplayName = firstNonEmpty(mapString(author, "username"), mapString(author, "name"), req.DisplayName)
	}

	req.PlatformUserID = firstNonEmpty(mapString(payload, "FromUserName"), req.PlatformUserID)
	req.Channel = firstNonEmpty(mapString(payload, "ToUserName"), req.Channel)
	req.Content = firstNonEmpty(mapString(payload, "Content"), req.Content)
	req.Attachments = attachmentsFromIM(payload)
	return req
}

func attachmentsFromIM(payload map[string]interface{}) []api.MessageAttachment {
	var out []api.MessageAttachment
	if value, ok := payload["attachments"]; ok {
		out = append(out, attachmentsFromValue(value)...)
	}
	if value, ok := payload["media_paths"]; ok {
		out = append(out, attachmentsFromValue(value)...)
	}
	return out
}

func attachmentsFromValue(value interface{}) []api.MessageAttachment {
	var out []api.MessageAttachment
	switch v := value.(type) {
	case []interface{}:
		for _, item := range v {
			out = append(out, attachmentsFromValue(item)...)
		}
	case []string:
		for _, item := range v {
			if item = strings.TrimSpace(item); item != "" {
				out = append(out, api.MessageAttachment{Path: item})
			}
		}
	case string:
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			out = append(out, api.MessageAttachment{Path: trimmed})
		}
	case map[string]interface{}:
		att := api.MessageAttachment{
			Kind:     mapString(v, "kind"),
			Path:     firstNonEmpty(mapString(v, "path"), mapString(v, "url")),
			MimeType: firstNonEmpty(mapString(v, "mime_type"), mapString(v, "mime")),
			Name:     mapString(v, "name"),
		}
		if size := mapString(v, "size"); size != "" {
			fmt.Sscanf(size, "%d", &att.Size)
		}
		if att.Path != "" || att.Name != "" {
			out = append(out, att)
		}
	}
	return out
}

func contentText(value interface{}) string {
	switch v := value.(type) {
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return ""
		}
		var decoded map[string]interface{}
		if strings.HasPrefix(trimmed, "{") && json.Unmarshal([]byte(trimmed), &decoded) == nil {
			return firstNonEmpty(mapString(decoded, "text"), mapString(decoded, "content"))
		}
		return trimmed
	case map[string]interface{}:
		return firstNonEmpty(mapString(v, "text"), mapString(v, "content"))
	default:
		return ""
	}
}

func nestedMap(payload map[string]interface{}, keys ...string) map[string]interface{} {
	current := payload
	for _, key := range keys {
		next, ok := current[key].(map[string]interface{})
		if !ok {
			return nil
		}
		current = next
	}
	return current
}

func mapString(payload map[string]interface{}, key string) string {
	if payload == nil {
		return ""
	}
	value, ok := payload[key]
	if !ok || value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func boolFromMap(payload map[string]interface{}, key string) bool {
	if payload == nil {
		return false
	}
	value, ok := payload[key]
	if !ok || value == nil {
		return false
	}
	switch v := value.(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		default:
			return false
		}
	default:
		return strings.EqualFold(strings.TrimSpace(fmt.Sprint(v)), "true")
	}
}

func isControlCommand(content string) bool {
	lower := strings.ToLower(strings.TrimSpace(content))
	if lower == "" {
		return false
	}
	switch {
	case lower == "/id" || lower == "id":
		return true
	case lower == "/stop" || lower == "stop":
		return true
	case lower == "/tasks" || lower == "tasks":
		return true
	case lower == "/workspaces" || lower == "workspaces":
		return true
	case lower == "/events" || lower == "events":
		return true
	case lower == "/approvals" || lower == "approvals":
		return true
	case lower == "/status" || lower == "status" || lower == "/task status" || lower == "task status":
		return true
	case strings.HasPrefix(lower, "/new"):
		return true
	case strings.HasPrefix(lower, "/resume ") || strings.HasPrefix(lower, "/task ") || strings.HasPrefix(lower, "/workspace "):
		return true
	case strings.HasPrefix(lower, "/approve ") || strings.HasPrefix(lower, "approve ") || strings.HasPrefix(lower, "/reject ") || strings.HasPrefix(lower, "reject "):
		return true
	default:
		return false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
