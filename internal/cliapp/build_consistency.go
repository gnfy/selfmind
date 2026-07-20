package cliapp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"selfmind/internal/buildinfo"
	"selfmind/internal/gateway/api"
	gatewayrt "selfmind/internal/runtime/gateway"
)

func gatewayBuildState(daemonFingerprint string) string {
	daemonFingerprint = strings.TrimSpace(daemonFingerprint)
	if daemonFingerprint == "" {
		return "unknown"
	}
	if daemonFingerprint != buildinfo.Fingerprint() {
		return "mismatch:" + daemonFingerprint
	}
	return daemonFingerprint
}

func warnGatewayBuildMismatch(parent context.Context, gatewayURL string, stderr io.Writer) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Second)
	defer cancel()
	data, statusCode, err := gatewayrt.RequestStatus(ctx, gatewayURL)
	if err != nil || statusCode >= 400 {
		return
	}
	var status api.GatewayStatusResponse
	if json.Unmarshal(data, &status) != nil {
		return
	}
	clientBuild := buildinfo.Fingerprint()
	daemonBuild := strings.TrimSpace(status.Runtime.BuildFingerprint)
	if daemonBuild == clientBuild {
		return
	}
	if daemonBuild == "" {
		fmt.Fprintln(stderr, "SelfMind notice: the running gateway does not expose build identity. Run `selfmind gateway restart`.")
		return
	}
	fmt.Fprintf(stderr, "SelfMind notice: CLI build %s differs from gateway build %s. Run `selfmind gateway restart`.\n", clientBuild, daemonBuild)
}
