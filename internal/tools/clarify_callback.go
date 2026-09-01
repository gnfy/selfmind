package tools

// ClarifyRequest represents a pending clarify request from the agent.
// The ClarifyTool blocks the agent goroutine here until the TUI delivers a response.
type ClarifyRequest struct {
	ID           string
	Question     string
	Choices      []string
	ResponseChan chan string
}

type ClarifyBridge struct {
	events chan ClarifyRequest
}

func NewClarifyBridge() *ClarifyBridge {
	return &ClarifyBridge{events: make(chan ClarifyRequest, 1)}
}

func (b *ClarifyBridge) Handler() ClarifyHandler {
	if b == nil {
		return nil
	}
	return func(question string, choices []string) string {
		req := ClarifyRequest{
			Question:     question,
			Choices:      choices,
			ResponseChan: make(chan string, 1),
		}
		select {
		case b.events <- req:
		default:
			return ""
		}
		return <-req.ResponseChan
	}
}

func (b *ClarifyBridge) Events() <-chan ClarifyRequest {
	if b == nil {
		return nil
	}
	return b.events
}

func (b *ClarifyBridge) Submit(req ClarifyRequest, response string) {
	select {
	case req.ResponseChan <- response:
	default:
	}
}

func (b *ClarifyBridge) Drain() {
	if b == nil {
		return
	}
	for {
		select {
		case req := <-b.events:
			req.ResponseChan <- ""
		default:
			return
		}
	}
}

// ClarifyEventChan is kept for legacy callers. New code should create a
// ClarifyBridge and inject bridge.Handler() into the dispatcher.
var ClarifyEventChan chan ClarifyRequest

var defaultClarifyBridge *ClarifyBridge

// RegisterClarifyCallback initializes the ClarifyEventChan and injects ClarifyFn.
// Call this from main.go or Start() before the TUI runs.
func RegisterClarifyCallback() {
	defaultClarifyBridge = NewClarifyBridge()
	ClarifyEventChan = defaultClarifyBridge.events
	ClarifyFn = defaultClarifyBridge.Handler()
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
	if defaultClarifyBridge != nil {
		defaultClarifyBridge.Drain()
		return
	}
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
