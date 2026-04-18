package tools

import (
	"sync"
)

// ClarifyRequest represents a pending clarify request from the agent.
// The ClarifyTool blocks the agent goroutine here until the TUI delivers a response.
type ClarifyRequest struct {
	Question     string
	Choices      []string
	ResponseChan chan string
}

// ClarifyEventChan is the channel the TUI reads to detect clarify requests.
// Initialized once by RegisterClarifyCallback().
var ClarifyEventChan chan ClarifyRequest

var clarifyOnce sync.Once

// RegisterClarifyCallback initializes the ClarifyEventChan and injects ClarifyFn.
// Call this from main.go or Start() before the TUI runs.
func RegisterClarifyCallback() {
	clarifyOnce.Do(func() {
		ClarifyEventChan = make(chan ClarifyRequest, 1)
		ClarifyFn = func(question string, choices []string) string {
			req := ClarifyRequest{
				Question:     question,
				Choices:      choices,
				ResponseChan: make(chan string, 1),
			}
			select {
			case ClarifyEventChan <- req:
			default:
				// Shouldn't happen in normal single-threaded use
			}
			return <-req.ResponseChan
		}
	})
}

// SubmitClarifyResponse delivers the user's answer to a blocked ClarifyFn call.
// Called by the TUI after the user submits their answer.
func SubmitClarifyResponse(req ClarifyRequest, response string) {
	select {
	case req.ResponseChan <- response:
	default:
		// Already delivered
	}
}

// DrainClarifyChan reads and discards any stale requests.
// Used by the TUI shutdown path to unblock agent goroutines.
func DrainClarifyChan() {
	if ClarifyEventChan == nil {
		return
	}
	for {
		select {
		case req := <-ClarifyEventChan:
			req.ResponseChan <- ""
		default:
			return
		}
	}
}
