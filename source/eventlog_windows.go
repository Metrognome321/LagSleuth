//go:build windows

package main

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// queryEvents reads one event log from this PC (host "") or a remote Windows host with wevtutil, which is
// built into Windows. Remote reads use the Windows account LagSleuth runs as: it needs to be in
// "Event Log Readers" (or an admin) on the remote host, and the "Remote Event Log Management" firewall
// rule must be allowed there.
func queryEvents(host string, ch LogChannel, from, to float64, max int) ([]LogEvent, error) {
	if simulate {
		return simEvents(host, ch, from, to), nil
	}
	args := []string{"qe", ch.Name, "/q:" + logQuery(ch, from, to), "/f:RenderedXml", "/e:Events", "/c:" + strconv.Itoa(max)}
	if host != "" {
		args = append(args, "/r:"+host)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wevtutil", args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, errors.New("timed out reading the event log (host unreachable or firewall blocking)")
	}
	if err != nil {
		msg := strings.TrimSpace(strings.ToValidUTF8(string(out), "?"))
		if strings.Contains(strings.ToLower(msg), "could not be found") {
			return nil, errChannelMissing
		}
		// wevtutil puts the reason on the following line(s), e.g. "Failed to open event query.\nThe RPC server is unavailable."
		msg = strings.Join(strings.Fields(msg), " ")
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	return parseEvents(out)
}
