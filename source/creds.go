package main

// Credentials for servers LagSleuth can ping but whose event logs or performance counters it may not read as
// the Windows account it runs as.
//
// They are kept in memory only: never written to config.json, the data folder, Windows Credential Manager or
// any log, and gone when LagSleuth exits. The password is held encrypted (Windows memory encryption, readable
// only by this process) and decrypted only for the moment it is handed to Windows to log on. It is never sent
// back by the API, and it never appears on a command line. They can only be entered from a browser on this PC.

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

type credEntry struct {
	Host   string
	User   string // as entered, e.g. DOMAIN\name, SERVER\name or name@domain.com
	user   string // the part passed to Windows as the user name
	domain string // ... and the domain ("" for name@domain.com)
	Set    time.Time
	sec    *secret // encrypted password + network logon token (platform specific)
}

var (
	credMu  sync.RWMutex
	creds   = map[string]*credEntry{}
	credVer = map[string]int{} // bumped whenever a host's credentials change, so collectors reconnect
)

// splitUser turns DOMAIN\name into (name, DOMAIN); name@domain and plain names are passed as they are.
func splitUser(u string) (user, domain string) {
	if i := strings.Index(u, `\`); i > 0 {
		return u[i+1:], u[:i]
	}
	return u, ""
}

func credVersion(host string) int {
	credMu.RLock()
	defer credMu.RUnlock()
	return credVer[host]
}

func hasCred(host string) bool {
	credMu.RLock()
	defer credMu.RUnlock()
	return creds[host] != nil
}

func setCred(host, user string, password []uint16) error {
	u, d := splitUser(user)
	sec, err := newSecret(u, d, password)
	if err != nil {
		return err
	}
	credMu.Lock()
	if old := creds[host]; old != nil {
		old.sec.free()
	}
	creds[host] = &credEntry{Host: host, User: user, user: u, domain: d, Set: time.Now(), sec: sec}
	credVer[host]++
	credMu.Unlock()
	hostFailMu.Lock()
	delete(hostFailUntil, host) // try the event logs again straight away
	hostFailMu.Unlock()
	return nil
}

func removeCred(host string) {
	credMu.Lock()
	if e := creds[host]; e != nil {
		e.sec.free()
		delete(creds, host)
		credVer[host]++
	}
	credMu.Unlock()
}

// clearCreds wipes every password and closes every logon token (on exit).
func clearCreds() {
	credMu.Lock()
	for h, e := range creds {
		e.sec.free()
		delete(creds, h)
	}
	credMu.Unlock()
}

// clearCredsOnExit wipes credentials when LagSleuth is stopped (Ctrl+C, closing the console window,
// logging off or shutting down). Memory is released by Windows on exit anyway; this just makes it explicit.
func clearCredsOnExit() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		clearCreds()
		os.Exit(0)
	}()
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func zeroUTF16(b []uint16) {
	for i := range b {
		b[i] = 0
	}
}

// jsonStringUTF16 decodes a raw JSON string (with its quotes) straight into UTF-16, so the password never
// becomes a Go string (strings cannot be wiped).
func jsonStringUTF16(raw []byte) ([]uint16, error) {
	bad := errors.New("password must be a JSON string")
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return nil, bad
	}
	in := raw[1 : len(raw)-1]
	out := make([]uint16, 0, len(in)+1)
	for i := 0; i < len(in); {
		c := in[i]
		if c == '\\' {
			if i+1 >= len(in) {
				return nil, bad
			}
			switch in[i+1] {
			case '"', '\\', '/':
				out = append(out, uint16(in[i+1]))
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'u':
				if i+6 > len(in) {
					return nil, bad
				}
				var v uint16
				for _, h := range in[i+2 : i+6] {
					v <<= 4
					switch {
					case h >= '0' && h <= '9':
						v |= uint16(h - '0')
					case h >= 'a' && h <= 'f':
						v |= uint16(h-'a') + 10
					case h >= 'A' && h <= 'F':
						v |= uint16(h-'A') + 10
					default:
						return nil, bad
					}
				}
				out = append(out, v)
				i += 6
				continue
			default:
				return nil, bad
			}
			i += 2
			continue
		}
		r, size := utf8.DecodeRune(in[i:])
		if r == utf8.RuneError && size <= 1 {
			return nil, bad
		}
		if r1, r2 := utf16.EncodeRune(r); r1 != utf8.RuneError {
			out = append(out, uint16(r1), uint16(r2))
		} else {
			out = append(out, uint16(r))
		}
		i += size
	}
	return out, nil
}

// localOnly allows credential requests only from a browser on this PC, opened at localhost, and only from
// LagSleuth's own page (the custom header cannot be sent cross-site without a CORS preflight, which is refused).
func localOnly(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	p := net.ParseIP(ip)
	if p == nil || !p.IsLoopback() || (host != "localhost" && host != "127.0.0.1" && host != "::1") || r.Header.Get("X-LagSleuth") != "1" {
		w.WriteHeader(http.StatusForbidden)
		writeJSON(w, map[string]string{"error": "Credentials can only be entered in a browser on the PC running LagSleuth, at http://localhost:" + listenPort()})
		return false
	}
	return true
}

func listenPort() string {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return strconv.Itoa(cfg.Port)
}

func credRoutes(mux *http.ServeMux) {
	// GET /api/credentials -> hosts with credentials (never the password)
	mux.HandleFunc("GET /api/credentials", func(w http.ResponseWriter, r *http.Request) {
		if !localOnly(w, r) {
			return
		}
		credMu.RLock()
		list := []map[string]any{}
		for _, e := range creds {
			list = append(list, map[string]any{"host": e.Host, "user": e.User, "set": float64(e.Set.Unix())})
		}
		credMu.RUnlock()
		sort.Slice(list, func(a, b int) bool { return list[a]["host"].(string) < list[b]["host"].(string) })
		writeJSON(w, list)
	})

	// POST /api/credentials {"host": "...", "user": "DOMAIN\\name", "password": "..."} -> store in memory, then test
	mux.HandleFunc("POST /api/credentials", func(w http.ResponseWriter, r *http.Request) {
		if !localOnly(w, r) {
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 16<<10))
		defer zeroBytes(body)
		var req struct {
			Host     string          `json:"host"`
			User     string          `json:"user"`
			Password json.RawMessage `json:"password"`
		}
		err := json.Unmarshal(body, &req)
		defer zeroBytes(req.Password)
		host, user := strings.TrimSpace(req.Host), strings.TrimSpace(req.User)
		if err != nil || host == "" || user == "" || !hostRE.MatchString(host) || !logEligible(host) {
			w.WriteHeader(400)
			writeJSON(w, map[string]string{"error": "Enter a server, a user name and a password."})
			return
		}
		pw, err := jsonStringUTF16(req.Password)
		defer zeroUTF16(pw)
		if err != nil || len(pw) == 0 {
			w.WriteHeader(400)
			writeJSON(w, map[string]string{"error": "Enter a password."})
			return
		}
		if err := setCred(host, user, pw); err != nil {
			w.WriteHeader(500)
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		// quick test: can we read the System log there with these credentials?
		res := map[string]any{"ok": true, "host": host, "user": user}
		if err := testEventLogAccess(host); err != nil {
			res["event_logs"] = "failed: " + err.Error()
		} else {
			res["event_logs"] = "ok"
		}
		writeJSON(w, res)
	})

	// DELETE /api/credentials/{host} -> wipe them
	mux.HandleFunc("DELETE /api/credentials/{host}", func(w http.ResponseWriter, r *http.Request) {
		if !localOnly(w, r) {
			return
		}
		removeCred(r.PathValue("host"))
		writeJSON(w, map[string]bool{"ok": true})
	})
}

// testEventLogAccess reads one recent System event from the host, using its credentials.
func testEventLogAccess(host string) error {
	if simulate {
		return nil
	}
	now := float64(time.Now().Unix())
	_, err := queryEvents(host, LogChannel{"System", "(Level=1 or Level=2 or Level=3 or Level=4 or Level=0)"}, now-7*86400, now, 1)
	return err
}
