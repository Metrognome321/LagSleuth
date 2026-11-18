//go:build !windows

package main

import (
	"math"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// Non-Windows builds use the system ping command (kept for testing / Linux servers).

type Pinger struct{}

func newPinger() *Pinger { return &Pinger{} }
func (p *Pinger) Close() {}

var pingTimeRE = regexp.MustCompile(`([=<])\s*(\d+(?:[.,]\d+)?)\s*ms`)

func (p *Pinger) Ping(host string, timeoutMs int) float64 {
	wait := strconv.Itoa(int(math.Max(1, math.Ceil(float64(timeoutMs)/1000))))
	if runtime.GOOS == "darwin" {
		wait = strconv.Itoa(timeoutMs)
	}
	out, _ := exec.Command("ping", "-c", "1", "-W", wait, host).Output()
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(strings.ToUpper(line), "TTL") {
			continue
		}
		if m := pingTimeRE.FindStringSubmatch(line); m != nil {
			v, _ := strconv.ParseFloat(strings.Replace(m[2], ",", ".", 1), 64)
			if m[1] == "<" {
				return v / 2
			}
			return v
		}
	}
	return math.NaN()
}

func openBrowser(url string) {
	for _, c := range []string{"xdg-open", "open"} {
		if exec.Command(c, url).Start() == nil {
			return
		}
	}
}

func pauseBeforeExit() {}
