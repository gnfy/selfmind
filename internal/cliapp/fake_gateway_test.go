package cliapp

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	gatewayrt "selfmind/internal/runtime/gateway"
)

// newFakeGateway serves handler the way a real daemon does: on TCP for
// lifecycle probes and IM-style callers, and on the local control socket the
// person's CLI uses for everything that carries CLI identity.
func newFakeGateway(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	// Keep the socket and its data directory in a throwaway location so the
	// test leaves nothing in the scratch HOME's ~/.selfmind.
	t.Setenv("SELF_STORAGE_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	path := gatewayrt.LocalControlSocketPath((&App{}).gatewayDataDir())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	socketServer := &http.Server{Handler: handler}
	go socketServer.Serve(listener)
	t.Cleanup(func() { _ = socketServer.Close() })
	return server
}
