package delivery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
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
	PartIndex      int    `json:"part_index,omitempty"`
	PartTotal      int    `json:"part_total,omitempty"`
}

type Sender interface {
	Send(ctx context.Context, msg Message) error
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

func (r *Router) Send(ctx context.Context, msg Message) error {
	if r == nil {
		return ErrNoSender
	}
	if sender := r.byPlatform[strings.ToLower(strings.TrimSpace(msg.Platform))]; sender != nil {
		return sender.Send(ctx, msg)
	}
	if r.defaultSender != nil {
		return r.defaultSender.Send(ctx, msg)
	}
	return ErrNoSender
}

var ErrNoSender = fmt.Errorf("no outbound sender configured")

type Options struct {
	MaxMessageChars int
	RetryAttempts   int
	RetryBaseDelay  time.Duration
	PollInterval    time.Duration
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

func (s *Service) EnqueueAndTry(ctx context.Context, msg Message) error {
	if s == nil || s.store == nil {
		return ErrNoSender
	}
	parts := splitMessage(msg.Content, s.opts.MaxMessageChars)
	var lastErr error
	for i, part := range parts {
		partMsg := msg
		partMsg.Content = part
		partMsg.PartIndex = i + 1
		partMsg.PartTotal = len(parts)
		d, err := s.store.EnqueueDelivery(ctx, control.Delivery{
			TenantID:       msg.TenantID,
			PersonID:       msg.PersonID,
			Platform:       msg.Platform,
			Channel:        msg.Channel,
			TaskID:         msg.TaskID,
			RunID:          msg.RunID,
			Content:        part,
			MaxAttempts:    s.opts.RetryAttempts,
			PartIndex:      i + 1,
			PartTotal:      len(parts),
			IdempotencyKey: idempotencyKey(partMsg),
		})
		if err != nil {
			lastErr = err
			continue
		}
		if err := s.tryDelivery(ctx, d); err != nil {
			lastErr = err
		}
	}
	return lastErr
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
	msg := Message{
		TenantID:  d.TenantID,
		PersonID:  d.PersonID,
		Platform:  d.Platform,
		Channel:   d.Channel,
		TaskID:    d.TaskID,
		RunID:     d.RunID,
		Content:   d.Content,
		PartIndex: d.PartIndex,
		PartTotal: d.PartTotal,
	}
	err := s.sender.Send(ctx, msg)
	if err == nil {
		return s.store.MarkDeliveryAttempt(ctx, d.ID, true, "", time.Time{})
	}
	if err == ErrNoSender {
		return err
	}
	delay := s.nextDelay(d.Attempts + 1)
	next := time.Now().Add(delay)
	redacted := tools.RedactSensitive(err.Error())
	_ = s.store.MarkDeliveryAttempt(ctx, d.ID, false, redacted, next)
	return err
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
	base := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%d|%s",
		msg.TenantID, msg.PersonID, msg.Platform, msg.Channel, msg.TaskID, msg.RunID, msg.PartIndex, msg.Content)
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
