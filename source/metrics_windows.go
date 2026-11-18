//go:build windows

package main

import (
	"errors"
	"fmt"
	"math"
	"syscall"
	"unsafe"
)

// Performance counters through pdh.dll (the API behind Performance Monitor and typeperf). Remote machines are
// read as "\\host\Object(Instance)\Counter" with the Windows account PingWatch runs as; the remote machine
// needs the Remote Registry service and File and Printer Sharing allowed, and the account must be in
// "Performance Monitor Users" (or an admin) there.

var (
	pdh                 = syscall.NewLazyDLL("pdh.dll")
	procPdhOpenQuery    = pdh.NewProc("PdhOpenQueryW")
	procPdhAddEnglish   = pdh.NewProc("PdhAddEnglishCounterW")
	procPdhCollect      = pdh.NewProc("PdhCollectQueryData")
	procPdhGetFormatted = pdh.NewProc("PdhGetFormattedCounterValue")
	procPdhCloseQuery   = pdh.NewProc("PdhCloseQuery")
	procPdhConnect      = pdh.NewProc("PdhConnectMachineW")
)

const (
	pdhFmtDouble   = 0x00000200
	pdhFmtNoCap100 = 0x00008000
)

// PDH_FMT_COUNTERVALUE: DWORD status, then a union whose double is 8-byte aligned on both 32- and 64-bit.
type pdhFmtValue struct {
	CStatus uint32
	_       uint32
	Value   float64
}

func pdhError(code uintptr) error {
	switch uint32(code) {
	case 0x800007D0, 0xC0000BB4:
		return errors.New("the computer could not be reached or refused the connection (check Remote Registry service, File and Printer Sharing firewall rule, and access)")
	case 0xC0000BDB:
		return errors.New("access is denied (add the account to Performance Monitor Users on that server)")
	case 0xC0000BB8:
		return errors.New("counter object not found")
	case 0xC0000BB9:
		return errors.New("counter not found")
	case 0x800007D5:
		return errors.New("no data")
	}
	if code < 0x10000 {
		return syscall.Errno(code)
	}
	return fmt.Errorf("performance counter error 0x%08X", uint32(code))
}

type pdhSource struct {
	query    uintptr
	counters []uintptr // 0 = this machine does not have that counter (e.g. no VMware Tools)
}

func openCounters(host string) (counterSource, error) {
	if simulate {
		return &simSource{host: host}, nil
	}
	prefix := ""
	if host != metricLocal {
		prefix = `\\` + host
		name, _ := syscall.UTF16PtrFromString(prefix)
		if r, _, _ := procPdhConnect.Call(uintptr(unsafe.Pointer(name))); r != 0 {
			return nil, pdhError(r)
		}
	}
	s := &pdhSource{counters: make([]uintptr, len(metricDefs))}
	if r, _, _ := procPdhOpenQuery.Call(0, 0, uintptr(unsafe.Pointer(&s.query))); r != 0 {
		return nil, pdhError(r)
	}
	var firstErr error
	n := 0
	for i, m := range metricDefs {
		path, _ := syscall.UTF16PtrFromString(prefix + m.Path)
		var h uintptr
		if r, _, _ := procPdhAddEnglish.Call(s.query, uintptr(unsafe.Pointer(path)), 0, uintptr(unsafe.Pointer(&h))); r != 0 {
			if firstErr == nil {
				firstErr = pdhError(r)
			}
			continue
		}
		s.counters[i] = h
		n++
	}
	if n == 0 {
		s.close()
		if firstErr == nil {
			firstErr = errors.New("no performance counters available")
		}
		return nil, firstErr
	}
	// rate counters (%, per second, avg. sec/read) need two samples before they have a value
	if r, _, _ := procPdhCollect.Call(s.query); r != 0 {
		s.close()
		return nil, pdhError(r)
	}
	return s, nil
}

func (s *pdhSource) available() []string {
	keys := []string{}
	for i, h := range s.counters {
		if h != 0 {
			keys = append(keys, metricDefs[i].Key)
		}
	}
	return keys
}

func (s *pdhSource) collect() ([]float64, error) {
	if r, _, _ := procPdhCollect.Call(s.query); r != 0 {
		return nil, pdhError(r)
	}
	vals := make([]float64, len(metricDefs))
	for i, h := range s.counters {
		vals[i] = math.NaN()
		if h == 0 {
			continue
		}
		var v pdhFmtValue
		r, _, _ := procPdhGetFormatted.Call(h, pdhFmtDouble|pdhFmtNoCap100, 0, uintptr(unsafe.Pointer(&v)))
		if r != 0 || v.CStatus > 1 { // 0 = valid data, 1 = new data
			continue
		}
		vals[i] = v.Value * metricDefs[i].Scale
	}
	return vals, nil
}

func (s *pdhSource) close() {
	if s.query != 0 {
		procPdhCloseQuery.Call(s.query)
		s.query = 0
	}
}
