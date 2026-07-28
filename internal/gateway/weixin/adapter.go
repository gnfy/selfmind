package weixin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/gateway/api"
	"selfmind/internal/gateway/command"
	"selfmind/internal/gateway/delivery"
	"selfmind/internal/gateway/router"
	"selfmind/internal/platform/log"
	"selfmind/internal/tools"
)

type MessageHandler func(context.Context, api.MessageRequest) (api.MessageResponse, int)

type Adapter struct {
	cfg     RuntimeConfig
	client  *Client
	store   *control.Store
	handler MessageHandler

	clientMu                  sync.RWMutex
	credentialRefreshInterval time.Duration
	mu                        sync.Mutex
	seen                      map[string]time.Time
	cancel                    context.CancelFunc
	done                      chan struct{}
}

func NewAdapter(cfg RuntimeConfig, store *control.Store, handler MessageHandler) *Adapter {
	return &Adapter{
		cfg:                       cfg,
		client:                    NewClient(cfg),
		store:                     store,
		handler:                   handler,
		seen:                      map[string]time.Time{},
		done:                      make(chan struct{}),
		credentialRefreshInterval: 15 * time.Second,
	}
}

func (a *Adapter) Start(ctx context.Context) error {
	if a == nil {
		return nil
	}
	if !a.cfg.Enabled {
		return nil
	}
	if strings.TrimSpace(a.cfg.AccountID) == "" {
		return fmt.Errorf("gateway.weixin.account_id is required")
	}
	if strings.TrimSpace(a.cfg.Token) == "" {
		return fmt.Errorf("gateway.weixin.token is required")
	}
	if a.handler == nil {
		return fmt.Errorf("weixin message handler is required")
	}
	if a.cfg.OwnerPersonID != "" && len(a.cfg.AllowFrom) == 0 {
		log.Warn("weixin owner auto-binding is disabled until gateway.weixin.allow_from explicitly identifies the owner account")
	}
	a.clientSnapshot().RestoreContextTokens()
	pollCtx, cancel := context.WithCancel(ctx)
	a.cancel = cancel
	go a.pollLoop(pollCtx)
	log.Info("weixin gateway started", "account_id", safeID(a.cfg.AccountID), "base_url", a.cfg.BaseURL)
	return nil
}

func (a *Adapter) Stop() {
	if a == nil {
		return
	}
	if a.cancel != nil {
		a.cancel()
	}
	select {
	case <-a.done:
	case <-time.After(3 * time.Second):
	}
}

func (a *Adapter) Send(ctx context.Context, msg delivery.Message) error {
	_, err := a.SendWithReceipt(ctx, msg)
	return err
}

// SendWithReceipt implements delivery.SenderWithReceipt: the iLink API accepts
// proactive pushes on a stale context_token with ret=0 yet the message never
// reaches the phone (observed live 2026-07-04). Delivery confidence therefore
// comes from token freshness, not from the send response.
func (a *Adapter) SendWithReceipt(ctx context.Context, msg delivery.Message) (bool, error) {
	if a == nil {
		return false, delivery.ErrNoSender
	}
	client := a.clientSnapshot()
	if client == nil {
		return false, delivery.ErrNoSender
	}
	target := strings.TrimSpace(msg.Channel)
	// Proactive jobs historically stored the generic platform name in Channel
	// while PlatformUserID held the actual recipient. Sending to literal
	// "weixin" produces iLink ret=-3. Prefer the concrete recipient whenever
	// Channel is absent or is only a platform marker; interactive group/DM
	// channels remain authoritative when they contain a real chat id.
	if target == "" || strings.EqualFold(target, strings.TrimSpace(msg.Platform)) {
		target = strings.TrimSpace(msg.PlatformUserID)
	}
	if target == "" {
		return false, fmt.Errorf("weixin delivery target is empty")
	}
	confirmed := client.PushConfidence(target)
	if err := client.Send(ctx, target, msg.Content); err != nil {
		return confirmed, err
	}
	if !confirmed {
		log.Warn("weixin push accepted but unconfirmed (stale context_token); it may not arrive",
			"target", target, "kind", msg.Kind)
	}
	return confirmed, nil
}

func (a *Adapter) pollLoop(ctx context.Context) {
	defer close(a.done)
	syncBuf := a.loadSyncBuf()
	backoff := time.Second
	sessionExpiredLogged := false
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		client := a.clientSnapshot()
		resp, err := client.GetUpdates(ctx, syncBuf, 50*time.Second)
		if err != nil {
			if errors.Is(err, ErrSessionExpired) {
				if !sessionExpiredLogged {
					log.Error("weixin session expired - inbound messages are not being received; run `selfmind weixin login` to refresh the account", "error", tools.RedactSensitive(err.Error()))
					sessionExpiredLogged = true
				}
				if a.waitForCredentialRefresh(ctx, client.cfg.Token, time.Now()) {
					syncBuf = a.loadSyncBuf()
					backoff = time.Second
					sessionExpiredLogged = false
					log.Info("weixin credentials refreshed; polling resumed", "account_id", safeID(a.cfg.AccountID))
					continue
				}
				return
			}
			log.Warn("weixin getupdates failed", "error", tools.RedactSensitive(err.Error()))
			wait := backoff
			if wait > 30*time.Second {
				wait = 30 * time.Second
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		sessionExpiredLogged = false
		if next := firstNonEmpty(stringFromMap(resp, "get_updates_buf"), stringFromMap(resp, "sync_buf")); next != "" {
			syncBuf = next
			a.saveSyncBuf(syncBuf)
		}
		for _, msg := range extractMessages(resp) {
			if err := a.processMessage(ctx, msg); err != nil {
				log.Warn("weixin message processing failed", "error", tools.RedactSensitive(err.Error()))
			}
		}
		if len(extractMessages(resp)) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
}

func (a *Adapter) processMessage(ctx context.Context, raw map[string]interface{}) error {
	msg := normalizeRawMessage(raw)
	if msg == nil {
		return nil
	}
	msgID := messageID(msg)
	if msgID != "" && a.isDuplicate(ctx, msgID) {
		return nil
	}
	sender := senderID(msg, a.cfg.AccountID)
	chatID := chatID(msg, sender, a.cfg.AccountID)
	if sender == "" || chatID == "" {
		return nil
	}
	if sender == a.cfg.AccountID {
		return nil
	}
	client := a.clientSnapshot()
	isGroup := isGroupChat(msg, chatID)
	if !a.allowed(sender, chatID, isGroup) {
		log.Warn("weixin message ignored by policy", "sender", safeID(sender), "chat", safeID(chatID), "group", isGroup)
		return nil
	}
	if token := stringFromMap(msg, "context_token"); token != "" {
		client.SaveContextToken(chatID, token)
	}
	client.FetchTypingTicket(ctx, chatID, stringFromMap(msg, "context_token"))

	text := extractMessageText(msg)
	attachments := a.extractAttachments(ctx, msg)
	if strings.TrimSpace(text) == "" && len(attachments) == 0 {
		return nil
	}
	if strings.TrimSpace(text) == "" {
		text = summarizeAttachments(attachments)
	}
	displayName := firstNonEmpty(stringFromMap(msg, "sender_nick"), stringFromMap(msg, "display_name"), safeID(sender))
	tenantID := firstNonEmpty(a.cfg.DefaultTenantID, control.DefaultTenantID)
	if a.ownerBindingAllowed(sender, chatID, isGroup) && a.store != nil {
		if _, err := a.store.BindAccount(ctx, tenantID, a.cfg.OwnerPersonID, "weixin", sender, displayName); err != nil {
			return err
		}
	}
	_ = client.SendTyping(ctx, chatID, true)
	defer client.SendTyping(context.Background(), chatID, false)

	req := api.MessageRequest{
		TenantID:       tenantID,
		Platform:       "weixin",
		PlatformUserID: sender,
		DisplayName:    displayName,
		Channel:        chatID,
		Content:        text,
		Async:          !isControlCommand(text),
		Attachments:    attachments,
	}
	resp, status := a.handler(ctx, req)
	if status >= http.StatusBadRequest || strings.TrimSpace(resp.Error) != "" {
		errText := firstNonEmpty(resp.Error, fmt.Sprintf("weixin request failed: HTTP %d", status))
		_ = client.Send(context.Background(), chatID, "SelfMind error: "+errText)
		return nil
	}
	if strings.TrimSpace(resp.Content) != "" {
		return client.Send(context.Background(), chatID, resp.Content)
	}
	if resp.Accepted {
		return client.Send(context.Background(), chatID, router.WorkingNotice("weixin"))
	}
	return nil
}

// ownerBindingAllowed is deliberately stricter than message admission. Open
// DMs may use the bot as separate identities, but only an explicitly
// allowlisted account may inherit the configured owner's person-level grants.
func (a *Adapter) ownerBindingAllowed(sender, chat string, isGroup bool) bool {
	if a == nil || strings.TrimSpace(a.cfg.OwnerPersonID) == "" {
		return false
	}
	if isGroup {
		return stringInList(chat, a.cfg.GroupAllowFrom) || stringInList(sender, a.cfg.GroupAllowFrom)
	}
	return stringInList(sender, a.cfg.AllowFrom) || stringInList(chat, a.cfg.AllowFrom)
}

func (a *Adapter) extractAttachments(ctx context.Context, msg map[string]interface{}) []api.MessageAttachment {
	client := a.clientSnapshot()
	items := interfaceSlice(msg["item_list"])
	if len(items) == 0 {
		items = interfaceSlice(msg["items"])
	}
	var out []api.MessageAttachment
	for _, itemValue := range items {
		item, _ := itemValue.(map[string]interface{})
		if item == nil {
			continue
		}
		if intFromMap(item, "type") == itemText {
			continue
		}
		att, err := client.DownloadAttachment(ctx, item, a.cfg.DataDir)
		if err != nil {
			log.Warn("weixin media download failed", "error", tools.RedactSensitive(err.Error()))
			continue
		}
		if att != nil && att.Path != "" {
			out = append(out, *att)
		}
	}
	return out
}

func (a *Adapter) clientSnapshot() *Client {
	if a == nil {
		return nil
	}
	a.clientMu.RLock()
	defer a.clientMu.RUnlock()
	return a.client
}

func (a *Adapter) replaceClient(client *Client) {
	a.clientMu.Lock()
	a.client = client
	a.clientMu.Unlock()
}

func (a *Adapter) waitForCredentialRefresh(ctx context.Context, expiredToken string, expiredAt time.Time) bool {
	interval := a.credentialRefreshInterval
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		cred, err := LoadCredentials(a.cfg.HomeDir, a.cfg.AccountID)
		if err == nil && credentialRefreshesSession(cred, expiredToken, expiredAt) {
			cfg := a.cfg
			cfg.Token = strings.TrimSpace(cred.Token)
			if baseURL := strings.TrimSpace(cred.BaseURL); baseURL != "" {
				cfg.BaseURL = strings.TrimRight(baseURL, "/")
			}
			client := NewClient(cfg)
			client.RestoreContextTokens()
			a.replaceClient(client)
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func credentialRefreshesSession(cred *Credentials, expiredToken string, expiredAt time.Time) bool {
	if cred == nil || strings.TrimSpace(cred.Token) == "" {
		return false
	}
	if strings.TrimSpace(cred.Token) != strings.TrimSpace(expiredToken) {
		return true
	}
	savedAt, err := time.Parse(time.RFC3339, strings.TrimSpace(cred.SavedAt))
	return err == nil && savedAt.After(expiredAt)
}

func (a *Adapter) allowed(sender, chat string, isGroup bool) bool {
	if isGroup {
		switch strings.ToLower(firstNonEmpty(a.cfg.GroupPolicy, "disabled")) {
		case "disabled", "off", "deny":
			return false
		case "allowlist", "allow_list":
			return stringInList(chat, a.cfg.GroupAllowFrom) || stringInList(sender, a.cfg.GroupAllowFrom)
		default:
			return true
		}
	}
	switch strings.ToLower(firstNonEmpty(a.cfg.DMPolicy, "open")) {
	case "disabled", "off", "deny":
		return false
	case "allowlist", "allow_list":
		return stringInList(sender, a.cfg.AllowFrom) || stringInList(chat, a.cfg.AllowFrom)
	default:
		return len(a.cfg.AllowFrom) == 0 || stringInList(sender, a.cfg.AllowFrom) || stringInList(chat, a.cfg.AllowFrom)
	}
}

func (a *Adapter) isDuplicate(ctx context.Context, id string) bool {
	a.mu.Lock()
	now := time.Now()
	for key, at := range a.seen {
		if now.Sub(at) > 24*time.Hour {
			delete(a.seen, key)
		}
	}
	_, dup := a.seen[id]
	if !dup {
		a.seen[id] = now
	}
	a.mu.Unlock()
	if dup {
		return true
	}
	// The in-memory map dies with the process while the iLink sync buffer
	// replays recent messages on reconnect, so a restart used to re-run the
	// agent on already-processed messages. The durable first-seen check in
	// control.db is what closes that window; a store error fails open so a
	// dedup hiccup never drops a real message.
	if a.store != nil {
		if first, err := a.store.MarkInboundSeen(ctx, "weixin", id); err == nil && !first {
			return true
		}
	}
	return false
}

func (a *Adapter) syncBufPath() string {
	return SyncBufFilePath(a.cfg.HomeDir, a.cfg.AccountID)
}

func (a *Adapter) loadSyncBuf() string {
	data, err := os.ReadFile(a.syncBufPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func (a *Adapter) saveSyncBuf(value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	path := a.syncBufPath()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return
	}
	_ = os.WriteFile(path, []byte(value), 0600)
}

func extractMessages(resp map[string]interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	for _, key := range []string{"msgs", "messages", "msg_list", "message_list"} {
		for _, item := range interfaceSlice(resp[key]) {
			if m, ok := item.(map[string]interface{}); ok {
				out = append(out, m)
			}
		}
	}
	if nested := nestedMap(resp, "data"); nested != nil {
		out = append(out, extractMessages(nested)...)
	}
	return out
}

func normalizeRawMessage(raw map[string]interface{}) map[string]interface{} {
	if raw == nil {
		return nil
	}
	if msg := nestedMap(raw, "msg"); msg != nil {
		return msg
	}
	if msg := nestedMap(raw, "message"); msg != nil {
		return msg
	}
	return raw
}

func messageID(msg map[string]interface{}) string {
	return firstNonEmpty(
		stringFromMap(msg, "msgid"),
		stringFromMap(msg, "msg_id"),
		stringFromMap(msg, "message_id"),
		stringFromMap(msg, "client_id"),
	)
}

func senderID(msg map[string]interface{}, accountID string) string {
	from := firstNonEmpty(stringFromMap(msg, "from_user_id"), stringFromMap(msg, "sender"), stringFromMap(msg, "sender_id"))
	if from != "" && from != accountID {
		return from
	}
	return firstNonEmpty(stringFromMap(msg, "actual_user_id"), stringFromMap(msg, "talker"), from)
}

func chatID(msg map[string]interface{}, sender, accountID string) string {
	room := firstNonEmpty(stringFromMap(msg, "room_id"), stringFromMap(msg, "chat_room_id"), stringFromMap(msg, "chat_id"))
	if room != "" {
		return room
	}
	to := stringFromMap(msg, "to_user_id")
	if to != "" && to != accountID {
		return to
	}
	return sender
}

func isGroupChat(msg map[string]interface{}, chatID string) bool {
	if strings.Contains(chatID, "@chatroom") {
		return true
	}
	if stringFromMap(msg, "room_id") != "" || stringFromMap(msg, "chat_room_id") != "" {
		return true
	}
	return strings.EqualFold(stringFromMap(msg, "chat_type"), "group")
}

func extractMessageText(msg map[string]interface{}) string {
	var parts []string
	if text := firstNonEmpty(stringFromMap(msg, "content"), stringFromMap(msg, "text")); text != "" {
		parts = append(parts, text)
	}
	items := interfaceSlice(msg["item_list"])
	if len(items) == 0 {
		items = interfaceSlice(msg["items"])
	}
	for _, itemValue := range items {
		item, _ := itemValue.(map[string]interface{})
		if item == nil {
			continue
		}
		switch intFromMap(item, "type") {
		case itemText:
			textItem := nestedMap(item, "text_item")
			if text := firstNonEmpty(stringFromMap(textItem, "text"), stringFromMap(item, "text")); text != "" {
				parts = append(parts, text)
			}
		case itemVoice:
			voice := nestedMap(item, "voice_item")
			if text := firstNonEmpty(stringFromMap(voice, "voice_text"), stringFromMap(voice, "transcript"), stringFromMap(voice, "text")); text != "" {
				parts = append(parts, "[voice transcript]\n"+text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(uniqueStrings(parts), "\n"))
}

func summarizeAttachments(attachments []api.MessageAttachment) string {
	if len(attachments) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Received attachments:")
	for _, att := range attachments {
		b.WriteString("\n- ")
		b.WriteString(firstNonEmpty(att.Kind, "file"))
		if att.Name != "" {
			b.WriteString(" ")
			b.WriteString(att.Name)
		}
		if att.Path != "" {
			b.WriteString(" ")
			b.WriteString(att.Path)
		}
	}
	return b.String()
}

// isControlCommand is the Weixin async-hint: a gateway control command must be
// handled synchronously (the switch returns an inline reply) rather than
// dispatched as an async task with a working notice. It delegates to the shared
// registry so it can never again omit a gateway command.
func isControlCommand(content string) bool {
	return command.IsGatewayControl(content)
}

func interfaceSlice(value interface{}) []interface{} {
	switch v := value.(type) {
	case []interface{}:
		return v
	case []map[string]interface{}:
		out := make([]interface{}, 0, len(v))
		for _, item := range v {
			out = append(out, item)
		}
		return out
	default:
		return nil
	}
}

func stringInList(value string, list []string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	for _, item := range list {
		if strings.EqualFold(strings.TrimSpace(item), value) {
			return true
		}
	}
	return false
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
