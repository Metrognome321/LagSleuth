//go:build !windows

package main

import "errors"

// Performance counters are a Windows feature; other builds only produce simulated metrics (for testing).
func openCounters(host string) (counterSource, error) {
	if simulate {
		return &simSource{host: host}, nil
	}
	return nil, errors.New("performance counters are only available on Windows")
}
