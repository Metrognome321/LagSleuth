package main

// Windows event logs around incidents.
//
// After an incident ends, PingWatch reads Critical/Error/Warning events (plus a few informational ones that
// explain stalls) from the machine it runs on and from each affected host, for a window around the incident.
// The results are stored per incident and shown under it on the dashboard.

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	logBefore       = 120.0 // seconds of log collected before an incident starts
	logAfter        = 60.0  // ... and after it ends
	logSettle       = 30.0  // wait this long after an incident ends before collecting
	logMaxPerHost   = 300
	logHostBackoff  = 30 * time.Minute
	localHostLabel  = "This PC"
	logParallelHost = 4
)

// LogChannel is one event log to read, with an XPath condition added to the time filter.
type LogChannel struct {
	Name string
	Cond string
}

// Critical/Error/Warning from the main logs, plus informational events that commonly explain a stall:
// clock changes, shadow copies / backups, scheduled tasks starting, Windows Update, disk timeouts.
var logChannels = []LogChannel{
	{"System", "(Level=1 or Level=2 or Level=3 or (Provider[@Name='Microsoft-Windows-Kernel-General'] and (EventID=1)) or Provider[@Name='volsnap'])"},
	{"Application", "(Level=1 or Level=2 or Level=3 or Provider[@Name='VSS'] or Provider[@Name='vmtools'] or Provider[@Name='VMTools'])"},
	{"Microsoft-Windows-TaskScheduler/Operational", "(EventID=100 or EventID=200 or Level=1 or Level=2 or Level=3)"},
	{"Microsoft-Windows-Storage-Storport/Operational", "(Level=1 or Level=2 or Level=3)"},
	{"Microsoft-Windows-Time-Service/Operational", "(Level=1 or Level=2 or Level=3)"},
	{"Microsoft-Windows-WindowsUpdateClient/Operational", "(Level=1 or Level=2 or Level=3 or EventID=19 or EventID=43)"},
}

type LogEvent struct {
	TS       float64 `json:"ts"`
	Channel  string  `json:"channel"`
	Provider string  `json:"provider"`
	ID       int     `json:"id"`
	Level    int     `json:"level"` // 1 critical, 2 error, 3 warning, 4 information
	Computer string  `json:"computer"`
	Message  string  `json:"message"`
}

type HostLogs struct {
	Host    string     `json:"host"`
	Label   string     `json:"label"`
	Status  string     `json:"status"` // ok | no_access | skipped | error
	Error   string     `json:"error,omitempty"`
	Events  []LogEvent `json:"events"`
	Capped  bool       `json:"capped,omitempty"`
	Skipped []string   `json:"skipped,omitempty"` // extra logs that could not be read (e.g. need admin)
}

type IncidentLogs struct {
	Key       int64      `json:"key"` // incident start, whole seconds
	From      float64    `json:"from"`
	To        float64    `json:"to"`
	Collected float64    `json:"collected"`
	Hosts     []HostLogs `json:"hosts"`
}

var (
	logStore      = map[int64]*IncidentLogs{}
	logStoreMu    sync.Mutex
	logStoreDirty bool
	logBusy       = map[int64]bool{}
	hostFailUntil = map[string]time.Time{}
	hostFailMu    sync.Mutex
)

func logsPath() string { return filepath.Join(dataDir, "data", "eventlogs.json") }

func loadLogs() {
	b, err := os.ReadFile(logsPath())
	if err != nil {
		return
	}
	var list []*IncidentLogs
	if json.Unmarshal(b, &list) == nil {
		for _, l := range list {
			logStore[l.Key] = l
		}
	}
}

func saveLogs() {
	logStoreMu.Lock()
	if !logStoreDirty {
		logStoreMu.Unlock()
		return
	}
	cut := float64(time.Now().AddDate(0, 0, -cfg.RetentionDays).Unix())
	list := make([]*IncidentLogs, 0, len(logStore))
	for k, l := range logStore {
		if l.To < cut {
			delete(logStore, k)
			continue
		}
		list = append(list, l)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })
	b, _ := json.Marshal(list)
	logStoreDirty = false
	logStoreMu.Unlock()
	os.MkdirAll(filepath.Dir(logsPath()), 0o755)
	tmp := logsPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, logsPath())
	}
}

func incidentKey(inc Incident) int64 { return int64(inc.StartTS) }

// worthLogging: skip one-ping blips on a single host; they are frequent and rarely explained by a log entry.
func worthLogging(inc Incident) bool {
	return len(inc.Hosts) >= 2 || inc.DurationS >= 5 || inc.Timeouts >= 3
}

// logEligible says whether a target can have its logs read: host names and private IPv4/IPv6 addresses.
// Public addresses such as 8.8.8.8 are never Windows machines we manage.
func logEligible(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return true
	}
	return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()
}

func eventLogsEnabled() bool {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return cfg.EventLogs
}

// collectFor reads the logs for one incident from this PC and every affected host, in parallel.
func collectFor(inc Incident) *IncidentLogs {
	from, to := inc.StartTS-logBefore, inc.EndTS+logAfter
	type job struct{ host, label string }
	jobs := []job{{"", localHostLabel}}
	for _, h := range inc.Hosts {
		jobs = append(jobs, job{h.Target, h.Target})
	}
	out := &IncidentLogs{Key: incidentKey(inc), From: from, To: to, Hosts: make([]HostLogs, len(jobs))}
	sem := make(chan struct{}, logParallelHost)
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out.Hosts[i] = collectHost(j.host, j.label, from, to)
		}(i, j)
	}
	wg.Wait()
	out.Collected = float64(time.Now().Unix())
	return out
}

func collectHost(host, label string, from, to float64) HostLogs {
	hl := HostLogs{Host: host, Label: label, Events: []LogEvent{}}
	if host != "" && !logEligible(host) {
		hl.Status = "skipped"
		hl.Error = "public address - not a managed Windows host"
		return hl
	}
	hostFailMu.Lock()
	until, failing := hostFailUntil[host]
	hostFailMu.Unlock()
	if host != "" && failing && time.Now().Before(until) {
		hl.Status = "no_access"
		hl.Error = "skipped: no access on a recent attempt (retrying after " + until.Format("15:04") + ")"
		return hl
	}
	hl.Status = "ok"
	for i, ch := range logChannels {
		evs, err := queryEvents(host, ch, from, to, logMaxPerHost)
		if err != nil {
			if err == errChannelMissing {
				continue
			}
			if i == 0 { // System is read first: if that fails, the host itself is unreachable or refuses us
				hl.Status, hl.Error = "error", err.Error()
				if host != "" && isAccessError(err) {
					hl.Status = "no_access"
					hostFailMu.Lock()
					hostFailUntil[host] = time.Now().Add(logHostBackoff)
					hostFailMu.Unlock()
				}
				break
			}
			// an extra log we may not read (some operational logs need admin): note it and carry on
			hl.Skipped = append(hl.Skipped, ch.Name+" ("+err.Error()+")")
			continue
		}
		hl.Events = append(hl.Events, evs...)
	}
	sort.SliceStable(hl.Events, func(a, b int) bool { return hl.Events[a].TS < hl.Events[b].TS })
	if len(hl.Events) > logMaxPerHost {
		hl.Events, hl.Capped = hl.Events[:logMaxPerHost], true
	}
	return hl
}

func isAccessError(err error) bool {
	s := strings.ToLower(err.Error())
	for _, k := range []string{"access is denied", "rpc server is unavailable", "timed out", "network path was not found",
		"not found or access", "unknown host", "failed to open event query", "no such host", "logon failure", "endpoint mapper",
		"could not be reached"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

// logCollector runs in the background: once an incident has been over for logSettle seconds, collect its logs.
func logCollector() {
	for range time.Tick(15 * time.Second) {
		if !eventLogsEnabled() {
			continue
		}
		now := float64(time.Now().Unix())
		for _, inc := range buildIncidents(now-3*3600, now) {
			if inc.Ongoing || now-inc.EndTS < logSettle || !worthLogging(inc) {
				continue
			}
			k := incidentKey(inc)
			logStoreMu.Lock()
			done, busy := logStore[k] != nil, logBusy[k]
			if !done && !busy {
				logBusy[k] = true
			}
			logStoreMu.Unlock()
			if done || busy {
				continue
			}
			storeLogs(collectFor(inc))
		}
		saveLogs()
	}
}

func storeLogs(l *IncidentLogs) {
	logStoreMu.Lock()
	logStore[l.Key] = l
	delete(logBusy, l.Key)
	logStoreDirty = true
	logStoreMu.Unlock()
}

// LogSummary is the per-incident line shown in the incidents table.
type LogSummary struct {
	Status   string `json:"status"` // collected | pending | collecting | off | not_needed
	Events   int    `json:"events"`
	Warnings int    `json:"warnings"` // errors + criticals + warnings
	NoAccess int    `json:"no_access"`
}

func logSummary(inc Incident) LogSummary {
	k := incidentKey(inc)
	logStoreMu.Lock()
	l, busy := logStore[k], logBusy[k]
	logStoreMu.Unlock()
	switch {
	case l != nil:
		s := LogSummary{Status: "collected"}
		for _, h := range l.Hosts {
			if h.Status == "no_access" {
				s.NoAccess++
			}
			s.Events += len(h.Events)
			for _, e := range h.Events {
				if e.Level >= 1 && e.Level <= 3 {
					s.Warnings++
				}
			}
		}
		return s
	case busy:
		return LogSummary{Status: "collecting"}
	case !eventLogsEnabled():
		return LogSummary{Status: "off"}
	case !worthLogging(inc):
		return LogSummary{Status: "not_needed"}
	}
	return LogSummary{Status: "pending"}
}

func findIncident(key int64) (Incident, bool) {
	for _, inc := range buildIncidents(float64(key)-3600, float64(key)+3600) {
		if incidentKey(inc) == key {
			return inc, true
		}
	}
	return Incident{}, false
}

func eventLogRoutes(mux *http.ServeMux) {
	// GET /api/eventlogs?key=<incident start> -> the stored logs for that incident
	mux.HandleFunc("GET /api/eventlogs", func(w http.ResponseWriter, r *http.Request) {
		key, _ := strconv.ParseInt(r.URL.Query().Get("key"), 10, 64)
		logStoreMu.Lock()
		l := logStore[key]
		logStoreMu.Unlock()
		if l == nil {
			w.WriteHeader(404)
			writeJSON(w, map[string]string{"error": "no logs collected for this incident yet"})
			return
		}
		writeJSON(w, l)
	})
	// POST /api/eventlogs/collect?key=<incident start> -> collect (or re-collect) now
	mux.HandleFunc("POST /api/eventlogs/collect", func(w http.ResponseWriter, r *http.Request) {
		key, _ := strconv.ParseInt(r.URL.Query().Get("key"), 10, 64)
		inc, ok := findIncident(key)
		if !ok {
			w.WriteHeader(404)
			writeJSON(w, map[string]string{"error": "incident not found"})
			return
		}
		logStoreMu.Lock()
		if logBusy[key] {
			logStoreMu.Unlock()
			writeJSON(w, map[string]any{"ok": true, "status": "collecting"})
			return
		}
		logBusy[key] = true
		logStoreMu.Unlock()
		if r.URL.Query().Get("retry") == "1" { // manual retry ignores the no-access backoff
			hostFailMu.Lock()
			for _, h := range inc.Hosts {
				delete(hostFailUntil, h.Target)
			}
			hostFailMu.Unlock()
		}
		go func() { storeLogs(collectFor(inc)); saveLogs() }()
		writeJSON(w, map[string]any{"ok": true, "status": "collecting"})
	})
}

var errChannelMissing = errors.New("event log channel not found")

// parseEvents reads wevtutil "/f:RenderedXml /e:Events" output.
func parseEvents(out []byte) ([]LogEvent, error) {
	var doc struct {
		Events []struct {
			System struct {
				Provider struct {
					Name string `xml:"Name,attr"`
				} `xml:"Provider"`
				EventID     int `xml:"EventID"`
				Level       int `xml:"Level"`
				TimeCreated struct {
					SystemTime string `xml:"SystemTime,attr"`
				} `xml:"TimeCreated"`
				Channel  string `xml:"Channel"`
				Computer string `xml:"Computer"`
			} `xml:"System"`
			EventData struct {
				Data []string `xml:"Data"`
			} `xml:"EventData"`
			Rendering struct {
				Message string `xml:"Message"`
			} `xml:"RenderingInfo"`
		} `xml:"Event"`
	}
	// wevtutil writes in the console code page; make sure the XML parser only sees valid UTF-8
	if err := xml.Unmarshal([]byte(strings.ToValidUTF8(string(out), "?")), &doc); err != nil {
		return nil, fmt.Errorf("could not read event log output: %v", err)
	}
	evs := make([]LogEvent, 0, len(doc.Events))
	for _, e := range doc.Events {
		t, err := time.Parse(time.RFC3339Nano, e.System.TimeCreated.SystemTime)
		if err != nil {
			continue
		}
		msg := strings.TrimSpace(e.Rendering.Message)
		if msg == "" {
			msg = strings.Join(e.EventData.Data, " · ")
		}
		if len(msg) > 2000 {
			msg = msg[:2000] + "…"
		}
		evs = append(evs, LogEvent{TS: float64(t.UnixNano()) / 1e9, Channel: e.System.Channel, Provider: e.System.Provider.Name,
			ID: e.System.EventID, Level: e.System.Level, Computer: e.System.Computer, Message: msg})
	}
	return evs, nil
}

// simEvents produces plausible events in simulation mode so the dashboard can be tried without Windows logs.
func simEvents(host string, ch LogChannel, from, to float64) []LogEvent {
	if ch.Name != "System" && ch.Name != "Application" {
		return nil
	}
	if host == "10.0.0.6" {
		return nil
	}
	mid := from + logBefore
	pc := host
	if pc == "" {
		pc = "PINGWATCH-PC"
	}
	if ch.Name == "Application" {
		return []LogEvent{
			{TS: mid - 1.2, Channel: "Application", Provider: "VSS", ID: 8224, Level: 4, Computer: pc, Message: "The VSS service is shutting down due to idle timeout."},
			{TS: mid + 0.4, Channel: "Application", Provider: "vmtools", ID: 1000, Level: 3, Computer: pc, Message: "[ warning] [vmsvc] Freeze/thaw: guest quiesced for snapshot."},
		}
	}
	evs := []LogEvent{{TS: mid + 0.8, Channel: "System", Provider: "Microsoft-Windows-Kernel-General", ID: 1, Level: 4, Computer: pc,
		Message: "The system time has changed to ‎2026‎-‎11‎-‎18T12:00:01 from ‎2026‎-‎11‎-‎18T12:00:00."}}
	if rand.Intn(2) == 0 {
		evs = append(evs, LogEvent{TS: mid + 2.5, Channel: "System", Provider: "disk", ID: 153, Level: 3, Computer: pc,
			Message: "The IO operation at logical block address 0x2a3f10 for Disk 0 was retried."})
	}
	return evs
}

// fmtSystemTime formats a unix time for an event log XPath filter (UTC, millisecond precision).
func fmtSystemTime(ts float64) string {
	return time.Unix(0, int64(ts*1e9)).UTC().Format("2006-01-02T15:04:05.000Z")
}

func logQuery(ch LogChannel, from, to float64) string {
	return fmt.Sprintf("*[System[%s and TimeCreated[@SystemTime>='%s' and @SystemTime<='%s']]]", ch.Cond, fmtSystemTime(from), fmtSystemTime(to))
}
