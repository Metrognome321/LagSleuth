package main

// Cross-host incidents and monitor self-check.
//
// An incident groups deviation events on different targets that start together, so "six hosts went
// slow at 12:00:01" is one row instead of six. The stall watcher checks that LagSleuth itself kept
// running on time, so a pause of the monitoring machine is not mistaken for a network problem.

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------- monitor self-check

const (
	stallTick      = 100 * time.Millisecond
	stallThreshold = 250 * time.Millisecond // a wake-up this late means LagSleuth itself was paused
)

type Stall struct {
	TS float64 `json:"ts"` // when the pause began
	Ms float64 `json:"ms"` // how long LagSleuth was late
}

var (
	stalls      []Stall
	stallsMu    sync.Mutex
	stallsDirty bool
	startedAt   = time.Now()
)

func stallsPath() string { return filepath.Join(dataDir, "data", "stalls.json") }

func loadStalls() {
	if b, err := os.ReadFile(stallsPath()); err == nil {
		json.Unmarshal(b, &stalls)
	}
}

// stallWatch wakes every 100 ms. If a wake-up is more than 250 ms late, the LagSleuth process (or the
// whole machine it runs on) was paused, and pings sent around that time cannot be trusted.
func stallWatch() {
	last := time.Now()
	lastSave := time.Now()
	for {
		time.Sleep(stallTick)
		now := time.Now()
		if late := now.Sub(last) - stallTick; late > stallThreshold {
			stallsMu.Lock()
			stalls = append(stalls, Stall{TS: float64(last.Add(stallTick).UnixNano()) / 1e9, Ms: float64(late.Milliseconds())})
			stallsDirty = true
			stallsMu.Unlock()
		}
		last = now
		if now.Sub(lastSave) > 10*time.Second {
			lastSave = now
			saveStalls()
		}
	}
}

func saveStalls() {
	stallsMu.Lock()
	if !stallsDirty {
		stallsMu.Unlock()
		return
	}
	cut := float64(time.Now().AddDate(0, 0, -cfg.RetentionDays).Unix())
	kept := stalls[:0]
	for _, s := range stalls {
		if s.TS >= cut {
			kept = append(kept, s)
		}
	}
	stalls = kept
	b, _ := json.Marshal(stalls)
	stallsDirty = false
	stallsMu.Unlock()
	os.MkdirAll(filepath.Dir(stallsPath()), 0o755)
	tmp := stallsPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, stallsPath())
	}
}

func stallsIn(start, end float64) []Stall {
	stallsMu.Lock()
	defer stallsMu.Unlock()
	out := []Stall{}
	for i := len(stalls) - 1; i >= 0; i-- {
		if s := stalls[i]; s.TS >= start && s.TS <= end {
			out = append(out, s)
		}
	}
	return out
}

func healthAPI(start, end float64) map[string]any {
	in := stallsIn(start, end)
	var worst, last *float64
	for _, s := range in {
		if worst == nil || s.Ms > *worst {
			worst = fptr(s.Ms)
		}
		if last == nil || s.TS > *last {
			last = fptr(s.TS)
		}
	}
	if len(in) > 200 {
		in = in[:200]
	}
	return map[string]any{
		"version": version, "simulated": simulate, "started_ts": float64(startedAt.Unix()),
		"stall_threshold_ms": float64(stallThreshold.Milliseconds()),
		"stall_count":        len(stallsIn(start, end)), "worst_ms": worst, "last_ts": last, "stalls": in,
		"start": start, "end": end,
	}
}

// ---------------------------------------------------------------------------- incidents

type IncidentHost struct {
	Target     string             `json:"target"`
	StartTS    float64            `json:"start_ts"`
	EndTS      float64            `json:"end_ts"`
	Ongoing    bool               `json:"ongoing"`
	DevSamples int                `json:"dev_samples"`
	Timeouts   int                `json:"timeouts"`
	PeakMs     *float64           `json:"peak_ms"`
	AvgMs      *float64           `json:"avg_ms"`
	BaselineMs *float64           `json:"baseline_ms"`
	Metrics    map[string]float64 `json:"metrics,omitempty"` // worst CPU / memory / disk values during the incident
	TopProcs   []ProcSummary      `json:"top_procs,omitempty"`
	sumMs      float64
	okN        int
}

type Incident struct {
	StartTS        float64            `json:"start_ts"`
	EndTS          float64            `json:"end_ts"`
	Ongoing        bool               `json:"ongoing"`
	DurationS      float64            `json:"duration_s"`
	OnsetSpreadS   float64            `json:"onset_spread_s"` // first to last host going slow
	Hosts          []IncidentHost     `json:"hosts"`
	Clean          []string           `json:"clean"` // monitored at the time but not affected
	Monitored      int                `json:"monitored"`
	DevSamples     int                `json:"dev_samples"`
	Timeouts       int                `json:"timeouts"`
	PeakMs         *float64           `json:"peak_ms"`
	PeakTarget     string             `json:"peak_target"`
	MonitorStallMs *float64           `json:"monitor_stall_ms"`        // LagSleuth itself paused during this incident
	Key            int64              `json:"key"`                     // stable id (start, whole seconds) for event logs
	LocalMetrics   map[string]float64 `json:"local_metrics,omitempty"` // the LagSleuth machine during the incident
	LocalTopProcs  []ProcSummary      `json:"local_top_procs,omitempty"`
	Logs           *LogSummary        `json:"logs,omitempty"`
	AlsoHit        int                `json:"also_hit,omitempty"` // report for chosen targets: other hosts slow at the same time
	lastStart      float64
}

func incidentWindow() float64 {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	if cfg.IncidentWindowS <= 0 {
		return 10
	}
	return cfg.IncidentWindowS
}

// buildIncidents groups deviation events into incidents, newest first. An event joins the current
// incident when it starts within incident_window_s of the incident's latest start, or while the
// incident is still in progress or within incident_window_s of its end (up to 5 minutes after it began).
func buildIncidents(start, end float64) []Incident {
	evs := eventsIn("", start, end, 1<<30)
	sort.SliceStable(evs, func(a, b int) bool { return evs[a].StartTS < evs[b].StartTS })
	win := incidentWindow()
	var incs []*Incident
	var cur *Incident
	for _, e := range evs {
		stop := e.StartTS + e.DurationS
		joins := cur != nil && (e.StartTS <= cur.lastStart+win || (e.StartTS <= cur.EndTS+win && e.StartTS-cur.StartTS <= 300))
		if !joins {
			cur = &Incident{StartTS: e.StartTS, EndTS: stop}
			incs = append(incs, cur)
		}
		cur.lastStart = math.Max(cur.lastStart, e.StartTS)
		cur.EndTS = math.Max(cur.EndTS, stop)
		cur.Ongoing = cur.Ongoing || e.Ongoing
		var h *IncidentHost
		for j := range cur.Hosts {
			if cur.Hosts[j].Target == e.Target {
				h = &cur.Hosts[j]
			}
		}
		if h == nil {
			cur.Hosts = append(cur.Hosts, IncidentHost{Target: e.Target, StartTS: e.StartTS, EndTS: stop, BaselineMs: e.BaselineMs})
			h = &cur.Hosts[len(cur.Hosts)-1]
		}
		h.EndTS = math.Max(h.EndTS, stop)
		h.Ongoing = h.Ongoing || e.Ongoing
		h.DevSamples += e.DevSamples
		h.Timeouts += e.Timeouts
		if okn := e.DevSamples - e.Timeouts; okn > 0 && e.AvgMs != nil {
			h.sumMs += *e.AvgMs * float64(okn)
			h.okN += okn
		}
		if e.PeakMs != nil && (h.PeakMs == nil || *e.PeakMs > *h.PeakMs) {
			h.PeakMs = fptr(*e.PeakMs)
		}
	}

	targets := targetList()
	allStalls := stallsIn(start-600, end+600)
	out := make([]Incident, 0, len(incs))
	for i := len(incs) - 1; i >= 0; i-- {
		inc := incs[i]
		inc.DurationS = inc.EndTS - inc.StartTS
		hit := map[string]bool{}
		inc.LocalMetrics = metricPeaks(metricLocal, inc.StartTS-1, inc.EndTS+1)
		inc.LocalTopProcs = topProcsDuring(metricLocal, inc.StartTS-1, inc.EndTS+1)
		for j := range inc.Hosts {
			h := &inc.Hosts[j]
			hit[h.Target] = true
			h.Metrics = metricPeaks(h.Target, h.StartTS-1, h.EndTS+1)
			h.TopProcs = topProcsDuring(h.Target, h.StartTS-1, h.EndTS+1)
			if h.okN > 0 {
				h.AvgMs = fptr(h.sumMs / float64(h.okN))
			}
			inc.DevSamples += h.DevSamples
			inc.Timeouts += h.Timeouts
			inc.OnsetSpreadS = math.Max(inc.OnsetSpreadS, h.StartTS-inc.StartTS)
			if h.PeakMs != nil && (inc.PeakMs == nil || *h.PeakMs > *inc.PeakMs) {
				inc.PeakMs, inc.PeakTarget = h.PeakMs, h.Target
			}
		}
		// worst first: by peak ms, then timeouts (a host that only timed out has no peak)
		sort.SliceStable(inc.Hosts, func(a, b int) bool {
			pa, pb := -1.0, -1.0
			if p := inc.Hosts[a].PeakMs; p != nil {
				pa = *p
			}
			if p := inc.Hosts[b].PeakMs; p != nil {
				pb = *p
			}
			if pa != pb {
				return pa > pb
			}
			return inc.Hosts[a].Timeouts > inc.Hosts[b].Timeouts
		})
		// hosts that were being pinged during the incident but stayed normal
		inc.Clean = []string{}
		for _, t := range targets {
			if hit[t] {
				continue
			}
			seen := false
			Scan(t, inc.StartTS-1, inc.EndTS+1, func(Sample) { seen = true })
			if seen {
				inc.Clean = append(inc.Clean, t)
			}
		}
		inc.Monitored = len(inc.Hosts) + len(inc.Clean)
		inc.Key = int64(inc.StartTS)
		for _, s := range allStalls {
			if s.TS >= inc.StartTS-5 && s.TS <= inc.EndTS+5 && (inc.MonitorStallMs == nil || s.Ms > *inc.MonitorStallMs) {
				inc.MonitorStallMs = fptr(s.Ms)
			}
		}
		out = append(out, *inc)
	}
	return out
}

// ---------------------------------------------------------------------------- incident time pattern

// observedHours returns the clock hours (local) in which any current target has samples.
func observedHours(start, end float64) map[int64]bool {
	hours := map[int64]bool{}
	for _, t := range targetList() {
		var lo, hi float64 = 1, 0 // current hour [lo, hi)
		Scan(t, start, end, func(s Sample) {
			if s.TS >= lo && s.TS < hi {
				return
			}
			tm := time.Unix(int64(s.TS), 0)
			h := time.Date(tm.Year(), tm.Month(), tm.Day(), tm.Hour(), 0, 0, 0, time.Local)
			lo, hi = float64(h.Unix()), float64(h.Add(time.Hour).Unix())
			hours[h.Unix()] = true
		})
	}
	return hours
}

type incidentFinding struct {
	Status        string   `json:"status"` // pattern | none | insufficient
	Text          string   `json:"text"`
	Range         *[2]int  `json:"range"`
	HoursWith     int      `json:"hours_with"`
	HoursObserved int      `json:"hours_observed"`
	MultiHost     int      `json:"multi_host"`
	TypicalHosts  int      `json:"typical_hosts"`
	WorstMs       *float64 `json:"worst_ms"`
}

// incidentPattern looks for incidents that hit 2+ hosts at the same minute of the hour, hour after hour.
func targetSet(targets []string) map[string]bool {
	keep := map[string]bool{}
	for _, t := range targets {
		keep[t] = true
	}
	return keep
}

// focusIncident trims an incident to the chosen hosts: only their rows, deviating pings, timeouts and peak.
// AlsoHit counts the other hosts that were slow at the same time; their names and figures are left out.
func focusIncident(inc Incident, keep map[string]bool) Incident {
	hosts := []IncidentHost{}
	for _, h := range inc.Hosts {
		if keep[h.Target] {
			hosts = append(hosts, h)
		}
	}
	inc.AlsoHit = len(inc.Hosts) - len(hosts)
	inc.Hosts = hosts
	clean := []string{}
	for _, c := range inc.Clean {
		if keep[c] {
			clean = append(clean, c)
		}
	}
	inc.Clean = clean
	inc.DevSamples, inc.Timeouts, inc.PeakMs, inc.PeakTarget, inc.OnsetSpreadS = 0, 0, nil, "", 0
	first, last := math.Inf(1), math.Inf(-1)
	for _, h := range hosts {
		inc.DevSamples += h.DevSamples
		inc.Timeouts += h.Timeouts
		if h.PeakMs != nil && (inc.PeakMs == nil || *h.PeakMs > *inc.PeakMs) {
			inc.PeakMs, inc.PeakTarget = h.PeakMs, h.Target
		}
		first, last = math.Min(first, h.StartTS), math.Max(last, h.StartTS)
	}
	if len(hosts) > 1 {
		inc.OnsetSpreadS = last - first
	}
	return inc
}

// incidentsFor keeps the incidents that hit at least one of the given targets.
func incidentsFor(incs []Incident, targets []string) []Incident {
	keep := targetSet(targets)
	out := []Incident{}
	for _, inc := range incs {
		for _, h := range inc.Hosts {
			if keep[h.Target] {
				out = append(out, inc)
				break
			}
		}
	}
	return out
}

func incidentPattern(incs []Incident, start, end float64) incidentFinding {
	obs := len(observedHours(start, end))
	f := incidentFinding{HoursObserved: obs}
	var hourSets [60]map[int64]bool
	var byMinute [60][]Incident
	for _, inc := range incs {
		if len(inc.Hosts) < 2 {
			continue
		}
		f.MultiHost++
		tm := time.Unix(int64(inc.StartTS), 0)
		m := tm.Minute()
		if hourSets[m] == nil {
			hourSets[m] = map[int64]bool{}
		}
		hourSets[m][time.Date(tm.Year(), tm.Month(), tm.Day(), tm.Hour(), 0, 0, 0, time.Local).Unix()] = true
		byMinute[m] = append(byMinute[m], inc)
	}
	switch {
	case obs < 4:
		f.Status = "insufficient"
		f.Text = fmt.Sprintf("Need at least 4 hours of data to spot a recurring incident time (have %d).", obs)
		return f
	case f.MultiHost == 0:
		f.Status = "none"
		f.Text = fmt.Sprintf("No incident affected more than one host at the same time in %d hours of data.", obs)
		return f
	}
	var any [60]bool
	for m := range any {
		any[m] = len(hourSets[m]) > 0
	}
	var best [2]int
	bestHours := 0
	var bestIncs []Incident
	for _, r := range circularRuns(any) {
		hours := map[int64]bool{}
		var list []Incident
		for m := r[0]; ; m = (m + 1) % 60 {
			for h := range hourSets[m] {
				hours[h] = true
			}
			list = append(list, byMinute[m]...)
			if m == r[1] {
				break
			}
		}
		if len(hours) > bestHours {
			best, bestHours, bestIncs = r, len(hours), list
		}
	}
	minHours := int(math.Max(3, math.Ceil(float64(obs)*0.4)))
	if bestHours < minHours {
		f.Status = "none"
		f.Text = fmt.Sprintf("%d incident(s) hit several hosts at once, but not at a consistent time of the hour (best: %d of %d hours).", f.MultiHost, bestHours, obs)
		return f
	}
	f.Status = "pattern"
	f.Range, f.HoursWith = &best, bestHours
	counts := make([]int, 0, len(bestIncs))
	for _, inc := range bestIncs {
		counts = append(counts, len(inc.Hosts))
		if inc.PeakMs != nil && (f.WorstMs == nil || *inc.PeakMs > *f.WorstMs) {
			f.WorstMs = fptr(*inc.PeakMs)
		}
	}
	sort.Ints(counts)
	f.TypicalHosts = counts[len(counts)/2]
	when := fmt.Sprintf(":%02d", best[0])
	if best[0] != best[1] {
		when = fmt.Sprintf(":%02d–:%02d", best[0], best[1])
	}
	f.Text = fmt.Sprintf("Incidents hitting several hosts at once start at %s past the hour: seen in %d of %d hours. Typically %d hosts affected",
		when, bestHours, obs, f.TypicalHosts)
	if f.WorstMs != nil {
		f.Text += fmt.Sprintf(", worst %.0f ms", *f.WorstMs)
	}
	f.Text += "."
	return f
}

// ---------------------------------------------------------------------------- http

func incidentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/incidents", func(w http.ResponseWriter, r *http.Request) {
		start, end := window(r)
		minHosts, _ := strconv.Atoi(r.URL.Query().Get("min_hosts"))
		all := buildIncidents(start, end)
		list := []Incident{}
		for _, inc := range all {
			if len(inc.Hosts) >= minHosts && len(list) < 500 {
				s := logSummary(inc)
				inc.Logs = &s
				list = append(list, inc)
			}
		}
		lb, la := logWindow()
		writeJSON(w, map[string]any{"incidents": list, "total": len(all), "finding": incidentPattern(all, start, end),
			"window_s": incidentWindow(), "start": start, "end": end, "log_before_s": lb, "log_after_s": la, "log_settle_s": la + logSettle})
	})

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		start, end := window(r)
		writeJSON(w, healthAPI(start, end))
	})

	mux.HandleFunc("GET /api/export/incidents.csv", func(w http.ResponseWriter, r *http.Request) {
		start, end := window(r)
		minHosts, _ := strconv.Atoi(r.URL.Query().Get("min_hosts"))
		incs := buildIncidents(start, end)
		if targets, picked := reqTargets(r); picked {
			keep := targetSet(targets)
			incs = incidentsFor(incs, targets)
			for i := range incs {
				incs[i] = focusIncident(incs[i], keep)
			}
		}
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", "attachment; filename=lagsleuth_incidents.csv")
		cw := csv.NewWriter(w)
		cw.Write([]string{"start", "end", "duration_s", "hosts_affected", "hosts_monitored", "affected", "clean",
			"worst_target", "peak_ms", "deviating_pings", "timeouts", "onset_spread_s", "monitor_stall_ms", "other_hosts_affected"})
		for i := len(incs) - 1; i >= 0; i-- {
			inc := incs[i]
			if len(inc.Hosts)+inc.AlsoHit < minHosts {
				continue
			}
			names := make([]string, len(inc.Hosts))
			for j, h := range inc.Hosts {
				if h.PeakMs == nil {
					names[j] = fmt.Sprintf("%s (timeouts only)", h.Target)
				} else {
					names[j] = fmt.Sprintf("%s (%s ms)", h.Target, num(h.PeakMs))
				}
			}
			endS := fmtTS(inc.EndTS)
			if inc.Ongoing {
				endS = "ongoing"
			}
			cw.Write([]string{fmtTS(inc.StartTS), endS, strconv.FormatFloat(math.Round(inc.DurationS*10)/10, 'f', -1, 64),
				strconv.Itoa(len(inc.Hosts)), strconv.Itoa(inc.Monitored), strings.Join(names, "; "), strings.Join(inc.Clean, "; "),
				inc.PeakTarget, num(inc.PeakMs), strconv.Itoa(inc.DevSamples), strconv.Itoa(inc.Timeouts),
				strconv.FormatFloat(math.Round(inc.OnsetSpreadS*10)/10, 'f', -1, 64), num(inc.MonitorStallMs), strconv.Itoa(inc.AlsoHit)})
		}
		cw.Flush()
	})
}
