//go:build darwin

package llm

import "testing"

func TestParseMacOSSystemProxyManualAndDirect(t *testing.T) {
	manual := parseMacOSSystemProxy(`<dictionary> {
  ExceptionsList : <array> {
    0 : 127.0.0.1
    1 : *.local
    2 : <local>
  }
  HTTPEnable : 1
  HTTPPort : 7897
  HTTPProxy : 127.0.0.1
  HTTPSEnable : 1
  HTTPSPort : 7897
  HTTPSProxy : 127.0.0.1
  ProxyAutoConfigEnable : 0
}`)
	if manual.mode != systemProxyConfigured || manual.httpProxy == nil || manual.httpsProxy == nil {
		t.Fatalf("manual snapshot = %+v", manual)
	}
	if manual.httpsProxy.String() != "http://127.0.0.1:7897" {
		t.Fatalf("https proxy = %s", manual.httpsProxy)
	}
	if len(manual.bypass) != 3 {
		t.Fatalf("bypass = %v", manual.bypass)
	}

	direct := parseMacOSSystemProxy(`<dictionary> {
  HTTPEnable : 0
  HTTPSEnable : 0
  ProxyAutoConfigEnable : 0
}`)
	if direct.mode != systemProxyDirect {
		t.Fatalf("direct snapshot = %+v", direct)
	}
}

func TestParseMacOSSystemProxyRefusesUnsupportedPAC(t *testing.T) {
	snapshot := parseMacOSSystemProxy(`<dictionary> {
  ProxyAutoConfigEnable : 1
  ProxyAutoConfigURLString : http://proxy.example.test/proxy.pac
}`)
	if snapshot.mode != systemProxyUnsupported {
		t.Fatalf("PAC snapshot = %+v", snapshot)
	}
}
