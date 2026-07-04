package weixin

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/skip2/go-qrcode"

	"selfmind/internal/gateway/api"
	"selfmind/internal/platform/config"
	"selfmind/internal/tools"
)

const (
	DefaultBaseURL    = "https://ilinkai.weixin.qq.com"
	DefaultCDNBaseURL = "https://novac2c.cdn.weixin.qq.com/c2c"

	ilinkAppID            = "bot"
	channelVersion        = "2.2.0"
	ilinkAppClientVersion = (2 << 16) | (2 << 8)

	epGetUpdates  = "ilink/bot/getupdates"
	epSendMessage = "ilink/bot/sendmessage"
	epSendTyping  = "ilink/bot/sendtyping"
	epGetConfig   = "ilink/bot/getconfig"
	epGetUpload   = "ilink/bot/getuploadurl"
	epGetQR       = "ilink/bot/get_bot_qrcode"
	epQRStatus    = "ilink/bot/get_qrcode_status"

	itemText  = 1
	itemImage = 2
	itemVoice = 3
	itemFile  = 4
	itemVideo = 5

	mediaImage = 1
	mediaVideo = 2
	mediaFile  = 3
	mediaVoice = 4

	msgTypeBot     = 2
	msgStateFinish = 2

	typingStart = 1
	typingStop  = 2

	sessionExpiredCode = -14
	rateLimitCode      = -2
)

type RuntimeConfig struct {
	Enabled                bool
	OwnerPersonID          string
	AccountID              string
	Token                  string
	BaseURL                string
	CDNBaseURL             string
	DMPolicy               string
	GroupPolicy            string
	AllowFrom              []string
	GroupAllowFrom         []string
	SplitMultilineMessages bool
	SendChunkDelay         time.Duration
	SendChunkRetries       int
	DataDir                string
	HomeDir                string
	DefaultTenantID        string
}

type Credentials struct {
	AccountID string `json:"account_id"`
	Token     string `json:"token"`
	BaseURL   string `json:"base_url"`
	UserID    string `json:"user_id,omitempty"`
	SavedAt   string `json:"saved_at,omitempty"`
}

func RuntimeConfigFrom(cfg config.WeixinConfig, dataDir, tenantID string) RuntimeConfig {
	baseURL := firstNonEmpty(cfg.BaseURL, DefaultBaseURL)
	cdnBaseURL := firstNonEmpty(cfg.CDNBaseURL, DefaultCDNBaseURL)
	retries := cfg.SendChunkRetries
	if retries <= 0 {
		retries = 4
	}
	delay := time.Duration(cfg.SendChunkDelaySeconds * float64(time.Second))
	if delay <= 0 {
		delay = 1500 * time.Millisecond
	}
	return RuntimeConfig{
		Enabled:                cfg.Enabled,
		OwnerPersonID:          strings.TrimSpace(cfg.OwnerPersonID),
		AccountID:              strings.TrimSpace(cfg.AccountID),
		Token:                  strings.TrimSpace(cfg.Token),
		BaseURL:                strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		CDNBaseURL:             strings.TrimRight(strings.TrimSpace(cdnBaseURL), "/"),
		DMPolicy:               strings.TrimSpace(cfg.DMPolicy),
		GroupPolicy:            strings.TrimSpace(cfg.GroupPolicy),
		AllowFrom:              append([]string(nil), cfg.AllowFrom...),
		GroupAllowFrom:         append([]string(nil), cfg.GroupAllowFrom...),
		SplitMultilineMessages: cfg.SplitMultilineMessages,
		SendChunkDelay:         delay,
		SendChunkRetries:       retries,
		DataDir:                dataDir,
		HomeDir:                defaultSelfMindHome(),
		DefaultTenantID:        firstNonEmpty(tenantID, "default"),
	}
}

type Client struct {
	cfg        RuntimeConfig
	httpClient *http.Client
	tokens     *ContextTokenStore
	typing     *typingTicketCache
}

func NewClient(cfg RuntimeConfig) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.CDNBaseURL == "" {
		cfg.CDNBaseURL = DefaultCDNBaseURL
	}
	if cfg.HomeDir == "" {
		cfg.HomeDir = defaultSelfMindHome()
	}
	if cfg.SendChunkRetries <= 0 {
		cfg.SendChunkRetries = 4
	}
	if cfg.SendChunkDelay <= 0 {
		cfg.SendChunkDelay = 1500 * time.Millisecond
	}
	client := &http.Client{
		Timeout: 45 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
		},
	}
	return &Client{
		cfg:        cfg,
		httpClient: client,
		tokens:     NewContextTokenStore(cfg.HomeDir),
		typing:     newTypingTicketCache(10 * time.Minute),
	}
}

func (c *Client) Config() RuntimeConfig {
	return c.cfg
}

func (c *Client) RestoreContextTokens() {
	if c == nil || c.tokens == nil || c.cfg.AccountID == "" {
		return
	}
	c.tokens.Restore(c.cfg.AccountID)
}

func (c *Client) SaveContextToken(peerID, token string) {
	if c == nil || c.tokens == nil || c.cfg.AccountID == "" || strings.TrimSpace(peerID) == "" || strings.TrimSpace(token) == "" {
		return
	}
	c.tokens.Set(c.cfg.AccountID, peerID, token)
}

func (c *Client) GetUpdates(ctx context.Context, syncBuf string, timeout time.Duration) (map[string]interface{}, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.apiPost(ctx, epGetUpdates, map[string]interface{}{"get_updates_buf": syncBuf}, c.cfg.Token)
}

func (c *Client) SendTyping(ctx context.Context, chatID string, start bool) error {
	ticket := c.typing.Get(chatID)
	if ticket == "" {
		return nil
	}
	status := typingStop
	if start {
		status = typingStart
	}
	_, err := c.apiPost(ctx, epSendTyping, map[string]interface{}{
		"ilink_user_id": chatID,
		"typing_ticket": ticket,
		"status":        status,
	}, c.cfg.Token)
	return err
}

func (c *Client) FetchTypingTicket(ctx context.Context, chatID, contextToken string) {
	if c == nil || c.typing.Get(chatID) != "" {
		return
	}
	payload := map[string]interface{}{"ilink_user_id": chatID}
	if contextToken != "" {
		payload["context_token"] = contextToken
	}
	resp, err := c.apiPost(ctx, epGetConfig, payload, c.cfg.Token)
	if err != nil {
		return
	}
	if ticket := stringFromMap(resp, "typing_ticket"); ticket != "" {
		c.typing.Set(chatID, ticket)
	}
}

func (c *Client) Send(ctx context.Context, chatID, content string) error {
	if c == nil {
		return fmt.Errorf("weixin client is nil")
	}
	if strings.TrimSpace(c.cfg.Token) == "" {
		return fmt.Errorf("weixin token is required")
	}
	if strings.TrimSpace(c.cfg.AccountID) == "" {
		return fmt.Errorf("weixin account_id is required")
	}
	chatID = strings.TrimSpace(chatID)
	if chatID == "" {
		return fmt.Errorf("weixin chat id is required")
	}

	mediaPaths, cleaned := extractMediaPaths(content)
	text := strings.TrimSpace(cleaned)
	if text != "" {
		chunks := splitTextForDelivery(formatForWeixin(text), 2000, c.cfg.SplitMultilineMessages)
		for i, chunk := range chunks {
			if strings.TrimSpace(chunk) == "" {
				continue
			}
			if err := c.sendTextChunk(ctx, chatID, chunk); err != nil {
				return err
			}
			if i < len(chunks)-1 && c.cfg.SendChunkDelay > 0 {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(c.cfg.SendChunkDelay):
				}
			}
		}
	}
	for _, path := range mediaPaths {
		if err := c.SendFile(ctx, chatID, path, ""); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) sendTextChunk(ctx context.Context, chatID, text string) error {
	var lastErr error
	contextToken := c.tokens.Get(c.cfg.AccountID, chatID)
	retriedWithoutToken := false
	for attempt := 0; attempt <= c.cfg.SendChunkRetries; attempt++ {
		clientID := "selfmind-weixin-" + uuid.NewString()
		resp, err := c.sendTextOnce(ctx, chatID, text, contextToken, clientID)
		if err == nil {
			ret, errcode := intFromMap(resp, "ret"), intFromMap(resp, "errcode")
			if isOKCode(ret) && isOKCode(errcode) {
				return nil
			}
			if isSessionExpired(ret, errcode, stringFromMap(resp, "errmsg")) && contextToken != "" && !retriedWithoutToken {
				retriedWithoutToken = true
				contextToken = ""
				c.tokens.Delete(c.cfg.AccountID, chatID)
				continue
			}
			err = fmt.Errorf("iLink sendmessage error: ret=%d errcode=%d errmsg=%s", ret, errcode, stringFromMap(resp, "errmsg"))
			if ret == rateLimitCode || errcode == rateLimitCode {
				lastErr = err
			} else {
				return err
			}
		}
		if err != nil {
			lastErr = err
		}
		if attempt >= c.cfg.SendChunkRetries {
			break
		}
		wait := time.Duration(attempt+1) * time.Second
		if lastErr != nil && strings.Contains(lastErr.Error(), fmt.Sprintf("errcode=%d", rateLimitCode)) {
			wait *= 3
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("weixin send failed")
	}
	return lastErr
}

func (c *Client) sendTextOnce(ctx context.Context, chatID, text, contextToken, clientID string) (map[string]interface{}, error) {
	msg := map[string]interface{}{
		"from_user_id":  "",
		"to_user_id":    chatID,
		"client_id":     clientID,
		"message_type":  msgTypeBot,
		"message_state": msgStateFinish,
		"item_list": []interface{}{
			map[string]interface{}{
				"type": itemText,
				"text_item": map[string]interface{}{
					"text": text,
				},
			},
		},
	}
	if contextToken != "" {
		msg["context_token"] = contextToken
	}
	return c.apiPost(ctx, epSendMessage, map[string]interface{}{"msg": msg}, c.cfg.Token)
}

func (c *Client) SendFile(ctx context.Context, chatID, path, caption string) error {
	path = strings.TrimSpace(strings.TrimPrefix(path, "file://"))
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	mediaType, itemBuilder := outboundMediaBuilder(path, false)
	fileKey := randomHex(16)
	aesKey := randomBytes(16)
	rawMD5 := md5.Sum(data)
	uploadResp, err := c.apiPost(ctx, epGetUpload, map[string]interface{}{
		"filekey":       fileKey,
		"media_type":    mediaType,
		"to_user_id":    chatID,
		"rawsize":       len(data),
		"rawfilemd5":    hex.EncodeToString(rawMD5[:]),
		"filesize":      aesPaddedSize(len(data)),
		"no_need_thumb": true,
		"aeskey":        hex.EncodeToString(aesKey),
	}, c.cfg.Token)
	if err != nil {
		return err
	}
	uploadURL := stringFromMap(uploadResp, "upload_full_url")
	if uploadURL == "" {
		uploadParam := stringFromMap(uploadResp, "upload_param")
		if uploadParam == "" {
			return fmt.Errorf("getuploadurl returned no upload URL")
		}
		uploadURL = c.cdnUploadURL(uploadParam, fileKey)
	}
	ciphertext, err := aes128ECBEncrypt(data, aesKey)
	if err != nil {
		return err
	}
	encryptedParam, err := c.uploadCiphertext(ctx, uploadURL, ciphertext)
	if err != nil {
		return err
	}
	if strings.TrimSpace(caption) != "" {
		if err := c.sendTextChunk(ctx, chatID, caption); err != nil {
			return err
		}
	}
	contextToken := c.tokens.Get(c.cfg.AccountID, chatID)
	apiAESKey := base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(aesKey)))
	item := itemBuilder(outboundMediaArgs{
		EncryptedQueryParam: encryptedParam,
		AESKeyForAPI:        apiAESKey,
		CiphertextSize:      len(ciphertext),
		PlaintextSize:       len(data),
		Filename:            filepath.Base(path),
		RawFileMD5:          hex.EncodeToString(rawMD5[:]),
	})
	msg := map[string]interface{}{
		"from_user_id":  "",
		"to_user_id":    chatID,
		"client_id":     "selfmind-weixin-" + uuid.NewString(),
		"message_type":  msgTypeBot,
		"message_state": msgStateFinish,
		"item_list":     []interface{}{item},
	}
	if contextToken != "" {
		msg["context_token"] = contextToken
	}
	resp, err := c.apiPost(ctx, epSendMessage, map[string]interface{}{"msg": msg}, c.cfg.Token)
	if err != nil {
		return err
	}
	ret, errcode := intFromMap(resp, "ret"), intFromMap(resp, "errcode")
	if !isOKCode(ret) || !isOKCode(errcode) {
		return fmt.Errorf("iLink media send error: ret=%d errcode=%d errmsg=%s", ret, errcode, stringFromMap(resp, "errmsg"))
	}
	return nil
}

func (c *Client) DownloadAttachment(ctx context.Context, item map[string]interface{}, dataDir string) (*api.MessageAttachment, error) {
	kind, ref := mediaReference(item)
	if kind == "" || ref == nil {
		return nil, nil
	}
	raw, err := c.downloadAndDecryptMedia(ctx, ref)
	if err != nil {
		return nil, err
	}
	filename := firstNonEmpty(ref.Filename, kind+"-"+uuid.NewString()+ref.Ext)
	if filepath.Ext(filename) == "" && ref.Ext != "" {
		filename += ref.Ext
	}
	path, err := saveMedia(dataDir, kind, filename, raw)
	if err != nil {
		return nil, err
	}
	return &api.MessageAttachment{
		Kind:     kind,
		Path:     path,
		MimeType: mimeTypeForPath(path, ref.MimeType),
		Name:     filename,
		Size:     int64(len(raw)),
	}, nil
}

func (c *Client) downloadAndDecryptMedia(ctx context.Context, ref *mediaRef) ([]byte, error) {
	downloadURL := ref.FullURL
	if downloadURL == "" {
		if ref.EncryptedQueryParam == "" {
			return nil, fmt.Errorf("missing media download URL")
		}
		downloadURL = c.cdnDownloadURL(ref.EncryptedQueryParam)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 100<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("media download failed: %s", resp.Status)
	}
	if ref.AESKey == "" {
		return body, nil
	}
	key, err := parseAESKey(ref.AESKey)
	if err != nil {
		return nil, err
	}
	return aes128ECBDecrypt(body, key)
}

func (c *Client) apiGet(ctx context.Context, endpoint string) (map[string]interface{}, error) {
	u := strings.TrimRight(c.cfg.BaseURL, "/") + "/" + strings.TrimLeft(endpoint, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("iLink-App-Id", ilinkAppID)
	req.Header.Set("iLink-App-ClientVersion", fmt.Sprint(ilinkAppClientVersion))
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("iLink GET %s failed: %s %s", endpoint, resp.Status, tools.RedactSensitive(string(data)))
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) apiPost(ctx context.Context, endpoint string, payload map[string]interface{}, token string) (map[string]interface{}, error) {
	if payload == nil {
		payload = map[string]interface{}{}
	}
	payload["base_info"] = map[string]interface{}{"channel_version": channelVersion}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	u := strings.TrimRight(c.cfg.BaseURL, "/") + "/" + strings.TrimLeft(endpoint, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for key, value := range ilinkHeaders(token, body) {
		req.Header.Set(key, value)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("iLink POST %s failed: %s %s", endpoint, resp.Status, tools.RedactSensitive(string(data)))
	}
	var out map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Client) uploadCiphertext(ctx context.Context, uploadURL string, ciphertext []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(ciphertext))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("weixin CDN upload failed: %s %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if encrypted := strings.TrimSpace(resp.Header.Get("x-encrypted-param")); encrypted != "" {
		_, _ = io.Copy(io.Discard, resp.Body)
		return encrypted, nil
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return "", fmt.Errorf("weixin CDN upload missing x-encrypted-param: %s", strings.TrimSpace(string(data)))
}

func (c *Client) cdnDownloadURL(encryptedQueryParam string) string {
	return strings.TrimRight(c.cfg.CDNBaseURL, "/") + "/download?encrypted_query_param=" + url.QueryEscape(encryptedQueryParam)
}

func (c *Client) cdnUploadURL(uploadParam, fileKey string) string {
	return strings.TrimRight(c.cfg.CDNBaseURL, "/") + "/upload?encrypted_query_param=" + url.QueryEscape(uploadParam) + "&filekey=" + url.QueryEscape(fileKey)
}

func QRLogin(ctx context.Context, homeDir string, out io.Writer, timeout time.Duration) (*Credentials, error) {
	if homeDir == "" {
		homeDir = defaultSelfMindHome()
	}
	if timeout <= 0 {
		timeout = 8 * time.Minute
	}
	client := NewClient(RuntimeConfig{BaseURL: DefaultBaseURL, CDNBaseURL: DefaultCDNBaseURL, HomeDir: homeDir})
	resp, err := client.apiGet(ctx, epGetQR+"?bot_type=3")
	if err != nil {
		return nil, err
	}
	qrcodeValue := stringFromMap(resp, "qrcode")
	qrcodeURL := firstNonEmpty(stringFromMap(resp, "qrcode_img_content"), qrcodeValue)
	if qrcodeValue == "" {
		return nil, fmt.Errorf("QR response missing qrcode")
	}
	fmt.Fprintln(out, "Use WeChat to scan this QR code and confirm login.")
	if qrcodeURL != "" {
		if qr, err := qrcode.New(qrcodeURL, qrcode.Medium); err == nil {
			fmt.Fprintln(out, qr.ToSmallString(false))
		}
		fmt.Fprintln(out, qrcodeURL)
	}

	deadline := time.Now().Add(timeout)
	currentBase := DefaultBaseURL
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
		client.cfg.BaseURL = currentBase
		status, err := client.apiGet(ctx, epQRStatus+"?qrcode="+url.QueryEscape(qrcodeValue))
		if err != nil {
			fmt.Fprint(out, ".")
			continue
		}
		switch strings.ToLower(stringFromMap(status, "status")) {
		case "wait":
			fmt.Fprint(out, ".")
		case "scaned", "scanned":
			fmt.Fprintln(out, "\nScanned. Confirm in WeChat...")
		case "scaned_but_redirect":
			if host := stringFromMap(status, "redirect_host"); host != "" {
				currentBase = "https://" + host
			}
		case "expired":
			return nil, fmt.Errorf("QR code expired; rerun selfmind weixin login")
		case "confirmed":
			cred := &Credentials{
				AccountID: stringFromMap(status, "ilink_bot_id"),
				Token:     stringFromMap(status, "bot_token"),
				BaseURL:   firstNonEmpty(stringFromMap(status, "baseurl"), currentBase, DefaultBaseURL),
				UserID:    stringFromMap(status, "ilink_user_id"),
				SavedAt:   time.Now().UTC().Format(time.RFC3339),
			}
			if cred.AccountID == "" || cred.Token == "" {
				return nil, fmt.Errorf("QR confirmed but credentials were incomplete")
			}
			if err := SaveCredentials(homeDir, cred); err != nil {
				return nil, err
			}
			fmt.Fprintln(out, "\nWeixin connected: account_id="+safeID(cred.AccountID))
			return cred, nil
		}
	}
	return nil, fmt.Errorf("weixin QR login timed out")
}

func SaveCredentials(homeDir string, cred *Credentials) error {
	if cred == nil {
		return fmt.Errorf("credentials are nil")
	}
	if strings.TrimSpace(cred.AccountID) == "" {
		return fmt.Errorf("account_id is required")
	}
	if cred.SavedAt == "" {
		cred.SavedAt = time.Now().UTC().Format(time.RFC3339)
	}
	path := accountFile(homeDir, cred.AccountID)
	data, err := json.MarshalIndent(cred, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func LoadCredentials(homeDir, accountID string) (*Credentials, error) {
	path := accountFile(homeDir, accountID)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cred Credentials
	if err := json.Unmarshal(data, &cred); err != nil {
		return nil, err
	}
	return &cred, nil
}

func AccountFilePath(homeDir, accountID string) string {
	return accountFile(homeDir, accountID)
}

func SyncBufFilePath(homeDir, accountID string) string {
	return filepath.Join(accountDir(homeDir), safeFileName(accountID)+".syncbuf")
}

func accountFile(homeDir, accountID string) string {
	return filepath.Join(accountDir(homeDir), safeFileName(accountID)+".json")
}

func accountDir(homeDir string) string {
	if homeDir == "" {
		homeDir = defaultSelfMindHome()
	}
	return filepath.Join(homeDir, "weixin", "accounts")
}

func defaultSelfMindHome() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".", ".selfmind")
	}
	return filepath.Join(home, ".selfmind")
}

// tokenEntry pairs an iLink context_token with when it was captured from an
// inbound message. Age matters: proactive pushes on a fresh token deliver,
// while pushes on a stale token are accepted by the API (ret=0) yet observed
// to never reach the phone — the send response cannot distinguish the two, so
// freshness is the only delivery-confidence signal we have.
type tokenEntry struct {
	Token   string `json:"token"`
	SavedAt int64  `json:"saved_at,omitempty"`
}

type ContextTokenStore struct {
	root  string
	mu    sync.Mutex
	cache map[string]tokenEntry
}

func NewContextTokenStore(homeDir string) *ContextTokenStore {
	return &ContextTokenStore{root: accountDir(homeDir), cache: map[string]tokenEntry{}}
}

func (s *ContextTokenStore) Restore(accountID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(filepath.Join(s.root, safeFileName(accountID)+".context-tokens.json"))
	if err != nil {
		return
	}
	// Current format: map[peer]tokenEntry. Legacy format: map[peer]string
	// (no timestamp — age unknown, treated as stale for push confidence).
	var payload map[string]tokenEntry
	if err := json.Unmarshal(data, &payload); err == nil {
		for peer, entry := range payload {
			if entry.Token != "" {
				s.cache[s.key(accountID, peer)] = entry
			}
		}
		return
	}
	var legacy map[string]string
	if err := json.Unmarshal(data, &legacy); err != nil {
		return
	}
	for peer, token := range legacy {
		if token != "" {
			s.cache[s.key(accountID, peer)] = tokenEntry{Token: token}
		}
	}
}

func (s *ContextTokenStore) Get(accountID, peerID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cache[s.key(accountID, peerID)].Token
}

// Age reports how long ago the peer's context_token was captured. ok is false
// when there is no token or its capture time is unknown (legacy persistence) —
// callers must treat both as stale.
func (s *ContextTokenStore) Age(accountID, peerID string) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, found := s.cache[s.key(accountID, peerID)]
	if !found || entry.Token == "" || entry.SavedAt <= 0 {
		return 0, false
	}
	return time.Since(time.Unix(entry.SavedAt, 0)), true
}

func (s *ContextTokenStore) Set(accountID, peerID, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[s.key(accountID, peerID)] = tokenEntry{Token: token, SavedAt: time.Now().Unix()}
	s.persistLocked(accountID)
}

func (s *ContextTokenStore) Delete(accountID, peerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.cache, s.key(accountID, peerID))
	s.persistLocked(accountID)
}

func (s *ContextTokenStore) key(accountID, peerID string) string {
	return accountID + ":" + peerID
}

func (s *ContextTokenStore) persistLocked(accountID string) {
	prefix := accountID + ":"
	payload := map[string]tokenEntry{}
	for key, entry := range s.cache {
		if strings.HasPrefix(key, prefix) {
			payload[strings.TrimPrefix(key, prefix)] = entry
		}
	}
	data, _ := json.MarshalIndent(payload, "", "  ")
	_ = os.MkdirAll(s.root, 0700)
	path := filepath.Join(s.root, safeFileName(accountID)+".context-tokens.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err == nil {
		_ = os.Rename(tmp, path)
	}
}

// pushSessionFreshWindow is how young a peer's context_token must be for a
// proactive push to be considered deliverable. Empirical bounds from live use
// (2026-07-04): minutes-old tokens delivered, a ~4.7h-old token was accepted
// by the API but never arrived. The true iLink window is undocumented; 30m is
// a conservative cut — an "unconfirmed" push may still arrive.
const pushSessionFreshWindow = 30 * time.Minute

// PushConfidence reports whether a proactive push to chatID is expected to
// actually deliver (fresh context_token), as opposed to accepted-but-dropped.
func (c *Client) PushConfidence(chatID string) bool {
	if c == nil || c.tokens == nil {
		return false
	}
	age, ok := c.tokens.Age(c.cfg.AccountID, strings.TrimSpace(chatID))
	return ok && age <= pushSessionFreshWindow
}

type typingTicketCache struct {
	ttl   time.Duration
	mu    sync.Mutex
	cache map[string]typingEntry
}

type typingEntry struct {
	ticket string
	at     time.Time
}

func newTypingTicketCache(ttl time.Duration) *typingTicketCache {
	return &typingTicketCache{ttl: ttl, cache: map[string]typingEntry{}}
}

func (c *typingTicketCache) Get(peer string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.cache[peer]
	if !ok || time.Since(entry.at) > c.ttl {
		delete(c.cache, peer)
		return ""
	}
	return entry.ticket
}

func (c *typingTicketCache) Set(peer, ticket string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache[peer] = typingEntry{ticket: ticket, at: time.Now()}
}

type outboundMediaArgs struct {
	EncryptedQueryParam string
	AESKeyForAPI        string
	CiphertextSize      int
	PlaintextSize       int
	Filename            string
	RawFileMD5          string
}

func outboundMediaBuilder(path string, forceFile bool) (int, func(outboundMediaArgs) map[string]interface{}) {
	ext := strings.ToLower(filepath.Ext(path))
	mtype := mimeTypeForPath(path, "")
	if strings.HasPrefix(mtype, "image/") {
		return mediaImage, func(args outboundMediaArgs) map[string]interface{} {
			return map[string]interface{}{
				"type": itemImage,
				"image_item": map[string]interface{}{
					"media": map[string]interface{}{
						"encrypt_query_param": args.EncryptedQueryParam,
						"aes_key":             args.AESKeyForAPI,
						"encrypt_type":        1,
					},
					"mid_size": args.CiphertextSize,
				},
			}
		}
	}
	if strings.HasPrefix(mtype, "video/") {
		return mediaVideo, func(args outboundMediaArgs) map[string]interface{} {
			return map[string]interface{}{
				"type": itemVideo,
				"video_item": map[string]interface{}{
					"media": map[string]interface{}{
						"encrypt_query_param": args.EncryptedQueryParam,
						"aes_key":             args.AESKeyForAPI,
						"encrypt_type":        1,
					},
					"video_size": args.CiphertextSize,
					"video_md5":  args.RawFileMD5,
				},
			}
		}
	}
	if ext == ".silk" && !forceFile {
		return mediaVoice, func(args outboundMediaArgs) map[string]interface{} {
			return map[string]interface{}{
				"type": itemVoice,
				"voice_item": map[string]interface{}{
					"media": map[string]interface{}{
						"encrypt_query_param": args.EncryptedQueryParam,
						"aes_key":             args.AESKeyForAPI,
						"encrypt_type":        1,
					},
					"encode_type":     6,
					"bits_per_sample": 16,
					"sample_rate":     24000,
				},
			}
		}
	}
	return mediaFile, func(args outboundMediaArgs) map[string]interface{} {
		return map[string]interface{}{
			"type": itemFile,
			"file_item": map[string]interface{}{
				"media": map[string]interface{}{
					"encrypt_query_param": args.EncryptedQueryParam,
					"aes_key":             args.AESKeyForAPI,
					"encrypt_type":        1,
				},
				"file_name": args.Filename,
				"len":       fmt.Sprint(args.PlaintextSize),
			},
		}
	}
}

type mediaRef struct {
	Kind                string
	EncryptedQueryParam string
	AESKey              string
	FullURL             string
	Filename            string
	Ext                 string
	MimeType            string
}

func mediaReference(item map[string]interface{}) (string, *mediaRef) {
	itemType := intFromMap(item, "type")
	switch itemType {
	case itemImage:
		image := nestedMap(item, "image_item")
		media := nestedMap(image, "media")
		return "image", &mediaRef{
			Kind:                "image",
			EncryptedQueryParam: stringFromMap(media, "encrypt_query_param"),
			AESKey:              firstNonEmpty(stringFromMap(image, "aeskey"), stringFromMap(media, "aes_key")),
			FullURL:             stringFromMap(media, "full_url"),
			Ext:                 ".jpg",
			MimeType:            "image/jpeg",
		}
	case itemVideo:
		video := nestedMap(item, "video_item")
		media := nestedMap(video, "media")
		return "video", &mediaRef{
			Kind:                "video",
			EncryptedQueryParam: stringFromMap(media, "encrypt_query_param"),
			AESKey:              stringFromMap(media, "aes_key"),
			FullURL:             stringFromMap(media, "full_url"),
			Ext:                 ".mp4",
			MimeType:            "video/mp4",
		}
	case itemFile:
		file := nestedMap(item, "file_item")
		media := nestedMap(file, "media")
		filename := firstNonEmpty(stringFromMap(file, "file_name"), "document.bin")
		return "file", &mediaRef{
			Kind:                "file",
			EncryptedQueryParam: stringFromMap(media, "encrypt_query_param"),
			AESKey:              stringFromMap(media, "aes_key"),
			FullURL:             stringFromMap(media, "full_url"),
			Filename:            filename,
			Ext:                 filepath.Ext(filename),
			MimeType:            mimeTypeForPath(filename, "application/octet-stream"),
		}
	case itemVoice:
		voice := nestedMap(item, "voice_item")
		media := nestedMap(voice, "media")
		return "voice", &mediaRef{
			Kind:                "voice",
			EncryptedQueryParam: stringFromMap(media, "encrypt_query_param"),
			AESKey:              stringFromMap(media, "aes_key"),
			FullURL:             stringFromMap(media, "full_url"),
			Ext:                 ".silk",
			MimeType:            "audio/silk",
		}
	default:
		return "", nil
	}
}

func saveMedia(dataDir, kind, filename string, data []byte) (string, error) {
	if strings.TrimSpace(dataDir) == "" {
		dataDir = filepath.Join(defaultSelfMindHome(), "data")
	}
	dir := filepath.Join(dataDir, "weixin", "media", time.Now().UTC().Format("20060102"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	name := safeFileName(filename)
	if name == "" || name == "." {
		name = kind + "-" + uuid.NewString()
	}
	path := filepath.Join(dir, uuid.NewString()+"-"+name)
	if err := os.WriteFile(path, data, 0600); err != nil {
		return "", err
	}
	return path, nil
}

func ilinkHeaders(token string, body []byte) map[string]string {
	headers := map[string]string{
		"Content-Type":            "application/json",
		"AuthorizationType":       "ilink_bot_token",
		"Content-Length":          fmt.Sprint(len(body)),
		"X-WECHAT-UIN":            randomWechatUIN(),
		"iLink-App-Id":            ilinkAppID,
		"iLink-App-ClientVersion": fmt.Sprint(ilinkAppClientVersion),
	}
	if strings.TrimSpace(token) != "" {
		headers["Authorization"] = "Bearer " + strings.TrimSpace(token)
	}
	return headers
}

func randomWechatUIN() string {
	b := randomBytes(4)
	value := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprint(value)))
}

func parseAESKey(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, fmt.Errorf("empty aes key")
	}
	if raw, err := base64.StdEncoding.DecodeString(value); err == nil {
		if len(raw) == 16 {
			return raw, nil
		}
		if len(raw) == 32 && isHexString(string(raw)) {
			return hex.DecodeString(string(raw))
		}
	}
	if len(value) == 32 && isHexString(value) {
		return hex.DecodeString(value)
	}
	return nil, fmt.Errorf("unexpected aes key format")
}

func aes128ECBEncrypt(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	padded := pkcs7Pad(plaintext, block.BlockSize())
	out := make([]byte, len(padded))
	for start := 0; start < len(padded); start += block.BlockSize() {
		block.Encrypt(out[start:start+block.BlockSize()], padded[start:start+block.BlockSize()])
	}
	return out, nil
}

func aes128ECBDecrypt(ciphertextBytes, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertextBytes)%block.BlockSize() != 0 {
		return nil, fmt.Errorf("ciphertext is not block-aligned")
	}
	out := make([]byte, len(ciphertextBytes))
	mode := newECBDecrypter(block)
	mode.CryptBlocks(out, ciphertextBytes)
	return pkcs7Unpad(out, block.BlockSize())
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	pad := blockSize - (len(data) % blockSize)
	out := make([]byte, 0, len(data)+pad)
	out = append(out, data...)
	out = append(out, bytes.Repeat([]byte{byte(pad)}, pad)...)
	return out
}

func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return nil, fmt.Errorf("invalid pkcs7 data")
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > blockSize || pad > len(data) {
		return nil, fmt.Errorf("invalid pkcs7 padding")
	}
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return nil, fmt.Errorf("invalid pkcs7 padding")
		}
	}
	return data[:len(data)-pad], nil
}

type ecbDecrypter struct {
	b         cipher.Block
	blockSize int
}

func newECBDecrypter(b cipher.Block) cipher.BlockMode {
	return &ecbDecrypter{b: b, blockSize: b.BlockSize()}
}

func (x *ecbDecrypter) BlockSize() int { return x.blockSize }

func (x *ecbDecrypter) CryptBlocks(dst, src []byte) {
	if len(src)%x.blockSize != 0 {
		panic("ecb: input not full blocks")
	}
	if len(dst) < len(src) {
		panic("ecb: output smaller than input")
	}
	for len(src) > 0 {
		x.b.Decrypt(dst[:x.blockSize], src[:x.blockSize])
		src = src[x.blockSize:]
		dst = dst[x.blockSize:]
	}
}

func aesPaddedSize(size int) int {
	return ((size + 1 + 15) / 16) * 16
}

func extractMediaPaths(content string) ([]string, string) {
	var media []string
	re := regexp.MustCompile(`(?m)MEDIA:([^\s]+)`)
	cleaned := re.ReplaceAllStringFunc(content, func(match string) string {
		path := strings.TrimSpace(strings.TrimPrefix(match, "MEDIA:"))
		addMediaPath(&media, path)
		return ""
	})
	fileURLRe := regexp.MustCompile(`file://[^\s)]+`)
	cleaned = fileURLRe.ReplaceAllStringFunc(cleaned, func(match string) string {
		path := strings.TrimPrefix(match, "file://")
		addMediaPath(&media, path)
		return match
	})
	for _, path := range detectLocalMediaPaths(cleaned) {
		addMediaPath(&media, path)
	}
	return media, strings.TrimSpace(cleaned)
}

func detectLocalMediaPaths(content string) []string {
	var out []string
	tokenRe := regexp.MustCompile(`(?:[A-Za-z]:[\\/]|/)[^\s)\]}]+`)
	for _, match := range tokenRe.FindAllString(content, -1) {
		path := cleanLocalPathCandidate(match)
		if path == "" || !looksSendableAttachment(path) {
			continue
		}
		addMediaPath(&out, path)
	}
	return out
}

func cleanLocalPathCandidate(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, "`'\".,;:")
	if strings.HasPrefix(value, "file://") {
		value = strings.TrimPrefix(value, "file://")
	}
	if idx := strings.LastIndex(value, ":"); idx > 1 && idx < len(value)-1 && isDigits(value[idx+1:]) && filepath.Ext(value[:idx]) != "" {
		value = value[:idx]
	}
	return strings.TrimSpace(value)
}

func looksSendableAttachment(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".bmp", ".mp4", ".mov", ".mkv", ".avi", ".pdf", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx", ".zip", ".txt", ".md", ".csv", ".json", ".silk", ".mp3", ".wav", ".m4a", ".aac", ".ogg":
	default:
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Size() > 0
}

func addMediaPath(paths *[]string, path string) {
	path = cleanLocalPathCandidate(path)
	if path == "" {
		return
	}
	for _, existing := range *paths {
		if existing == path {
			return
		}
	}
	*paths = append(*paths, path)
}

func isDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func splitTextForDelivery(content string, max int, perLine bool) []string {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	if max <= 0 || len(content) <= max {
		if perLine && strings.Contains(content, "\n") {
			var out []string
			for _, part := range strings.Split(content, "\n") {
				part = strings.TrimSpace(part)
				if part != "" {
					out = append(out, part)
				}
			}
			return out
		}
		return []string{content}
	}
	var out []string
	for len(content) > max {
		cut := strings.LastIndex(content[:max], "\n")
		if cut < max/2 {
			cut = max
		}
		out = append(out, strings.TrimSpace(content[:cut]))
		content = strings.TrimSpace(content[cut:])
	}
	if content != "" {
		out = append(out, content)
	}
	return out
}

func formatForWeixin(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	content = regexp.MustCompile(`\[([^\]]+)\]\((https?://[^)]+)\)`).ReplaceAllString(content, "$1 ($2)")
	return strings.TrimSpace(content)
}

func mimeTypeForPath(path, fallbackValue string) string {
	if mt := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); mt != "" {
		return mt
	}
	if fallbackValue != "" {
		return fallbackValue
	}
	return "application/octet-stream"
}

func randomHex(n int) string {
	return hex.EncodeToString(randomBytes(n))
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func isHexString(value string) bool {
	for _, ch := range value {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return value != ""
}

func isOKCode(code int) bool {
	return code == 0
}

func isSessionExpired(ret, errcode int, errmsg string) bool {
	if ret == sessionExpiredCode || errcode == sessionExpiredCode {
		return true
	}
	return (ret == rateLimitCode || errcode == rateLimitCode) && strings.EqualFold(strings.TrimSpace(errmsg), "unknown error")
}

func safeID(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 8 {
		return value
	}
	return value[:8] + "..."
}

func safeFileName(value string) string {
	value = filepath.Base(strings.TrimSpace(value))
	value = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= 'A' && r <= 'Z':
			return r
		case r >= '0' && r <= '9':
			return r
		case strings.ContainsRune("._-", r):
			return r
		default:
			return '-'
		}
	}, value)
	return strings.Trim(value, ".-")
}

func stringFromMap(m map[string]interface{}, key string) string {
	if m == nil {
		return ""
	}
	value := m[key]
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func intFromMap(m map[string]interface{}, key string) int {
	if m == nil || m[key] == nil {
		return 0
	}
	switch v := m[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		i, _ := v.Int64()
		return int(i)
	default:
		var i int
		_, _ = fmt.Sscanf(fmt.Sprint(v), "%d", &i)
		return i
	}
}

func nestedMap(m map[string]interface{}, keys ...string) map[string]interface{} {
	current := m
	for _, key := range keys {
		if current == nil {
			return nil
		}
		next, _ := current[key].(map[string]interface{})
		current = next
	}
	return current
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
