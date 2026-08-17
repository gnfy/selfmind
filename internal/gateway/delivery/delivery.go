package delivery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"selfmind/internal/control"
	"selfmind/internal/tools"
)

type Message struct {
	TenantID       string `json:"tenant_id"`
	PersonID       string `json:"person_id"`
	Platform       string `json:"platform"`
	PlatformUserID string `json:"platform_user_id,omitempty"`
	Channel        string `json:"channel"`
	TaskID         string `json:"task_id,omitempty"`
	RunID          string `json:"run_id,omitempty"`
	Content        string `json:"content"`
	// Kind types the message so platform senders can attach native affordances
	// instead of string-sniffing content. KindApproval + ApprovalID drive the
	// Telegram inline approve/reject keyboard; senders without native support
	// ignore both and fall back to the text instructions in Content.
	Kind       string `json:"kind,omitempty"`
	ApprovalID string `json:"approval_id,omitempty"`
	PartIndex  int    `json:"part_index,omitempty"`
	PartTotal  int    `json:"part_total,omitempty"`
	// LogicalKey is a control-plane effect key used to replay a durable result
	// after a daemon crash without creating a second outbound row. It is never
	// sent to the endpoint and ordinary messages leave it empty.
	LogicalKey string `json:"-"`
}

// KindApproval marks an approval-request notification.
const KindApproval = "approval"

// KindApprovalResolution closes a request previously pushed to another
// endpoint. It never carries approve/reject buttons.
const KindApprovalResolution = "approval_resolution"

// KindClarify marks a pending-question notification. Unlike an approval it has
// no native yes/no keyboard — the answer is free text, so senders render it as
// plain text and the person's next non-command reply resolves it.
const KindClarify = "clarify"

// KindFinalResult marks the terminal user-facing answer for a run. Keeping
// final answers typed lets cross-endpoint continuity surface a missed result
// without confusing it with progress, approval, or diagnostic notifications.
const KindFinalResult = "final_result"

// SessionRefreshError marks a platform failure that cannot succeed by retrying
// against the current session, but is safe to retry after a new inbound message
// refreshes that session.
type SessionRefreshError interface {
	error
	SessionRefreshRequired() bool
}

type Sender interface {
	Send(ctx context.Context, msg Message) error
}

// SenderWithReceipt lets a sender distinguish "the platform will deliver this"
// from "the platform accepted it but may silently drop it" (observed live on
// Weixin/iLink: sends on a stale context_token return success yet never reach
// the phone). confirmed=false does NOT mean failure — the message may still
// arrive — so such deliveries are marked sent_unconfirmed, never retried
// (a retry would use the same stale token and risk duplicates), and are
// surfaced later by the attach digest.
type SenderWithReceipt interface {
	SendWithReceipt(ctx context.Context, msg Message) (confirmed bool, err error)
}

type SenderFunc func(ctx context.Context, msg Message) error

func (f SenderFunc) Send(ctx context.Context, msg Message) error {
	return f(ctx, msg)
}

type Router struct {
	defaultSender Sender
	byPlatform    map[string]Sender
}

func NewRouter(defaultSender Sender) *Router {
	return &Router{defaultSender: defaultSender, byPlatform: map[string]Sender{}}
}

func (r *Router) Register(platform string, sender Sender) {
	if r == nil || sender == nil {
		return
	}
	platform = strings.ToLower(strings.TrimSpace(platform))
	if platform == "" {
		return
	}
	r.byPlatform[platform] = sender
}

// HasPlatform reports whether a message for the platform would reach a real
// sender (platform-specific or default). The gateway uses it to skip fan-out
// targets that would only fail and burn retry attempts.
func (r *Router) HasPlatform(platform string) bool {
	if r == nil {
		return false
	}
	if _, ok := r.byPlatform[strings.ToLower(strings.TrimSpace(platform))]; ok {
		return true
	}
	return r.defaultSender != nil
}

func (r *Router) Send(ctx context.Context, msg Message) error {
	_, err := r.SendWithReceipt(ctx, msg)
	return err
}

// SendWithReceipt routes like Send and passes the delivery-confidence receipt
// through when the platform sender provides one; senders without receipt
// support are assumed confirmed (their APIs fail loudly instead of dropping).
func (r *Router) SendWithReceipt(ctx context.Context, msg Message) (bool, error) {
	if r == nil {
		return false, ErrNoSender
	}
	sender := r.byPlatform[strings.ToLower(strings.TrimSpace(msg.Platform))]
	if sender == nil {
		sender = r.defaultSender
	}
	if sender == nil {
		return false, ErrNoSender
	}
	if receipted, ok := sender.(SenderWithReceipt); ok {
		return receipted.SendWithReceipt(ctx, msg)
	}
	return true, sender.Send(ctx, msg)
}

var ErrNoSender = fmt.Errorf("no outbound sender configured")

type Options struct {
	MaxMessageChars int
	RetryAttempts   int
	RetryBaseDelay  time.Duration
	PollInterval    time.Duration
	// CatchUpMaxAge bounds how old a sent_unconfirmed push may be and still be
	// re-pushed by the inbound-triggered catch-up (stale notices are noise, the
	// user likely learned the outcome elsewhere). 0 = default 4h.
	CatchUpMaxAge time.Duration
	// CatchUpLimit caps how many rows one catch-up replays (oldest first) so a
	// returning user is never flooded. 0 = default 3.
	CatchUpLimit int
}

type Service struct {
	store  *control.Store
	sender Sender
	opts   Options

	stopOnce sync.Once
	stopCh   chan struct{}
	doneCh   chan struct{}
}

func NewService(store *control.Store, sender Sender, opts Options) *Service {
	if opts.MaxMessageChars <= 0 {
		opts.MaxMessageChars = 3500
	}
	if opts.RetryAttempts <= 0 {
		opts.RetryAttempts = 3
	}
	if opts.RetryBaseDelay <= 0 {
		opts.RetryBaseDelay = 2 * time.Second
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 15 * time.Second
	}
	if opts.CatchUpMaxAge <= 0 {
		opts.CatchUpMaxAge = 4 * time.Hour
	}
	if opts.CatchUpLimit <= 0 {
		opts.CatchUpLimit = 3
	}
	return &Service{
		store:  store,
		sender: sender,
		opts:   opts,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

func (s *Service) Start(ctx context.Context) {
	if s == nil || s.store == nil {
		return
	}
	go s.loop(ctx)
}

func (s *Service) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	select {
	case <-s.doneCh:
	case <-time.After(2 * time.Second):
	}
}

// CatchUpMaxAge exposes the configured automatic recovery window for
// diagnostics. Manual recovery remains available for older pending-session
// rows, but automatic inbound-triggered replay is deliberately bounded.
func (s *Service) CatchUpMaxAge() time.Duration {
	if s == nil {
		return 0
	}
	return s.opts.CatchUpMaxAge
}

// SupportsPlatform reports whether this service can deliver to the platform.
// It is best-effort: when the configured sender does not expose platform
// routing information (custom senders, tests), it assumes delivery is possible.
func (s *Service) SupportsPlatform(platform string) bool {
	if s == nil || s.sender == nil {
		return false
	}
	if checker, ok := s.sender.(interface{ HasPlatform(string) bool }); ok {
		return checker.HasPlatform(platform)
	}
	return true
}

func (s *Service) EnqueueAndTry(ctx context.Context, msg Message) error {
	_, _, err := s.enqueueAndTry(ctx, msg)
	return err
}

// EnqueueAndTryConfirmed reports true only when every message part reached the
// durable "sent" state. pending_session and sent_unconfirmed remain recoverable
// outcomes, but callers must not stamp their source event as notified yet.
func (s *Service) EnqueueAndTryConfirmed(ctx context.Context, msg Message) (bool, error) {
	_, confirmed, err := s.enqueueAndTry(ctx, msg)
	return confirmed, err
}

// EnqueueAndTryAccepted reports whether every part exists in the durable
// outbound queue, independently of immediate endpoint confirmation. This is
// the outbox boundary used by queue finalization: once true, a daemon crash
// cannot lose the result even when the platform session is temporarily down.
func (s *Service) EnqueueAndTryAccepted(ctx context.Context, msg Message) (bool, error) {
	accepted, _, err := s.enqueueAndTry(ctx, msg)
	return accepted, err
}

func (s *Service) enqueueAndTry(ctx context.Context, msg Message) (bool, bool, error) {
	if s == nil || s.store == nil {
		return false, false, ErrNoSender
	}
	parts := splitMessage(msg.Content, s.opts.MaxMessageChars)
	var lastErr error
	accepted := true
	confirmed := true
	for i, part := range parts {
		partMsg := msg
		partMsg.Content = part
		partMsg.PartIndex = i + 1
		partMsg.PartTotal = len(parts)
		d, err := s.store.EnqueueDelivery(ctx, control.Delivery{
			TenantID:       msg.TenantID,
			PersonID:       msg.PersonID,
			Platform:       msg.Platform,
			PlatformUserID: msg.PlatformUserID,
			Channel:        msg.Channel,
			TaskID:         msg.TaskID,
			RunID:          msg.RunID,
			Content:        part,
			Kind:           msg.Kind,
			ApprovalID:     msg.ApprovalID,
			MaxAttempts:    s.opts.RetryAttempts,
			PartIndex:      i + 1,
			PartTotal:      len(parts),
			IdempotencyKey: idempotencyKey(partMsg),
		})
		if err != nil {
			lastErr = err
			accepted = false
			confirmed = false
			continue
		}
		if err := s.tryDelivery(ctx, d); err != nil {
			lastErr = err
		}
		status, err := s.store.DeliveryStatus(ctx, d.ID)
		if err != nil {
			lastErr = err
			confirmed = false
			continue
		}
		if status != "sent" {
			confirmed = false
		}
	}
	return accepted, confirmed, lastErr
}

func (s *Service) loop(ctx context.Context) {
	defer close(s.doneCh)
	ticker := time.NewTicker(s.opts.PollInterval)
	defer ticker.Stop()
	for {
		s.flushDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.stopCh:
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) flushDue(ctx context.Context) {
	deliveries, err := s.store.ListDueDeliveries(ctx, 50)
	if err != nil {
		return
	}
	for i := range deliveries {
		_ = s.tryDelivery(ctx, &deliveries[i])
	}
}

func (s *Service) tryDelivery(ctx context.Context, d *control.Delivery) error {
	if s.sender == nil {
		return ErrNoSender
	}
	// Claim before sending: EnqueueAndTry's immediate attempt and the retry
	// poller both see a freshly enqueued (immediately due) row, and without
	// mutual exclusion both send it — the recipient gets the message twice.
	// Exactly one dispatcher wins the claim; the loser treats the row as
	// handled and moves on.
	claimed, err := s.store.ClaimDelivery(ctx, d.ID)
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	if actionable, err := s.deliveryStillActionable(ctx, d); err != nil {
		_ = s.store.MarkDeliveryAttempt(ctx, d.ID, false, tools.RedactSensitive(err.Error()), time.Now().Add(s.nextDelay(d.Attempts+1)))
		return err
	} else if !actionable {
		return s.store.MarkDeliverySuperseded(ctx, d.ID, "approval is no longer pending")
	}
	msg := Message{
		TenantID:       d.TenantID,
		PersonID:       d.PersonID,
		Platform:       d.Platform,
		PlatformUserID: d.PlatformUserID,
		Channel:        d.Channel,
		TaskID:         d.TaskID,
		RunID:          d.RunID,
		Content:        d.Content,
		Kind:           d.Kind,
		ApprovalID:     d.ApprovalID,
		PartIndex:      d.PartIndex,
		PartTotal:      d.PartTotal,
	}
	confirmed := true
	if receipted, ok := s.sender.(SenderWithReceipt); ok {
		confirmed, err = receipted.SendWithReceipt(ctx, msg)
	} else {
		err = s.sender.Send(ctx, msg)
	}
	if err == nil {
		if !confirmed {
			// Accepted by the platform but delivery is doubtful (e.g. stale
			// iLink context_token). Terminal for the queue — retrying would
			// reuse the same stale session and risk duplicates — but recorded
			// distinctly so the attach digest can surface possibly-missed
			// notifications.
			return s.store.MarkDeliverySentUnconfirmed(ctx, d.ID)
		}
		return s.store.MarkDeliveryAttempt(ctx, d.ID, true, "", time.Time{})
	}
	if err == ErrNoSender {
		// No sender will ever appear for this row; retrying is futile and a
		// claimed-but-unmarked row would loop through stale-claim reclaim forever
		// (observed live: a mis-routed platform=cli approval push stuck in
		// 'sending'). Fail it permanently and keep it visible for the digest.
		_ = s.store.MarkDeliveryFailedPermanent(ctx, d.ID, "no sender for platform "+msg.Platform)
		return err
	}
	if sessionRefreshRequired(err) && catchUpWorthyKind(msg.Kind) {
		// Do not burn retries against the same broken session. The next inbound
		// from this peer is the safe recovery trigger because it refreshes the
		// platform context before catch-up runs.
		_ = s.store.MarkDeliveryPendingSession(ctx, d.ID, tools.RedactSensitive(err.Error()))
		return err
	}
	delay := s.nextDelay(d.Attempts + 1)
	next := time.Now().Add(delay)
	redacted := tools.RedactSensitive(err.Error())
	_ = s.store.MarkDeliveryAttempt(ctx, d.ID, false, redacted, next)
	return err
}

// CatchUpUnconfirmed re-pushes the person's sent_unconfirmed rows for one
// platform+channel, fired when that peer's INBOUND message just refreshed the
// platform session (e.g. a fresh iLink context_token) — the one moment a resend
// is likely to actually arrive. Anti-duplicate rails (P0-1, docs/STATUS.md
// "ACTIVE PLAN"): each row is claimed at most once (ClaimDeliveryCatchUp,
// claim-before-send), only rows fresher than CatchUpMaxAge qualify, and one
// catch-up replays at most CatchUpLimit rows oldest-first. Confirmed and
// unconfirmed sends retain the existing one-shot semantics. Session-refresh
// failures release their claim so a later inbound message can safely retry.
// Returns how many re-pushes were confirmed.
func (s *Service) CatchUpRecoverable(ctx context.Context, tenantID, personID, platform, channel string) int {
	if s == nil || s.store == nil || s.sender == nil {
		return 0
	}
	since := time.Now().Add(-s.opts.CatchUpMaxAge)
	rows, err := s.store.ListCatchUpEligible(ctx, tenantID, personID, platform, channel, since, s.opts.CatchUpLimit)
	if err != nil || len(rows) == 0 {
		return 0
	}
	confirmedCount := 0
	for i := range rows {
		d := &rows[i]
		claimed, err := s.store.ClaimDeliveryCatchUp(ctx, d.ID)
		if err != nil || !claimed {
			continue
		}
		status, err := s.replayClaimedDelivery(ctx, d)
		if err == nil && status == "sent" {
			confirmedCount++
		}
	}
	return confirmedCount
}

// RetryPendingSession performs one explicit recovery attempt for a durable
// pending-session row in the current IM peer. It intentionally excludes
// sent_unconfirmed rows because the platform may already have delivered them.
// The store claim makes concurrent manual and inbound-triggered retries safe.
func (s *Service) RetryPendingSession(ctx context.Context, tenantID, personID, platform, channel, ref string) (string, string, error) {
	if s == nil || s.store == nil || s.sender == nil {
		return "", "", ErrNoSender
	}
	d, err := s.store.FindPendingSessionDelivery(ctx, tenantID, personID, platform, channel, ref)
	if err != nil {
		return "", "", err
	}
	claimed, err := s.store.ClaimDeliveryCatchUp(ctx, d.ID)
	if err != nil {
		return d.ID, "", err
	}
	if !claimed {
		return d.ID, "", fmt.Errorf("delivery is already being recovered or is no longer pending")
	}
	status, err := s.replayClaimedDelivery(ctx, d)
	return d.ID, status, err
}

// DismissPendingSession closes one stale recovery item in the current IM
// peer. It never sends network traffic and cannot affect another channel.
func (s *Service) DismissPendingSession(ctx context.Context, tenantID, personID, platform, channel, ref string) (string, error) {
	if s == nil || s.store == nil {
		return "", ErrNoSender
	}
	d, err := s.store.FindPendingSessionDelivery(ctx, tenantID, personID, platform, channel, ref)
	if err != nil {
		return "", err
	}
	dismissed, err := s.store.DismissPendingSessionDelivery(ctx, d.ID)
	if err != nil {
		return d.ID, err
	}
	if !dismissed {
		return d.ID, fmt.Errorf("delivery is no longer pending")
	}
	return d.ID, nil
}

func (s *Service) replayClaimedDelivery(ctx context.Context, d *control.Delivery) (string, error) {
	actionable, err := s.deliveryStillActionable(ctx, d)
	if err != nil {
		_ = s.store.MarkDeliveryPendingSession(ctx, d.ID, tools.RedactSensitive(err.Error()))
		return "pending_session", err
	}
	if !actionable {
		if err := s.store.MarkDeliverySuperseded(ctx, d.ID, "approval is no longer pending"); err != nil {
			return "", err
		}
		return "superseded", nil
	}
	msg := deliveryMessage(d)
	confirmed := true
	err = nil
	if receipted, ok := s.sender.(SenderWithReceipt); ok {
		confirmed, err = receipted.SendWithReceipt(ctx, msg)
	} else {
		err = s.sender.Send(ctx, msg)
	}
	if err == nil && confirmed {
		_ = s.store.MarkDeliveryAttempt(ctx, d.ID, true, "", time.Time{})
		return "sent", nil
	}
	if err == nil {
		_ = s.store.MarkDeliverySentUnconfirmed(ctx, d.ID)
		return "sent_unconfirmed", nil
	}
	redacted := tools.RedactSensitive(err.Error())
	if sessionRefreshRequired(err) {
		// The platform did not accept the message. Clearing catchup_at through
		// MarkDeliveryPendingSession allows a later fresh inbound to try again.
		_ = s.store.MarkDeliveryPendingSession(ctx, d.ID, redacted)
		return "pending_session", err
	}
	// A fresh platform session cannot repair an unrelated transport failure.
	// Mark it failed instead of leaving a claimed pending row that looks
	// recoverable but can never be selected again.
	_ = s.store.MarkDeliveryFailedPermanent(ctx, d.ID, redacted)
	return "failed", err
}

func (s *Service) deliveryStillActionable(ctx context.Context, d *control.Delivery) (bool, error) {
	if d == nil || strings.TrimSpace(d.Kind) != KindApproval {
		return true, nil
	}
	return s.store.IsApprovalPending(ctx, d.TenantID, d.PersonID, d.ApprovalID)
}

func deliveryMessage(d *control.Delivery) Message {
	return Message{
		TenantID:       d.TenantID,
		PersonID:       d.PersonID,
		Platform:       d.Platform,
		PlatformUserID: d.PlatformUserID,
		Channel:        d.Channel,
		TaskID:         d.TaskID,
		RunID:          d.RunID,
		Content:        d.Content,
		Kind:           d.Kind,
		ApprovalID:     d.ApprovalID,
		PartIndex:      d.PartIndex,
		PartTotal:      d.PartTotal,
	}
}

// CatchUpUnconfirmed preserves the older API name while extending recovery to
// critical rows waiting for a fresh platform session.
func (s *Service) CatchUpUnconfirmed(ctx context.Context, tenantID, personID, platform, channel string) int {
	return s.CatchUpRecoverable(ctx, tenantID, personID, platform, channel)
}

func sessionRefreshRequired(err error) bool {
	if err == nil {
		return false
	}
	var typed SessionRefreshError
	return errors.As(err, &typed) && typed.SessionRefreshRequired()
}

func catchUpWorthyKind(kind string) bool {
	switch strings.TrimSpace(kind) {
	case KindFinalResult, KindApproval, KindApprovalResolution, KindClarify, "external_watch", "recovery", "maintenance_health":
		return true
	default:
		return false
	}
}

func (s *Service) nextDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	exp := math.Pow(2, float64(attempt-1))
	delay := time.Duration(exp) * s.opts.RetryBaseDelay
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}

func splitMessage(content string, max int) []string {
	content = strings.TrimSpace(content)
	if content == "" {
		return []string{""}
	}
	if max <= 0 || len(content) <= max {
		return []string{content}
	}
	var parts []string
	for len(content) > max {
		cut := strings.LastIndex(content[:max], "\n")
		if cut < max/2 {
			cut = max
		}
		parts = append(parts, strings.TrimSpace(content[:cut]))
		content = strings.TrimSpace(content[cut:])
	}
	if content != "" {
		parts = append(parts, content)
	}
	return parts
}

func idempotencyKey(msg Message) string {
	logical := strings.TrimSpace(msg.LogicalKey)
	if logical != "" {
		base := fmt.Sprintf("%s|%s|%s|%s|%s|%d", msg.TenantID, msg.PersonID, msg.Platform, msg.Channel, logical, msg.PartIndex)
		sum := sha256.Sum256([]byte(base))
		return fmt.Sprintf("%x", sum[:])
	}
	base := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%d|%s",
		msg.TenantID, msg.PersonID, msg.Platform, msg.PlatformUserID, msg.Channel, msg.TaskID, msg.RunID, msg.PartIndex, msg.Content)
	sum := sha256.Sum256([]byte(base))
	return fmt.Sprintf("%x", sum[:])
}

type WebhookSender struct {
	URL    string
	Token  string
	Client *http.Client
}

func (s *WebhookSender) Send(ctx context.Context, msg Message) error {
	if s == nil || strings.TrimSpace(s.URL) == "" {
		return ErrNoSender
	}
	body, _ := json.Marshal(msg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(s.Token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(s.Token))
	}
	client := s.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook delivery failed: %s", resp.Status)
	}
	return nil
}
