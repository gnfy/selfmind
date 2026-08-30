//go:build !darwin

package llm

func platformSystemProxyLookup() systemProxyLookup { return nil }
