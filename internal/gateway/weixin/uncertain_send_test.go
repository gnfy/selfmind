package weixin

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"selfmind/internal/gateway/delivery"
)

// The platform consumes the first complete message, then the connection dies
// before its acknowledgement. A second successful response must not hide the
// fact that the person may have received the same push twice.
func TestAcceptedWeixinSendWithLostAcknowledgementIsNotRetried(t *testing.T) {
	var accepted atomic.Int32
	requests := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]interface{}
		_ = json.Unmarshal(body, &payload)
		message := nestedMap(payload, "msg")
		digest := sha256.Sum256([]byte(extractMessageText(message)))
		requests <- fmt.Sprintf("client_id=%s text_sha256=%x", stringFromMap(message, "client_id"), digest[:8])
		if accepted.Add(1) == 1 {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close()
			return
		}
		_, _ = io.WriteString(w, `{"ret":0,"errcode":0}`)
	}))
	defer server.Close()
	client := NewClient(RuntimeConfig{AccountID: "synthetic-bot", Token: "synthetic-token", BaseURL: server.URL, HomeDir: t.TempDir(), SendChunkRetries: 1})
	err := client.Send(context.Background(), "synthetic-peer", "synthetic one-time approval prompt")
	var uncertain *delivery.UnconfirmedSendError
	if accepted.Load() != 1 || !errors.As(err, &uncertain) {
		var trace []string
		for len(requests) > 0 {
			trace = append(trace, <-requests)
		}
		t.Fatalf("accepted message was retried or its uncertainty hidden: accepted=%d err=%v trace=%v", accepted.Load(), err, trace)
	}
}

func TestMalformedWeixinAcceptanceVerdictIsNeverSuccess(t *testing.T) {
	for _, body := range []string{`null`, `{"errmsg":"backend failure"}`, `{"ret":"not-a-number"}`, `{"ret":null}`, `{"ret":0,"errcode":"invalid"}`, `{"ret":0.5}`} {
		t.Run(body, func(t *testing.T) {
			var received atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				received.Add(1)
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			client := NewClient(RuntimeConfig{AccountID: "synthetic-bot", Token: "synthetic-token", BaseURL: server.URL, HomeDir: t.TempDir(), SendChunkRetries: 1})
			err := client.Send(context.Background(), "synthetic-peer", "synthetic result")
			var uncertain *delivery.UnconfirmedSendError
			if !errors.As(err, &uncertain) || received.Load() != 1 {
				t.Fatalf("invalid acceptance was successful or retried: calls=%d err=%v", received.Load(), err)
			}
		})
	}
}

func TestWeixinSendAcceptsProtobufZeroOmittedEmptyObject(t *testing.T) {
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		received.Add(1)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	client := NewClient(RuntimeConfig{AccountID: "synthetic-bot", Token: "synthetic-token", BaseURL: server.URL, HomeDir: t.TempDir(), SendChunkRetries: 1})
	if err := client.Send(context.Background(), "synthetic-peer", "synthetic result"); err != nil || received.Load() != 1 {
		t.Fatalf("protobuf zero-omitted empty JSON object should succeed without retry: calls=%d err=%v", received.Load(), err)
	}
}

func TestKnownPreWriteConnectionFailureMayRetry(t *testing.T) {
	var received, dials atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		received.Add(1)
		_, _ = io.WriteString(w, `{"ret":0}`)
	}))
	defer server.Close()
	client := NewClient(RuntimeConfig{AccountID: "synthetic-bot", Token: "synthetic-token", BaseURL: server.URL, HomeDir: t.TempDir(), SendChunkRetries: 1})
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if dials.Add(1) == 1 {
			return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("synthetic failure before any request bytes")}
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	}
	client.httpClient.Transport = transport
	defer client.httpClient.CloseIdleConnections()
	if err := client.Send(context.Background(), "synthetic-peer", "synthetic result"); err != nil || received.Load() != 1 || dials.Load() != 2 {
		t.Fatalf("definite pre-write refusal lost safe retry: received=%d dials=%d err=%v", received.Load(), dials.Load(), err)
	}
}

func TestPartialWeixinMessageCannotRetryAsWholeMessage(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if requests.Add(1) == 1 {
			_, _ = io.WriteString(w, `{"ret":0}`)
		} else {
			_, _ = io.WriteString(w, `{"ret":-8,"errmsg":"synthetic permanent refusal"}`)
		}
	}))
	defer server.Close()
	client := NewClient(RuntimeConfig{AccountID: "synthetic-bot", Token: "synthetic-token", BaseURL: server.URL, HomeDir: t.TempDir(), SendChunkRetries: 1, SendChunkDelay: time.Nanosecond})
	err := client.Send(context.Background(), "synthetic-peer", strings.Repeat("x", 2001))
	var uncertain *delivery.UnconfirmedSendError
	if !errors.As(err, &uncertain) || requests.Load() != 2 {
		t.Fatalf("partial send became whole-message retryable: requests=%d err=%v", requests.Load(), err)
	}
}
