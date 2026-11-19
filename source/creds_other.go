//go:build !windows

package main

import "errors"

// Non-Windows builds (testing only): credentials are accepted for the dashboard, but nothing uses them.
type secret struct{ set bool }

func newSecret(user, domain string, password []uint16) (*secret, error) {
	return &secret{set: true}, nil
}
func (s *secret) free() {
	if s != nil {
		s.set = false
	}
}
func (s *secret) withPasswordUTF16(f func(pw []uint16)) error {
	if s == nil || !s.set {
		return errors.New("credentials were removed")
	}
	f([]uint16{'x'})
	return nil
}
func credThreadSetup(host string) int { return credVersion(host) }
func queryEventsWithCred(host string, ch LogChannel, from, to float64, max int) ([]LogEvent, error) {
	if simulate {
		return simEvents(host, ch, from, to), nil
	}
	return nil, errors.New("event logs are only available on Windows")
}
