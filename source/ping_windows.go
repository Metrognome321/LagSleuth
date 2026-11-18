//go:build windows

package main

import (
	"bufio"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// Native Windows ICMP (iphlpapi.dll IcmpSendEcho) - the same API ping.exe uses.
// No admin rights, no child processes, no dependence on the language of ping.exe output.

var (
	iphlpapi       = syscall.NewLazyDLL("iphlpapi.dll")
	procIcmpCreate = iphlpapi.NewProc("IcmpCreateFile")
	procIcmpClose  = iphlpapi.NewProc("IcmpCloseHandle")
	procIcmpSend   = iphlpapi.NewProc("IcmpSendEcho")
	invalidHandle  = ^uintptr(0)
	resolveCache   = map[string]resolved{}
	resolveCacheMu sync.Mutex
)

type resolved struct {
	ip   net.IP
	when time.Time
}

// ICMP_ECHO_REPLY - uintptr fields match the native pointer size on 386, amd64 and arm64
type icmpEchoReply struct {
	Address       uint32
	Status        uint32
	RoundTripTime uint32
	DataSize      uint16
	Reserved      uint16
	Data          uintptr
	Ttl           uint8
	Tos           uint8
	Flags         uint8
	OptionsSize   uint8
	OptionsData   uintptr
}

type Pinger struct{ handle uintptr }

func newPinger() *Pinger {
	h, _, _ := procIcmpCreate.Call()
	return &Pinger{handle: h}
}

func (p *Pinger) Close() {
	if p.handle != 0 && p.handle != invalidHandle {
		procIcmpClose.Call(p.handle)
	}
}

func resolve4(host string) net.IP {
	if ip := net.ParseIP(host); ip != nil {
		return ip.To4()
	}
	resolveCacheMu.Lock()
	c, ok := resolveCache[host]
	resolveCacheMu.Unlock()
	if ok && time.Since(c.when) < 5*time.Minute {
		return c.ip
	}
	addrs, err := net.LookupIP(host)
	var ip net.IP
	if err == nil {
		for _, a := range addrs {
			if v4 := a.To4(); v4 != nil {
				ip = v4
				break
			}
		}
	}
	resolveCacheMu.Lock()
	resolveCache[host] = resolved{ip, time.Now()}
	resolveCacheMu.Unlock()
	return ip
}

// Ping returns round-trip ms, or NaN on timeout / unreachable / unresolvable.
func (p *Pinger) Ping(host string, timeoutMs int) float64 {
	ip := resolve4(host)
	if ip == nil {
		if strings.Contains(host, ":") { // IPv6 literal - hand over to ping.exe
			return pingExe(host, timeoutMs)
		}
		time.Sleep(time.Duration(timeoutMs) * time.Millisecond) // DNS failure counts as a timeout
		return math.NaN()
	}
	if p.handle == 0 || p.handle == invalidHandle {
		return pingExe(host, timeoutMs)
	}
	dest := uint32(ip[0]) | uint32(ip[1])<<8 | uint32(ip[2])<<16 | uint32(ip[3])<<24
	payload := []byte("PingWatchPingWatchPingWatchPingW") // 32 bytes like ping.exe
	reply := make([]byte, int(unsafe.Sizeof(icmpEchoReply{}))+len(payload)+8+64)
	n, _, _ := procIcmpSend.Call(p.handle, uintptr(dest),
		uintptr(unsafe.Pointer(&payload[0])), uintptr(len(payload)), 0,
		uintptr(unsafe.Pointer(&reply[0])), uintptr(len(reply)), uintptr(timeoutMs))
	if n == 0 {
		return math.NaN()
	}
	r := (*icmpEchoReply)(unsafe.Pointer(&reply[0]))
	if r.Status != 0 { // IP_SUCCESS
		return math.NaN()
	}
	if r.RoundTripTime == 0 {
		return 0.5 // "<1ms"
	}
	return float64(r.RoundTripTime)
}

var pingTimeRE = regexp.MustCompile(`([=<])\s*(\d+(?:[.,]\d+)?)\s*ms`)

func pingExe(host string, timeoutMs int) float64 {
	cmd := exec.Command("ping", "-n", "1", "-w", strconv.Itoa(timeoutMs), host)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, _ := cmd.Output()
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(strings.ToUpper(line), "TTL") && !strings.Contains(line, "ms") {
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
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	cmd.Start()
}

func pauseBeforeExit() {
	fmt.Print("\nPress Enter to close...")
	bufio.NewReader(os.Stdin).ReadString('\n')
}
