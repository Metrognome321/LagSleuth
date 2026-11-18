//go:build !windows

package main

import "errors"

// Event logs are a Windows feature; other builds only produce simulated events (for testing the UI).
func queryEvents(host string, ch LogChannel, from, to float64, max int) ([]LogEvent, error) {
	if simulate {
		return simEvents(host, ch, from, to), nil
	}
	return nil, errors.New("event logs are only available on Windows")
}
