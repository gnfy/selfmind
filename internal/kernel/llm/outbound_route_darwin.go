//go:build darwin

package llm

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const macOSSystemProxyLookupTimeout = 2 * time.Second

func platformSystemProxyLookup() systemProxyLookup { return lookupMacOSSystemProxy }

func lookupMacOSSystemProxy(parent context.Context) systemProxySnapshot {
	ctx, cancel := context.WithTimeout(parent, macOSSystemProxyLookupTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--proxy").Output()
	if err != nil {
		return systemProxySnapshot{mode: systemProxyUnavailable, detail: "macOS system proxy lookup unavailable"}
	}
	return parseMacOSSystemProxy(string(output))
}

func parseMacOSSystemProxy(output string) systemProxySnapshot {
	values := make(map[string]string)
	var bypass []string
	inExceptions := false
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "ExceptionsList") && strings.Contains(line, "<array>") {
			inExceptions = true
			continue
		}
		if inExceptions {
			if line == "}" {
				inExceptions = false
				continue
			}
			if _, value, ok := strings.Cut(line, ":"); ok {
				value = strings.TrimSpace(value)
				if value != "" {
					bypass = append(bypass, value)
				}
			}
			continue
		}
		if key, value, ok := strings.Cut(line, ":"); ok {
			values[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	if scanner.Err() != nil {
		return systemProxySnapshot{mode: systemProxyUnavailable, detail: "macOS system proxy output was unreadable"}
	}

	if enabledProxyValue(values, "ProxyAutoConfigEnable") || enabledProxyValue(values, "ProxyAutoDiscoveryEnable") {
		return systemProxySnapshot{
			mode:   systemProxyUnsupported,
			bypass: bypass,
			detail: "macOS automatic proxy configuration (PAC/WPAD) is enabled but this SelfMind build supports manual system proxies only",
		}
	}

	httpProxy := macOSProxyURL(values, "HTTP", "http")
	httpsProxy := macOSProxyURL(values, "HTTPS", "http")
	socksProxy := macOSProxyURL(values, "SOCKS", "socks5")
	if httpProxy == nil && httpsProxy == nil && socksProxy == nil {
		return systemProxySnapshot{mode: systemProxyDirect, bypass: bypass, detail: "macOS direct"}
	}
	return systemProxySnapshot{
		mode:       systemProxyConfigured,
		httpProxy:  httpProxy,
		httpsProxy: httpsProxy,
		socksProxy: socksProxy,
		bypass:     bypass,
		detail:     "macOS manual system proxy",
	}
}

func enabledProxyValue(values map[string]string, key string) bool {
	return strings.TrimSpace(values[key]) == "1"
}

func macOSProxyURL(values map[string]string, prefix, scheme string) *url.URL {
	if !enabledProxyValue(values, prefix+"Enable") {
		return nil
	}
	host := strings.TrimSpace(values[prefix+"Proxy"])
	port, err := strconv.Atoi(strings.TrimSpace(values[prefix+"Port"]))
	if host == "" || err != nil || port < 1 || port > 65535 {
		return nil
	}
	parsed, err := url.Parse(fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(host, strconv.Itoa(port))))
	if err != nil {
		return nil
	}
	return parsed
}
