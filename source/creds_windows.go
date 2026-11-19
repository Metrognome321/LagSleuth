//go:build windows

package main

import (
	"errors"
	"regexp"
	"strings"
	"syscall"
	"unsafe"
)

var (
	crypt32                = syscall.NewLazyDLL("crypt32.dll")
	procCryptProtectMemory = crypt32.NewProc("CryptProtectMemory")
	procCryptUnprotectMem  = crypt32.NewProc("CryptUnprotectMemory")

	advapi32                    = syscall.NewLazyDLL("advapi32.dll")
	procLogonUserW              = advapi32.NewProc("LogonUserW")
	procImpersonateLoggedOnUser = advapi32.NewProc("ImpersonateLoggedOnUser")
	procRevertToSelf            = advapi32.NewProc("RevertToSelf")

	wevtapi                      = syscall.NewLazyDLL("wevtapi.dll")
	procEvtOpenSession           = wevtapi.NewProc("EvtOpenSession")
	procEvtQuery                 = wevtapi.NewProc("EvtQuery")
	procEvtNext                  = wevtapi.NewProc("EvtNext")
	procEvtRender                = wevtapi.NewProc("EvtRender")
	procEvtFormatMessage         = wevtapi.NewProc("EvtFormatMessage")
	procEvtOpenPublisherMetadata = wevtapi.NewProc("EvtOpenPublisherMetadata")
	procEvtClose                 = wevtapi.NewProc("EvtClose")
)

const (
	cryptProtectBlock     = 16 // CRYPTPROTECTMEMORY_BLOCK_SIZE
	cryptSameProcess      = 0  // CRYPTPROTECTMEMORY_SAME_PROCESS: only this process can decrypt
	logonNewCredentials   = 9  // LOGON32_LOGON_NEW_CREDENTIALS: like "runas /netonly", used for network access only
	logonProviderWinNT50  = 3
	errInsufficientBuffer = 122
	errNoMoreItems        = 259
	errTimeout            = 1460
	errEvtChannelNotFound = 15007
)

// secret is a password encrypted in memory plus a network-only logon token made from it.
type secret struct {
	enc   []byte         // UTF-16 password + NUL, padded to 16 bytes, encrypted with CryptProtectMemory
	token syscall.Handle // LOGON32_LOGON_NEW_CREDENTIALS token: outgoing network access uses these credentials
}

func newSecret(user, domain string, password []uint16) (*secret, error) {
	n := (len(password) + 1) * 2
	size := (n + cryptProtectBlock - 1) / cryptProtectBlock * cryptProtectBlock
	buf := make([]byte, size)
	for i, c := range password {
		buf[2*i], buf[2*i+1] = byte(c), byte(c>>8)
	}
	s := &secret{}
	// network-only logon token (Windows does not check the password here; the remote server does on first use)
	u, _ := syscall.UTF16PtrFromString(user)
	var d *uint16
	if domain != "" {
		d, _ = syscall.UTF16PtrFromString(domain)
	}
	r, _, err := procLogonUserW.Call(uintptr(unsafe.Pointer(u)), uintptr(unsafe.Pointer(d)), uintptr(unsafe.Pointer(&buf[0])),
		logonNewCredentials, logonProviderWinNT50, uintptr(unsafe.Pointer(&s.token)))
	if r == 0 {
		zeroBytes(buf)
		return nil, errors.New("Windows refused the user name: " + err.Error())
	}
	if r, _, err := procCryptProtectMemory.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(size), cryptSameProcess); r == 0 {
		zeroBytes(buf)
		syscall.CloseHandle(s.token)
		return nil, errors.New("could not encrypt the password in memory: " + err.Error())
	}
	s.enc = buf
	return s, nil
}

// free wipes the encrypted password and closes the logon token.
func (s *secret) free() {
	if s == nil {
		return
	}
	zeroBytes(s.enc)
	s.enc = nil
	if s.token != 0 {
		syscall.CloseHandle(s.token)
		s.token = 0
	}
}

// withPassword decrypts the password into a temporary buffer for f, then wipes it.
func (s *secret) withPassword(f func(pw *uint16)) error {
	if s == nil || len(s.enc) == 0 {
		return errors.New("credentials were removed")
	}
	tmp := make([]byte, len(s.enc))
	copy(tmp, s.enc)
	defer zeroBytes(tmp)
	if r, _, err := procCryptUnprotectMem.Call(uintptr(unsafe.Pointer(&tmp[0])), uintptr(len(tmp)), cryptSameProcess); r == 0 {
		return errors.New("could not decrypt the password: " + err.Error())
	}
	f((*uint16)(unsafe.Pointer(&tmp[0])))
	return nil
}

// withPasswordUTF16 is withPassword with the password as a slice (without the trailing NUL); the slice is
// wiped as soon as f returns, so f must copy anything it keeps.
func (s *secret) withPasswordUTF16(f func(pw []uint16)) error {
	return s.withPassword(func(p *uint16) {
		n := len(s.enc) / 2
		all := unsafe.Slice(p, n)
		end := 0
		for end < n && all[end] != 0 {
			end++
		}
		f(all[:end])
	})
}

// credThreadSetup runs on a collector's locked OS thread before it connects: with credentials for the host,
// the thread impersonates their network-only token, so PDH's remote connection uses them; otherwise it uses
// LagSleuth's own account. Returns the credentials version it set up for.
func credThreadSetup(host string) int {
	procRevertToSelf.Call()
	credMu.RLock()
	defer credMu.RUnlock()
	if e := creds[host]; e != nil && host != metricLocal && e.sec.token != 0 {
		procImpersonateLoggedOnUser.Call(uintptr(e.sec.token))
	}
	return credVer[host]
}

// ---------------------------------------------------------------------------- event logs with credentials

type evtRPCLogin struct {
	Server, User, Domain, Password *uint16
	Flags                          uint32 // EvtRpcLoginAuthNegotiate = 1
}

func evtClose(h uintptr) {
	if h != 0 {
		procEvtClose.Call(h)
	}
}

// openEvtSession opens an event log session to host with its stored credentials. The password is passed
// to Windows in memory (never on a command line) and wiped as soon as the session is open.
func openEvtSession(host string) (uintptr, error) {
	credMu.RLock()
	defer credMu.RUnlock()
	e := creds[host]
	if e == nil {
		return 0, errors.New("no credentials for " + host)
	}
	srv, _ := syscall.UTF16PtrFromString(host)
	usr, _ := syscall.UTF16PtrFromString(e.user)
	var dom *uint16
	if e.domain != "" {
		dom, _ = syscall.UTF16PtrFromString(e.domain)
	}
	var h uintptr
	var callErr error
	err := e.sec.withPassword(func(pw *uint16) {
		login := evtRPCLogin{Server: srv, User: usr, Domain: dom, Password: pw, Flags: 1}
		r, _, err := procEvtOpenSession.Call(1 /* EvtRpcLogin */, uintptr(unsafe.Pointer(&login)), 0, 0)
		login.Password = nil
		h = r
		if r == 0 {
			callErr = err
		}
	})
	if err != nil {
		return 0, err
	}
	if h == 0 {
		return 0, callErr
	}
	return h, nil
}

var providerRE = regexp.MustCompile(`<Provider Name=['"]([^'"]+)['"]`)

func evtRenderXML(ev uintptr) (string, error) {
	var used, props uint32
	procEvtRender.Call(0, ev, 1 /* EvtRenderEventXml */, 0, 0, uintptr(unsafe.Pointer(&used)), uintptr(unsafe.Pointer(&props)))
	if used == 0 {
		return "", errors.New("could not render event")
	}
	buf := make([]uint16, used/2+1)
	r, _, err := procEvtRender.Call(0, ev, 1, uintptr(len(buf)*2), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&used)), uintptr(unsafe.Pointer(&props)))
	if r == 0 {
		return "", err
	}
	return syscall.UTF16ToString(buf), nil
}

// evtFormatXML returns the event as XML including its rendered message (like wevtutil /f:RenderedXml).
func evtFormatXML(meta, ev uintptr) (string, bool) {
	var used uint32
	r, _, err := procEvtFormatMessage.Call(meta, ev, 0, 0, 0, 9 /* EvtFormatMessageXml */, 0, 0, uintptr(unsafe.Pointer(&used)))
	if r == 0 && (err != syscall.Errno(errInsufficientBuffer) || used == 0) {
		return "", false
	}
	buf := make([]uint16, used+1)
	if r, _, _ := procEvtFormatMessage.Call(meta, ev, 0, 0, 0, 9, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&used))); r == 0 {
		return "", false
	}
	return syscall.UTF16ToString(buf), true
}

// queryEventsWithCred reads one log from host through the Windows event log API, with the host's credentials.
func queryEventsWithCred(host string, ch LogChannel, from, to float64, max int) ([]LogEvent, error) {
	sess, err := openEvtSession(host)
	if err != nil {
		return nil, err
	}
	defer evtClose(sess)
	path, _ := syscall.UTF16PtrFromString(ch.Name)
	query, _ := syscall.UTF16PtrFromString(logQuery(ch, from, to))
	rs, _, err := procEvtQuery.Call(sess, uintptr(unsafe.Pointer(path)), uintptr(unsafe.Pointer(query)), 0x1|0x100 /* channel path, oldest first */)
	if rs == 0 {
		if err == syscall.Errno(errEvtChannelNotFound) {
			return nil, errChannelMissing
		}
		return nil, err
	}
	defer evtClose(rs)
	meta := map[string]uintptr{}
	defer func() {
		for _, m := range meta {
			evtClose(m)
		}
	}()
	var sb strings.Builder
	sb.WriteString("<Events>")
	handles := make([]uintptr, 32)
	count := 0
	for count < max {
		var got uint32
		r, _, err := procEvtNext.Call(rs, uintptr(len(handles)), uintptr(unsafe.Pointer(&handles[0])), 25000, 0, uintptr(unsafe.Pointer(&got)))
		if r == 0 {
			if err == syscall.Errno(errNoMoreItems) {
				break
			}
			if err == syscall.Errno(errTimeout) {
				return nil, errors.New("timed out reading the event log (host unreachable or firewall blocking)")
			}
			return nil, err
		}
		for i := 0; i < int(got); i++ {
			if count < max {
				if x, err := evtRenderXML(handles[i]); err == nil {
					// add the rendered message, using the event provider's message table on that server
					if m := providerRE.FindStringSubmatch(x); m != nil {
						h, seen := meta[m[1]]
						if !seen {
							name, _ := syscall.UTF16PtrFromString(m[1])
							h, _, _ = procEvtOpenPublisherMetadata.Call(sess, uintptr(unsafe.Pointer(name)), 0, 0, 0)
							meta[m[1]] = h
						}
						if h != 0 {
							if fx, ok := evtFormatXML(h, handles[i]); ok {
								x = fx
							}
						}
					}
					sb.WriteString(x)
					count++
				}
			}
			evtClose(handles[i])
		}
	}
	sb.WriteString("</Events>")
	return parseEvents([]byte(sb.String()))
}
