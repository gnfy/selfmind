package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"selfmind/internal/gateway/httpapi"
	"selfmind/internal/platform/config"
	"selfmind/internal/platform/log"
	"selfmind/internal/platform/peercred"
	"selfmind/internal/tools"
)

// localControlRuntime owns the daemon-lifetime pieces of the local control
// channel: the peer verifier, the record of tool process groups it consults,
// the private state every confined tool is kept from, and on Linux the child
// subreaper that keeps detached tools inside the daemon's process tree.
type localControlRuntime struct {
	socketPath string
	execution  *tools.ExecutionRuntime
	verifier   peercred.Verifier
	// judge decides one connection. It defaults to the verifier; tests replace
	// it to observe when the daemon actually judges a peer.
	judge      func(net.Conn) peercred.Verdict
	stopReaper context.CancelFunc
}

func startLocalControl(ctx context.Context, dataDir string, cfg *config.Config, addr string) *localControlRuntime {
	socketPath := LocalControlSocketPath(dataDir)
	groups := peercred.NewGroupTracker()
	runtime := &localControlRuntime{
		socketPath: socketPath,
		execution:  tools.NewExecutionRuntime(dataDir, filepath.Join(filepath.Dir(dataDir), "runtime"), daemonPrivateState(dataDir, cfg, socketPath, addr), groups),
		verifier:   peercred.Verifier{DaemonPID: os.Getpid(), OwnerUID: os.Getuid(), Groups: groups},
		stopReaper: func() {},
	}
	runtime.judge = runtime.verifier.Check
	// One lifetime for everything the runtime starts, ended by stop.
	workCtx, cancel := context.WithCancel(ctx)
	runtime.stopReaper = cancel
	go peercred.PruneEvery(workCtx, groups, 15*time.Second)
	if enabled, err := peercred.EnableChildSubreaper(); err != nil {
		log.Warn("gateway: child subreaper unavailable; a detached tool may leave the daemon's process tree", "error", err)
	} else if enabled {
		go peercred.ReapAdoptedOrphans(workCtx, 30*time.Second, groups)
	}
	return runtime
}

// listenTCP opens the TCP listener and tells the server where it really
// listens. The Host check of a loopback daemon compares against that address,
// not against the configured text, which names port 0 when the system picks the
// port and would refuse every request.
func listenTCP(addr string, server *httpapi.Server) (net.Listener, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	server.ListenAddr = listener.Addr().String()
	return listener, nil
}

// newRestartCapability makes the secret this daemon shares with the restart
// helper it starts for a model change, and with nothing else. It lives in
// memory and in the helper's environment only, for as long as this daemon does.
// A daemon that cannot read random bytes has no capability: its helper then
// cannot stop it, and the model change reports that instead of weakening the
// check.
func newRestartCapability() string {
	secret := make([]byte, 16)
	if _, err := rand.Read(secret); err != nil {
		log.Warn("gateway: no restart capability; model changes cannot restart this daemon", "error", err)
		return ""
	}
	return hex.EncodeToString(secret)
}

// stop ends only this runtime's background lifecycle. Other runtimes keep
// their private state, process receipts, and pending cache repairs.
func (r *localControlRuntime) stop() {
	r.stopReaper()
}

// connContext attaches a lazily taken peer verdict to every local control
// connection. The verdict reads the peer's kernel credentials, so it is taken
// at the first request that claims CLI identity, while that client waits for the
// answer, and never for a probe that connected and left without asking.
func (r *localControlRuntime) connContext(ctx context.Context, conn net.Conn) context.Context {
	if _, ok := conn.(*net.UnixConn); !ok {
		return ctx
	}
	return httpapi.WithLazyLocalPeer(ctx, func() httpapi.LocalPeer {
		verdict := r.judge(conn)
		// The verdict is also taken to decide who may administer the daemon, for
		// the restart helper and for liveness reads that ask for nothing a refused
		// peer may not have. The request that actually asked for something it may
		// not have is what the daemon warns about (httpapi); here is only the trace.
		if !verdict.Verified {
			log.Debug("gateway: a local control peer was not accepted as the person's CLI", "pid", verdict.PID, "uid", verdict.UID, "reason", verdict.Reason)
		}
		return httpapi.LocalPeer{
			Socket: true, Verified: verdict.Verified, UID: verdict.UID, PID: verdict.PID, Refusal: verdict.Reason,
		}
	})
}
