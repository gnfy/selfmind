package cliapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"selfmind/internal/gateway/api"
	gatewayrt "selfmind/internal/runtime/gateway"
)

// Doctor and the person's own CLI channel.
//
// The CLI reaches a local daemon only over its local control socket, where the
// daemon verifies this very process. Doctor reports that channel the way the
// daemon sees it: whether the socket answers, whether its directory can be
// trusted, and the daemon's verdict on this terminal. Running doctor from
// inside a tool the daemon started is the one case the channel refuses, so the
// refusal says so.

const (
	localControlHealthy     = "healthy"
	localControlRefused     = "refused"
	localControlUnreachable = "unreachable"
	localControlUnsafe      = "unsafe"
	localControlNotRunning  = "not running"
	localControlRemote      = "remote"
)

// localControlState is what doctor learned about the channel.
type localControlState struct {
	Socket      string
	Remote      bool
	Running     bool // the daemon answers its status over TCP
	DirState    gatewayrt.LocalControlDirState
	DirDetail   string
	Dial        string // why the socket did not answer, when it did not
	Answered    bool   // the socket answered a status request
	Peer        api.LocalControlPeer
	LegacyToken bool
}

func (s localControlState) status() string {
	switch {
	case s.Remote:
		return localControlRemote
	case !s.Running:
		return localControlNotRunning
	case s.DirState == gatewayrt.LocalControlDirUnsafe:
		return localControlUnsafe
	case !s.Answered:
		return localControlUnreachable
	case !s.Peer.Verified:
		return localControlRefused
	default:
		return localControlHealthy
	}
}

func (a *App) inspectLocalControl(ctx context.Context) localControlState {
	dataDir := a.gatewayDataDir()
	state := localControlState{Socket: gatewayrt.LocalControlSocketPath(dataDir)}
	if !a.gatewayTargetIsLocal() {
		state.Remote = true
		return state
	}
	state.LegacyToken = gatewayrt.HasLegacyLocalControlToken(dataDir)
	state.DirState, state.DirDetail = gatewayrt.InspectLocalControlDir(dataDir)

	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, code, err := gatewayrt.RequestStatus(probeCtx, a.gatewayURL()); err == nil && code < http.StatusBadRequest {
		state.Running = true
	}
	if state.DirState == gatewayrt.LocalControlDirUnsafe {
		return state
	}
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, a.gatewayURL()+"/v1/gateway/status", nil)
	if err != nil {
		state.Dial = err.Error()
		return state
	}
	a.attachGatewayAuth(request)
	response, err := gatewayrt.LocalControlClient(dataDir).Do(request)
	if err != nil {
		state.Dial = err.Error()
		return state
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode >= http.StatusBadRequest {
		state.Dial = a.gatewayErrorLine(response.Status, body)
		return state
	}
	var status api.GatewayStatusResponse
	if err := json.Unmarshal(body, &status); err != nil {
		state.Dial = "the daemon's local control socket returned an unreadable status"
		return state
	}
	state.Answered = true
	state.Peer = status.LocalControl
	return state
}

// localControlDoctorSection renders the channel section of the bundle.
func (a *App) localControlDoctorSection(ctx context.Context) string {
	return formatLocalControlSection(a.inspectLocalControl(ctx))
}

func formatLocalControlSection(state localControlState) string {
	var b strings.Builder
	b.WriteString("== Local control ==\n")
	fmt.Fprintf(&b, "socket: %s\n", state.Socket)
	status := state.status()
	if status == localControlRemote {
		b.WriteString("gateway: remote\n")
		b.WriteString("Requests to a remote gateway travel over TCP, and TCP requests never carry CLI identity: the daemon accepts the person's own CLI only on its local control socket, so commands that act as you are refused by a remote gateway.\n")
		fmt.Fprintf(&b, "status: %s\n", status)
		return strings.TrimRight(b.String(), "\n")
	}
	fmt.Fprintf(&b, "directory: %s\n", state.DirDetail)
	switch {
	case !state.Running:
		b.WriteString("daemon: not running\n")
	case state.Answered:
		b.WriteString("daemon: listening on the socket\n")
	default:
		b.WriteString("daemon: answers on TCP only\n")
	}
	switch {
	case state.Answered && state.Peer.Verified:
		fmt.Fprintf(&b, "this terminal: verified as your own CLI (peer pid %d)\n", state.Peer.PID)
	case state.Answered:
		fmt.Fprintf(&b, "this terminal: refused (%s)\n", oneLine(state.Peer.Refusal, 160))
	default:
		b.WriteString("this terminal: not checked\n")
	}
	if state.LegacyToken {
		b.WriteString("legacy token file: present (retired; a current daemon deletes it at startup)\n")
	} else {
		b.WriteString("legacy token file: absent\n")
	}
	fmt.Fprintf(&b, "status: %s\n", status)
	switch status {
	case localControlRefused:
		b.WriteString("The daemon does not accept this process as your CLI because it is, or descends from, a process the gateway started (a tool or a scheduled job). Run selfmind from your own terminal; no setting changes this.\n")
	case localControlUnreachable:
		fmt.Fprintf(&b, "The running gateway does not answer on its local control socket (%s). It predates the socket or failed to open it, so the CLI cannot act as you until it restarts.\n", oneLine(state.Dial, 200))
	case localControlUnsafe:
		fmt.Fprintf(&b, "The socket directory is accessible to other users, so the CLI refuses to trust a socket in it. %s\n", oneLine(state.DirDetail, 200))
	}
	return strings.TrimRight(b.String(), "\n")
}

// localControlDoctorIssue turns an unhealthy channel into a finding. A stopped
// daemon is the Gateway section's finding, not a second one here.
func localControlDoctorIssue(body string) (doctorIssue, bool) {
	status := localControlStatusOf(body)
	switch status {
	case localControlRefused:
		return doctorIssue{Category: "CONTROL", Title: "Local control channel", Details: body, Actions: []doctorAction{{
			Description: "Run selfmind from your own terminal, not from a tool or a process the gateway started.",
			Commands:    []string{"selfmind doctor"},
		}}}, true
	case localControlUnreachable, localControlUnsafe:
		return doctorIssue{Category: "CONTROL", Title: "Local control channel", Details: body, Actions: []doctorAction{{
			Description: "Restart the gateway so it opens its local control socket with a private directory.",
			Commands:    []string{"selfmind gateway restart"},
		}}}, true
	case localControlRemote:
		return doctorIssue{Category: "CONTROL", Title: "Local control channel", Details: body, Actions: []doctorAction{{
			Description: "Point the CLI at the gateway on this machine by unsetting SELF_GATEWAY_URL/SELF_GATEWAY_ADDR and gateway.url.",
			Commands:    []string{"selfmind config doctor"},
		}}}, true
	}
	return doctorIssue{}, false
}

func localControlStatusOf(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "status: "); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// localControlReportIsFatal reports a channel that leaves the CLI unable to
// act: every command that carries CLI identity would be refused.
func localControlReportIsFatal(body string) bool {
	switch localControlStatusOf(body) {
	case localControlRefused, localControlUnreachable, localControlUnsafe:
		return true
	}
	return false
}
