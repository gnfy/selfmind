package llm

import (
	"net/http"
	"reflect"
	"testing"
)

func TestProviderTransportKeepsStandardProcessProxyResolution(t *testing.T) {
	transport := newProviderTransport()
	if transport.Proxy == nil {
		t.Fatal("provider transport must retain the standard process proxy resolver")
	}
	got := reflect.ValueOf(transport.Proxy).Pointer()
	want := reflect.ValueOf(http.ProxyFromEnvironment).Pointer()
	if got != want {
		t.Fatal("provider transport must use http.ProxyFromEnvironment instead of a captured proxy snapshot")
	}
}
