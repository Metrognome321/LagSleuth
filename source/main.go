// LagSleuth - continuous multi-host ping monitor with latency distribution and deviation reporting.
// Single self-contained executable: web UI, Chart.js and storage are all built in.
package main

import (
	"embed"
	"encoding/binary"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"math"
	"math/rand"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

var version = "1.6.4"

// ---------------------------------------------------------------------------- config

type Config struct {
	Targets                []string `json:"targets"`
	IntervalSeconds        float64  `json:"interval_seconds"`
	TimeoutMs              int      `json:"timeout_ms"`
	BaselineWindow         int      `json:"baseline_window"`
	BaselineMinSamples     int      `json:"baseline_min_samples"`
	DeviationFactor        float64  `json:"deviation_factor"`
	DeviationMinDeltaMs    float64  `json:"deviation_min_delta_ms"`
	HighMs                 float64  `json:"high_ms"`
	EventCloseAfter        int      `json:"event_close_after"`
	RetentionDays          int      `json:"retention_days"`
	RangeBucketsMs         []int    `json:"range_buckets_ms"`
	IncidentWindowS        float64  `json:"incident_window_s"`
	EventLogs              bool     `json:"event_logs"`
	Metrics                bool     `json:"metrics"`
	MetricsIntervalSeconds float64  `json:"metrics_interval_seconds"`
	MetricsRetentionDays   int      `json:"metrics_retention_days"`
	ProcessIntervalSeconds float64  `json:"process_interval_seconds"`
	Host                   string   `json:"host"`
	Port                   int      `json:"port"`
	OpenBrowser            bool     `json:"open_browser"`
}

func defaultConfig() Config {
	return Config{
		Targets: []string{"8.8.8.8", "1.1.1.1"}, IntervalSeconds: 1, TimeoutMs: 1000,
		BaselineWindow: 300, BaselineMinSamples: 20, DeviationFactor: 3, DeviationMinDeltaMs: 10,
		HighMs: 0, EventCloseAfter: 5, RetentionDays: 30,
		RangeBucketsMs:  []int{1, 2, 5, 10, 20, 50, 100, 250, 500},
		IncidentWindowS: 10, EventLogs: true,
		Metrics: true, MetricsIntervalSeconds: 1, MetricsRetentionDays: 7, ProcessIntervalSeconds: 5,
		Host: "0.0.0.0", Port: 5055, OpenBrowser: true,
	}
}

var (
	cfg      Config
	cfgMu    sync.Mutex
	dataDir  string
	cfgPath  string
	simulate = os.Getenv("LAGSLEUTH_SIMULATE") == "1" || os.Getenv("PINGWATCH_SIMULATE") == "1" // old name still accepted
	hostRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.\-:]{0,252}$`)
)

// pick a writable home: next to the exe (portable), else %LOCALAPPDATA%\LagSleuth
func resolveDataDir(override string) string {
	if override != "" {
		os.MkdirAll(override, 0o755)
		return override
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		test := filepath.Join(dir, ".lagsleuth_write_test")
		if f, err := os.Create(test); err == nil {
			f.Close()
			os.Remove(test)
			return dir
		}
	}
	base, err := os.UserCacheDir() // %LOCALAPPDATA% on Windows
	if err != nil {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "LagSleuth")
	if !isDir(dir) {
		// keep using the folder from before the rename (PingWatch) so config and history carry over
		if old := filepath.Join(base, "PingWatch"); isDir(old) {
			return old
		}
	}
	os.MkdirAll(dir, 0o755)
	return dir
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func loadConfig() {
	cfg = defaultConfig()
	b, err := os.ReadFile(cfgPath)
	if err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			log.Printf("config.json is not valid JSON (%v) - using defaults", err)
		}
	}
	saveConfig()
}

func saveConfig() {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	os.WriteFile(cfgPath, b, 0o644)
}

// ---------------------------------------------------------------------------- storage
// Samples are stored per target per day: data/<target>/<YYYY-MM-DD>.bin
// record = float64 unix ts | float32 rtt ms (NaN = timeout) | uint8 deviation  (13 bytes)

const recSize = 13

type Sample struct {
	TS  float64
	RTT float64 // NaN = timeout
	Dev bool
}

func (s Sample) timeout() bool { return math.IsNaN(s.RTT) }

func safeName(t string) string {
	return regexp.MustCompile(`[^A-Za-z0-9.\-]`).ReplaceAllString(t, "_")
}

func targetDir(t string) string { return filepath.Join(dataDir, "data", safeName(t)) }

type Store struct {
	mu    sync.Mutex
	files map[string]*os.File
	days  map[string]string
}

var store = &Store{files: map[string]*os.File{}, days: map[string]string{}}

func (s *Store) Append(target string, smp Sample) {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := time.Unix(int64(smp.TS), 0).Format("2006-01-02")
	f := s.files[target]
	if f == nil || s.days[target] != day {
		if f != nil {
			f.Close()
		}
		os.MkdirAll(targetDir(target), 0o755)
		var err error
		f, err = os.OpenFile(filepath.Join(targetDir(target), day+".bin"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			log.Printf("storage error: %v", err)
			return
		}
		s.files[target], s.days[target] = f, day
	}
	var buf [recSize]byte
	binary.LittleEndian.PutUint64(buf[0:], math.Float64bits(smp.TS))
	binary.LittleEndian.PutUint32(buf[8:], math.Float32bits(float32(smp.RTT)))
	if smp.Dev {
		buf[12] = 1
	}
	f.Write(buf[:])
}

func (s *Store) Close(target string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f := s.files[target]; f != nil {
		f.Close()
		delete(s.files, target)
	}
}

// Scan calls fn for every sample of target in [start, end], in time order.
// Records within a day file are time-ordered, so it binary-searches to the start
// and only reads the span it needs.
func Scan(target string, start, end float64, fn func(Sample)) {
	d := time.Unix(int64(start), 0)
	d = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.Local)
	last := time.Unix(int64(end), 0)
	var rec [recSize]byte
	for !d.After(last) {
		scanFile(filepath.Join(targetDir(target), d.Format("2006-01-02")+".bin"), start, end, rec[:], fn)
		d = d.AddDate(0, 0, 1)
	}
}

func recTS(f *os.File, i int64, buf []byte) float64 {
	if _, err := f.ReadAt(buf[:8], i*recSize); err != nil {
		return math.Inf(1)
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(buf))
}

func scanFile(path string, start, end float64, buf []byte, fn func(Sample)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	n := st.Size() / recSize
	lo, hi := int64(0), n
	for lo < hi { // first record with ts >= start
		mid := (lo + hi) / 2
		if recTS(f, mid, buf) < start {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	const chunk = 4096
	block := make([]byte, chunk*recSize)
	for i := lo; i < n; i += chunk {
		cnt := n - i
		if cnt > chunk {
			cnt = chunk
		}
		got, _ := f.ReadAt(block[:cnt*recSize], i*recSize)
		for j := 0; j+recSize <= got; j += recSize {
			r := block[j:]
			ts := math.Float64frombits(binary.LittleEndian.Uint64(r[0:]))
			if ts > end {
				return
			}
			if ts < start {
				continue
			}
			fn(Sample{TS: ts, RTT: float64(math.Float32frombits(binary.LittleEndian.Uint32(r[8:]))), Dev: r[12] == 1})
		}
	}
}

// ---------------------------------------------------------------------------- events

type Event struct {
	ID          int64    `json:"id"`
	Target      string   `json:"target"`
	StartTS     float64  `json:"start_ts"`
	EndTS       *float64 `json:"end_ts"`
	LastTS      float64  `json:"last_ts"`
	DevSamples  int      `json:"dev_samples"`
	Timeouts    int      `json:"timeouts"`
	PeakMs      *float64 `json:"peak_ms"`
	SumMs       float64  `json:"sum_ms"`
	BaselineMs  *float64 `json:"baseline_ms"`
	ThresholdMs *float64 `json:"threshold_ms"`
}

var (
	events      []*Event
	eventsMu    sync.Mutex
	eventsDirty bool
	nextEventID int64 = 1
)

func eventsPath() string { return filepath.Join(dataDir, "data", "events.json") }

func loadEvents() {
	b, err := os.ReadFile(eventsPath())
	if err != nil {
		return
	}
	json.Unmarshal(b, &events)
	for _, e := range events {
		if e.EndTS == nil { // left open by previous run: close at last deviation
			v := e.LastTS
			e.EndTS = &v
		}
		if e.ID >= nextEventID {
			nextEventID = e.ID + 1
		}
	}
}

func eventSaver() {
	for range time.Tick(2 * time.Second) {
		eventsMu.Lock()
		if !eventsDirty {
			eventsMu.Unlock()
			continue
		}
		b, _ := json.Marshal(events)
		eventsDirty = false
		eventsMu.Unlock()
		os.MkdirAll(filepath.Dir(eventsPath()), 0o755)
		tmp := eventsPath() + ".tmp"
		if os.WriteFile(tmp, b, 0o644) == nil {
			os.Rename(tmp, eventsPath())
		}
	}
}

func fptr(v float64) *float64 { return &v }

// ---------------------------------------------------------------------------- monitor

type Monitor struct {
	target       string
	stop         chan struct{}
	mu           sync.Mutex
	recent       []Sample
	base         []float64
	baseline     *float64
	threshold    *float64
	event        *Event
	normalStreak int
}

var (
	monitors   = map[string]*Monitor{}
	monitorsMu sync.Mutex
)

func newMonitor(t string) *Monitor {
	m := &Monitor{target: t, stop: make(chan struct{})}
	now := float64(time.Now().Unix())
	Scan(t, now-6*3600, now+1, func(s Sample) {
		m.recent = append(m.recent, s)
		if !s.timeout() {
			m.base = append(m.base, s.RTT)
		}
	})
	if len(m.recent) > 600 {
		m.recent = m.recent[len(m.recent)-600:]
	}
	if len(m.base) > cfg.BaselineWindow {
		m.base = m.base[len(m.base)-cfg.BaselineWindow:]
	}
	m.updateBaseline()
	if simulate && m.baseline != nil { // continue the simulation from the stored history's normal
		simMu.Lock()
		simBase[t] = *m.baseline
		simMu.Unlock()
	}
	return m
}

func (m *Monitor) updateBaseline() {
	if len(m.base) < cfg.BaselineMinSamples {
		m.baseline, m.threshold = nil, nil
		return
	}
	c := append([]float64(nil), m.base...)
	sort.Float64s(c)
	med := c[len(c)/2]
	if len(c)%2 == 0 {
		med = (c[len(c)/2-1] + c[len(c)/2]) / 2
	}
	th := math.Max(med*cfg.DeviationFactor, med+cfg.DeviationMinDeltaMs)
	if cfg.HighMs > 0 {
		th = math.Min(th, cfg.HighMs)
	}
	m.baseline, m.threshold = fptr(med), fptr(th)
}

func (m *Monitor) isDeviation(rtt float64) bool {
	if math.IsNaN(rtt) {
		return true
	}
	if cfg.HighMs > 0 && rtt >= cfg.HighMs {
		return true
	}
	return m.threshold != nil && rtt > *m.threshold
}

func (m *Monitor) run() {
	pinger := newPinger()
	defer pinger.Close()
	interval := time.Duration(cfg.IntervalSeconds * float64(time.Second))
	for {
		started := time.Now()
		var rtt float64
		if simulate {
			rtt = simulated(m.target)
		} else {
			rtt = pinger.Ping(m.target, cfg.TimeoutMs)
		}
		// timestamp = when the ping was sent, so a slow reply is filed at the moment the stall began
		m.record(float64(started.UnixNano())/1e9, rtt)
		wait := interval - time.Since(started)
		if wait < 0 {
			wait = 0
		}
		select {
		case <-m.stop:
			store.Close(m.target)
			return
		case <-time.After(wait):
		}
	}
}

func (m *Monitor) record(ts, rtt float64) {
	m.mu.Lock()
	dev := m.isDeviation(rtt)
	baseline, threshold := m.baseline, m.threshold
	m.recent = append(m.recent, Sample{ts, rtt, dev})
	if len(m.recent) > 600 {
		m.recent = m.recent[len(m.recent)-600:]
	}
	if !math.IsNaN(rtt) {
		m.base = append(m.base, rtt)
		if len(m.base) > cfg.BaselineWindow {
			m.base = m.base[len(m.base)-cfg.BaselineWindow:]
		}
		m.updateBaseline()
	}
	m.mu.Unlock()

	store.Append(m.target, Sample{ts, rtt, dev})

	eventsMu.Lock()
	defer eventsMu.Unlock()
	if dev {
		m.normalStreak = 0
		if m.event == nil {
			m.event = &Event{ID: nextEventID, Target: m.target, StartTS: ts, BaselineMs: baseline, ThresholdMs: threshold}
			nextEventID++
			events = append(events, m.event)
		}
		e := m.event
		e.LastTS = ts
		e.DevSamples++
		if math.IsNaN(rtt) {
			e.Timeouts++
		} else {
			e.SumMs += rtt
			if e.PeakMs == nil || rtt > *e.PeakMs {
				e.PeakMs = fptr(rtt)
			}
		}
		eventsDirty = true
	} else if m.event != nil {
		m.normalStreak++
		if m.normalStreak >= cfg.EventCloseAfter {
			m.event.EndTS = fptr(m.event.LastTS)
			m.event, m.normalStreak = nil, 0
			eventsDirty = true
		}
	}
}

type jsonSample struct {
	TS  float64  `json:"ts"`
	RTT *float64 `json:"rtt"`
	Dev bool     `json:"dev"`
}

func rttPtr(v float64) *float64 {
	if math.IsNaN(v) {
		return nil
	}
	return fptr(v)
}

func (m *Monitor) snapshot(n int) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec := m.recent
	if len(rec) > n {
		rec = rec[len(rec)-n:]
	}
	out := make([]jsonSample, len(rec))
	for i, s := range rec {
		out[i] = jsonSample{s.TS, rttPtr(s.RTT), s.Dev}
	}
	cut := float64(time.Now().Unix()) - 300
	var tot, lost int
	for _, s := range m.recent {
		if s.TS >= cut {
			tot++
			if s.timeout() {
				lost++
			}
		}
	}
	loss := 0.0
	if tot > 0 {
		loss = float64(lost) / float64(tot) * 100
	}
	var lastRTT, lastTS *float64
	if len(rec) > 0 {
		lastRTT, lastTS = rttPtr(rec[len(rec)-1].RTT), fptr(rec[len(rec)-1].TS)
	}
	return map[string]any{
		"target": m.target, "samples": out, "baseline": m.baseline, "threshold": m.threshold,
		"last_rtt": lastRTT, "last_ts": lastTS, "deviating": m.event != nil, "loss_5m": loss,
	}
}

func startMonitor(t string) {
	monitorsMu.Lock()
	defer monitorsMu.Unlock()
	if _, ok := monitors[t]; ok {
		return
	}
	m := newMonitor(t)
	monitors[t] = m
	go m.run()
}

func stopMonitor(t string) {
	monitorsMu.Lock()
	m := monitors[t]
	delete(monitors, t)
	monitorsMu.Unlock()
	if m != nil {
		close(m.stop)
		eventsMu.Lock()
		if m.event != nil {
			m.event.EndTS = fptr(m.event.LastTS)
			eventsDirty = true
		}
		eventsMu.Unlock()
	}
}

func targetList() []string {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return append([]string(nil), cfg.Targets...)
}

// reqTargets returns the targets named by ?target= (one, or several separated by commas) and whether
// the request asked for specific ones; with no ?target= it returns every configured target.
func reqTargets(r *http.Request) ([]string, bool) {
	var out []string
	seen := map[string]bool{}
	for _, t := range strings.Split(r.URL.Query().Get("target"), ",") {
		if t = strings.TrimSpace(t); t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return targetList(), false
	}
	return out, true
}

// ---------------------------------------------------------------------------- simulation (testing)

var simMu sync.Mutex
var simState = map[string]*struct {
	spike int
	base  float64
}{}

var simBase = map[string]float64{}

// simSharedStall: every 3 minutes simulated hosts stall together for ~15 s, as if they share one
// virtualisation host. Public addresses, gateways (.1) and the demo host 10.0.0.6 play "off-host"
// targets and stay clean.
func simSharedStall(t string) bool {
	return time.Now().Unix()%180 < 15 && logEligible(t) && !strings.HasSuffix(t, ".1") && t != "10.0.0.6"
}

func simulated(t string) float64 {
	simMu.Lock()
	defer simMu.Unlock()
	st := simState[t]
	if st == nil {
		st = &struct {
			spike int
			base  float64
		}{0, []float64{3, 8, 20}[rand.Intn(3)]}
		if b, ok := simBase[t]; ok {
			st.base = b
		}
		simState[t] = st
	}
	if simSharedStall(t) && st.base < 20 && rand.Float64() < 0.7 {
		return st.base*20 + rand.Float64()*300
	}
	if _, fromHistory := simBase[t]; fromHistory { // continuing stored history (demos): quiet apart from the shared stall
		if st.base < 1 {
			return []float64{0.5, 0.5, 0.5, 0.5, 1}[rand.Intn(5)]
		}
		return st.base + float64(rand.Intn(3))
	}
	if st.spike > 0 {
		st.spike--
		if rand.Float64() < 0.2 {
			return math.NaN()
		}
		return st.base * (8 + rand.Float64()*12)
	}
	if rand.Float64() < 0.004 {
		st.spike = 4 + rand.Intn(12)
	}
	r := rand.Float64()
	switch {
	case r < 0.80:
		return st.base + float64([]int{0, 0, 0, 1}[rand.Intn(4)])
	case r < 0.92:
		return st.base * 5
	case r < 0.995:
		return st.base * (1.5 + rand.Float64()*1.5)
	}
	return math.NaN()
}

// ---------------------------------------------------------------------------- stats

type hist struct {
	counts   map[int]int
	timeouts int
	ok       int
	devs     int
	sum      float64
	min, max float64
}

func buildHist(target string, start, end float64) *hist {
	h := &hist{counts: map[int]int{}, min: math.Inf(1), max: math.Inf(-1)}
	Scan(target, start, end, func(s Sample) {
		if s.Dev {
			h.devs++
		}
		if s.timeout() {
			h.timeouts++
			return
		}
		h.ok++
		h.sum += s.RTT
		h.min = math.Min(h.min, s.RTT)
		h.max = math.Max(h.max, s.RTT)
		h.counts[int(math.Round(s.RTT))]++
	})
	return h
}

func (h *hist) sortedKeys() []int {
	k := make([]int, 0, len(h.counts))
	for ms := range h.counts {
		k = append(k, ms)
	}
	sort.Ints(k)
	return k
}

func (h *hist) percentile(p float64) *float64 {
	if h.ok == 0 {
		return nil
	}
	need, acc := float64(h.ok)*p, 0
	keys := h.sortedKeys()
	for _, ms := range keys {
		acc += h.counts[ms]
		if float64(acc) >= need {
			return fptr(float64(ms))
		}
	}
	return fptr(float64(keys[len(keys)-1]))
}

func summary(target string, h *hist) map[string]any {
	total := h.ok + h.timeouts
	pct := func(n int) float64 {
		if total == 0 {
			return 0
		}
		return float64(n) / float64(total) * 100
	}
	var mn, avg, mx *float64
	if h.ok > 0 {
		mn, avg, mx = fptr(h.min), fptr(h.sum/float64(h.ok)), fptr(h.max)
	}
	return map[string]any{
		"target": target, "total": total, "ok": h.ok, "timeouts": h.timeouts, "loss_pct": pct(h.timeouts),
		"min": mn, "avg": avg, "max": mx,
		"p50": h.percentile(0.5), "p95": h.percentile(0.95), "p99": h.percentile(0.99),
		"deviation_samples": h.devs, "deviation_pct": pct(h.devs),
	}
}

type Slice struct {
	Label string  `json:"label"`
	Count int     `json:"count"`
	Order int64   `json:"order"`
	Kind  string  `json:"kind,omitempty"`
	Pct   float64 `json:"pct"`
}

func distribution(h *hist, mode string) map[string]any {
	total := h.ok + h.timeouts
	var slices []Slice
	if mode == "ranges" {
		edges := cfg.RangeBucketsMs
		counts := make([]int, len(edges)+1)
		for ms, n := range h.counts {
			i := len(edges)
			for j, e := range edges {
				if ms <= e {
					i = j
					break
				}
			}
			counts[i] += n
		}
		for i, n := range counts {
			if n == 0 {
				continue
			}
			var label string
			switch {
			case i == 0:
				label = fmt.Sprintf("≤%d ms", edges[0])
			case i < len(edges):
				label = fmt.Sprintf("%d–%d ms", edges[i-1], edges[i])
			default:
				label = fmt.Sprintf(">%d ms", edges[len(edges)-1])
			}
			slices = append(slices, Slice{Label: label, Count: n, Order: int64(i)})
		}
	} else {
		keys := h.sortedKeys()
		byCount := append([]int(nil), keys...)
		sort.SliceStable(byCount, func(a, b int) bool { return h.counts[byCount[a]] > h.counts[byCount[b]] })
		top := byCount
		other := 0
		if len(top) > 8 {
			for _, ms := range top[8:] {
				other += h.counts[ms]
			}
			top = top[:8]
		}
		sort.Ints(top)
		for _, ms := range top {
			label := fmt.Sprintf("%d ms", ms)
			if ms == 0 {
				label = "<1 ms"
			}
			slices = append(slices, Slice{Label: label, Count: h.counts[ms], Order: int64(ms)})
		}
		if other > 0 {
			slices = append(slices, Slice{Label: "Other", Count: other, Order: 1e9, Kind: "other"})
		}
	}
	if h.timeouts > 0 {
		slices = append(slices, Slice{Label: "Timeout", Count: h.timeouts, Order: 1e10, Kind: "timeout"})
	}
	for i := range slices {
		if total > 0 {
			slices[i].Pct = float64(slices[i].Count) / float64(total) * 100
		}
	}
	if slices == nil {
		slices = []Slice{}
	}
	return map[string]any{"total": total, "slices": slices}
}

type EventOut struct {
	ID          int64    `json:"id"`
	Target      string   `json:"target"`
	Active      bool     `json:"active"` // false once the target has been removed from monitoring
	StartTS     float64  `json:"start_ts"`
	EndTS       *float64 `json:"end_ts"`
	Ongoing     bool     `json:"ongoing"`
	DurationS   float64  `json:"duration_s"`
	DevSamples  int      `json:"dev_samples"`
	Timeouts    int      `json:"timeouts"`
	PeakMs      *float64 `json:"peak_ms"`
	AvgMs       *float64 `json:"avg_ms"`
	BaselineMs  *float64 `json:"baseline_ms"`
	ThresholdMs *float64 `json:"threshold_ms"`
}

func eventsIn(target string, start, end float64, limit int) []EventOut {
	active := map[string]bool{}
	for _, t := range targetList() {
		active[t] = true
	}
	eventsMu.Lock()
	defer eventsMu.Unlock()
	out := []EventOut{}
	for i := len(events) - 1; i >= 0 && len(out) < limit; i-- {
		e := events[i]
		if target != "" && e.Target != target {
			continue
		}
		stop := e.LastTS
		if e.EndTS != nil {
			stop = *e.EndTS
		}
		if e.StartTS > end || stop < start {
			continue
		}
		var avg *float64
		if okn := e.DevSamples - e.Timeouts; okn > 0 {
			avg = fptr(e.SumMs / float64(okn))
		}
		out = append(out, EventOut{e.ID, e.Target, active[e.Target], e.StartTS, e.EndTS, e.EndTS == nil, stop - e.StartTS,
			e.DevSamples, e.Timeouts, e.PeakMs, avg, e.BaselineMs, e.ThresholdMs})
	}
	return out
}

func housekeeping() {
	for {
		cutoff := time.Now().AddDate(0, 0, -cfg.RetentionDays)
		dirs, _ := os.ReadDir(filepath.Join(dataDir, "data"))
		for _, d := range dirs {
			if !d.IsDir() {
				continue
			}
			files, _ := os.ReadDir(filepath.Join(dataDir, "data", d.Name()))
			for _, f := range files {
				day, err := time.ParseInLocation("2006-01-02", strings.TrimSuffix(f.Name(), ".bin"), time.Local)
				if err == nil && day.Before(cutoff.AddDate(0, 0, -1)) {
					os.Remove(filepath.Join(dataDir, "data", d.Name(), f.Name()))
				}
			}
		}
		cut := float64(cutoff.Unix())
		eventsMu.Lock()
		kept := events[:0]
		for _, e := range events {
			if e.EndTS == nil || *e.EndTS >= cut {
				kept = append(kept, e)
			}
		}
		if len(kept) != len(events) {
			eventsDirty = true
		}
		events = kept
		eventsMu.Unlock()
		time.Sleep(time.Hour)
	}
}

// ---------------------------------------------------------------------------- history samples
// Raw samples for short ranges; for long ranges, buckets with min/avg/max so spikes stay visible.

var niceBuckets = []float64{1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 14400}

type bucket struct {
	TS  float64  `json:"ts"`
	Min *float64 `json:"min"`
	Avg *float64 `json:"avg"`
	Max *float64 `json:"max"`
	N   int      `json:"n"`
	Dev int      `json:"dev"`
	TO  int      `json:"to"`
}

func samplesAPI(target string, start, end float64, maxPts int) map[string]any {
	span := end - start
	size := 0.0
	if span/1.0 > float64(maxPts) { // assume ~1 sample/second
		for _, b := range niceBuckets {
			size = b
			if span/b <= float64(maxPts) {
				break
			}
		}
	}
	raw := []jsonSample{}
	var logTail []jsonSample
	var buckets []*bucket
	var cur *bucket
	var sum float64
	var ok int
	counts := map[int]int{}
	total, okAll := 0, 0
	flush := func() {
		if cur != nil {
			if ok > 0 {
				cur.Avg = fptr(sum / float64(ok))
			}
			buckets = append(buckets, cur)
		}
	}
	Scan(target, start, end, func(s Sample) {
		total++
		js := jsonSample{s.TS, rttPtr(s.RTT), s.Dev}
		logTail = append(logTail, js)
		if len(logTail) > 400 {
			logTail = logTail[200:]
		}
		if !s.timeout() {
			okAll++
			counts[int(math.Round(s.RTT))]++
		}
		if size == 0 {
			raw = append(raw, js)
			return
		}
		bts := math.Floor(s.TS/size) * size
		if cur == nil || cur.TS != bts {
			flush()
			cur, sum, ok = &bucket{TS: bts}, 0, 0
		}
		cur.N++
		if s.Dev {
			cur.Dev++
		}
		if s.timeout() {
			cur.TO++
			return
		}
		ok++
		sum += s.RTT
		if cur.Min == nil || s.RTT < *cur.Min {
			cur.Min = fptr(s.RTT)
		}
		if cur.Max == nil || s.RTT > *cur.Max {
			cur.Max = fptr(s.RTT)
		}
	})
	flush()
	if len(logTail) > 200 {
		logTail = logTail[len(logTail)-200:]
	}
	var median *float64
	if okAll > 0 {
		k := make([]int, 0, len(counts))
		for v := range counts {
			k = append(k, v)
		}
		sort.Ints(k)
		acc := 0
		for _, v := range k {
			acc += counts[v]
			if acc*2 >= okAll {
				median = fptr(math.Max(float64(v), 0.5))
				break
			}
		}
	}
	out := map[string]any{"target": target, "start": start, "end": end, "bucket_s": size, "total": total,
		"median": median, "log": logTail}
	if size == 0 {
		out["points"] = raw
	} else {
		if buckets == nil {
			buckets = []*bucket{}
		}
		out["points"] = buckets
	}
	return out
}

// ---------------------------------------------------------------------------- time pattern
// Folds samples onto minute-of-hour to expose things like "every hour on the hour".

type hourRow struct {
	TS  int64      `json:"ts"`
	Max []*float64 `json:"max"`
	Avg []*float64 `json:"avg"`
	N   []int      `json:"n"`
	Dev []int      `json:"dev"`
	TO  []int      `json:"to"`
}

type minuteStat struct {
	Minute     int      `json:"minute"`
	Total      int      `json:"total"`
	Dev        int      `json:"dev"`
	Timeouts   int      `json:"timeouts"`
	DevPct     float64  `json:"dev_pct"`
	Avg        *float64 `json:"avg"`
	Max        *float64 `json:"max"`
	AvgHourMax *float64 `json:"avg_hour_max"` // typical worst ping in this minute, averaged over hours
	HoursSeen  int      `json:"hours_seen"`
	HoursDev   int      `json:"hours_dev"`
	Flagged    bool     `json:"flagged"`
}

type finding struct {
	Status      string   `json:"status"` // pattern | none | insufficient
	Text        string   `json:"text"`
	Ranges      [][2]int `json:"ranges"`
	HoursWith   int      `json:"hours_with"`
	HoursTotal  int      `json:"hours_total"`
	SharePct    float64  `json:"share_pct"`
	ExpectedPct float64  `json:"expected_pct"`
	TypicalPeak *float64 `json:"typical_peak"`
	Normal      *float64 `json:"normal"`
}

func pattern(target string, start, end float64) map[string]any {
	type acc struct {
		n, dev, to, ok [60]int
		sum, max       [60]float64
	}
	hours := map[int64]*acc{}
	ms := map[int]int{}
	totalDev, totalOK := 0, 0
	Scan(target, start, end, func(s Sample) {
		t := time.Unix(int64(s.TS), 0)
		hourTS := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, time.Local).Unix()
		a := hours[hourTS]
		if a == nil {
			a = &acc{}
			hours[hourTS] = a
		}
		m := t.Minute()
		a.n[m]++
		if s.Dev {
			a.dev[m]++
			totalDev++
		}
		if s.timeout() {
			a.to[m]++
			return
		}
		a.ok[m]++
		totalOK++
		a.sum[m] += s.RTT
		if s.RTT > a.max[m] {
			a.max[m] = s.RTT
		}
		ms[int(math.Round(s.RTT))]++
	})

	keys := make([]int64, 0, len(hours))
	for k := range hours {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	rows := []hourRow{}
	var mins [60]minuteStat
	var hourMaxSum [60]float64
	var hourMaxN [60]int
	var sum [60]float64
	var ok [60]int
	for i := range mins {
		mins[i].Minute = i
	}
	for _, k := range keys {
		a := hours[k]
		row := hourRow{TS: k, Max: make([]*float64, 60), Avg: make([]*float64, 60)}
		for m := 0; m < 60; m++ {
			if a.n[m] == 0 {
				continue
			}
			ms := &mins[m]
			ms.Total += a.n[m]
			ms.Dev += a.dev[m]
			ms.Timeouts += a.to[m]
			ms.HoursSeen++
			if a.dev[m] > 0 {
				ms.HoursDev++
			}
			if a.ok[m] > 0 {
				row.Max[m] = fptr(a.max[m])
				row.Avg[m] = fptr(a.sum[m] / float64(a.ok[m]))
				sum[m] += a.sum[m]
				ok[m] += a.ok[m]
				hourMaxSum[m] += a.max[m]
				hourMaxN[m]++
				if ms.Max == nil || a.max[m] > *ms.Max {
					ms.Max = fptr(a.max[m])
				}
			}
		}
		row.N, row.Dev, row.TO = append([]int(nil), a.n[:]...), append([]int(nil), a.dev[:]...), append([]int(nil), a.to[:]...)
		rows = append(rows, row)
	}
	totalN := 0
	for m := range mins {
		totalN += mins[m].Total
		if mins[m].Total > 0 {
			mins[m].DevPct = float64(mins[m].Dev) / float64(mins[m].Total) * 100
		}
		if ok[m] > 0 {
			mins[m].Avg = fptr(sum[m] / float64(ok[m]))
		}
		if hourMaxN[m] > 0 {
			mins[m].AvgHourMax = fptr(hourMaxSum[m] / float64(hourMaxN[m]))
		}
	}

	// normal = median of all successful pings in the window
	var normal *float64
	if totalOK > 0 {
		k := make([]int, 0, len(ms))
		for v := range ms {
			k = append(k, v)
		}
		sort.Ints(k)
		acc := 0
		for _, v := range k {
			acc += ms[v]
			if acc*2 >= totalOK {
				normal = fptr(math.Max(float64(v), 0.5))
				break
			}
		}
	}

	f := finding{HoursTotal: len(rows), Ranges: [][2]int{}, Normal: normal}
	switch {
	case len(rows) < 4:
		f.Status = "insufficient"
		f.Text = "Need at least 4 hours of data to confirm an hourly pattern. Leave LagSleuth running and pick the 24h or 7d window."
	case totalDev == 0:
		f.Status = "none"
		f.Text = fmt.Sprintf("No deviations in %d hours of data, so there is no recurring slow period.", len(rows))
	default:
		overall := float64(totalDev) / float64(totalN)
		minHours := int(math.Max(3, math.Ceil(float64(len(rows))*0.4)))
		var flagged [60]bool
		for m := range mins {
			s := mins[m]
			rate := 0.0
			if s.Total > 0 {
				rate = float64(s.Dev) / float64(s.Total)
			}
			flagged[m] = rate >= math.Max(overall*3, 0.05) && s.HoursDev >= minHours
			mins[m].Flagged = flagged[m]
		}
		f.Ranges = circularRuns(flagged)
		if len(f.Ranges) == 0 {
			f.Status = "none"
			f.Text = fmt.Sprintf("No recurring minute-of-hour pattern across %d hours: the %d deviating pings are spread out rather than clustered at the same time each hour.", len(rows), totalDev)
			break
		}
		f.Status = "pattern"
		devIn, nFlag := 0, 0
		peakSum, peakN := 0.0, 0
		for m := range mins {
			if flagged[m] {
				nFlag++
				devIn += mins[m].Dev
				if mins[m].AvgHourMax != nil {
					peakSum += *mins[m].AvgHourMax
					peakN++
				}
			}
		}
		for _, row := range rows {
			for m := 0; m < 60; m++ {
				if flagged[m] && row.Dev[m] > 0 {
					f.HoursWith++
					break
				}
			}
		}
		f.SharePct = float64(devIn) / float64(totalDev) * 100
		f.ExpectedPct = float64(nFlag) / 60 * 100
		if peakN > 0 {
			f.TypicalPeak = fptr(peakSum / float64(peakN))
		}
		var parts []string
		for _, r := range f.Ranges {
			if r[0] == r[1] {
				parts = append(parts, fmt.Sprintf(":%02d", r[0]))
			} else {
				parts = append(parts, fmt.Sprintf(":%02d–:%02d", r[0], r[1]))
			}
		}
		f.Text = fmt.Sprintf("High ping recurs at %s past the hour. It was seen in %d of %d hours, and these %d minute(s) hold %.0f%% of all deviating pings (%.0f%% would be expected if random).",
			strings.Join(parts, " and "), f.HoursWith, f.HoursTotal, nFlag, f.SharePct, f.ExpectedPct)
		if f.TypicalPeak != nil && normal != nil {
			f.Text += fmt.Sprintf(" Typical peak %.0f ms vs normal %.0f ms.", *f.TypicalPeak, *normal)
		}
	}
	return map[string]any{"by_minute": mins[:], "hours": rows, "finding": f, "start": start, "end": end}
}

// circularRuns groups true values into [from,to] runs, joining 59 -> 0 across the hour boundary.
func circularRuns(b [60]bool) [][2]int {
	all := true
	for _, v := range b {
		all = all && v
	}
	if all {
		return [][2]int{{0, 59}}
	}
	startAt := 0
	for i := 0; i < 60; i++ { // begin scanning just after a false so wrapped runs stay whole
		if !b[i] {
			startAt = (i + 1) % 60
			break
		}
	}
	var runs [][2]int
	for k := 0; k < 60; k++ {
		i := (startAt + k) % 60
		if !b[i] {
			continue
		}
		if len(runs) > 0 && runs[len(runs)-1][1] == (i+59)%60 {
			runs[len(runs)-1][1] = i
		} else {
			runs = append(runs, [2]int{i, i})
		}
	}
	return runs
}

// ---------------------------------------------------------------------------- http

func window(r *http.Request) (float64, float64) {
	now := float64(time.Now().UnixNano()) / 1e9
	q := r.URL.Query()
	if f := q.Get("from"); f != "" {
		parse := func(s string) (float64, bool) {
			for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
				if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
					return float64(t.Unix()), true
				}
			}
			return 0, false
		}
		start, ok := parse(f)
		if ok {
			end := now
			if t := q.Get("to"); t != "" {
				if v, ok := parse(t); ok {
					end = v
				}
			}
			return start, end
		}
	}
	minutes, err := strconv.ParseFloat(q.Get("minutes"), 64)
	if err != nil || minutes <= 0 {
		minutes = 60
	}
	return now - minutes*60, now
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func fmtTS(ts float64) string { return time.Unix(int64(ts), 0).Format("2006-01-02 15:04:05") }

func num(p *float64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatFloat(math.Round(*p*10)/10, 'f', -1, 64)
}

func routes() *http.ServeMux {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web")
	page := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			b, _ := fs.ReadFile(sub, name)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(b)
		}
	}
	mux.HandleFunc("GET /{$}", page("index.html"))
	mux.HandleFunc("GET /report", page("report.html"))
	mux.Handle("GET /static/", http.FileServer(http.FS(sub)))

	mux.HandleFunc("GET /api/targets", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, targetList()) })
	incidentRoutes(mux)
	eventLogRoutes(mux)
	metricRoutes(mux)
	processRoutes(mux)

	mux.HandleFunc("POST /api/targets", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Target string `json:"target"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		hosts := strings.FieldsFunc(body.Target, func(c rune) bool { return c == ',' || c == ';' || c == ' ' || c == '\n' || c == '\r' || c == '\t' })
		var bad []string
		for _, h := range hosts {
			if !hostRE.MatchString(h) {
				bad = append(bad, h)
			}
		}
		if len(hosts) == 0 || len(bad) > 0 {
			if len(bad) == 0 {
				bad = []string{"(empty)"}
			}
			w.WriteHeader(400)
			writeJSON(w, map[string]string{"error": "Invalid host name or IP: " + strings.Join(bad, ", ")})
			return
		}
		cfgMu.Lock()
		for _, h := range hosts {
			found := false
			for _, t := range cfg.Targets {
				if t == h {
					found = true
				}
			}
			if !found {
				cfg.Targets = append(cfg.Targets, h)
			}
		}
		saveConfig()
		cfgMu.Unlock()
		for _, h := range hosts {
			startMonitor(h)
		}
		writeJSON(w, map[string]any{"ok": true, "added": hosts})
	})

	mux.HandleFunc("DELETE /api/targets/{t}", func(w http.ResponseWriter, r *http.Request) {
		t := r.PathValue("t")
		cfgMu.Lock()
		kept := cfg.Targets[:0]
		for _, x := range cfg.Targets {
			if x != t {
				kept = append(kept, x)
			}
		}
		cfg.Targets = kept
		saveConfig()
		cfgMu.Unlock()
		stopMonitor(t)
		writeJSON(w, map[string]bool{"ok": true})
	})

	mux.HandleFunc("GET /api/live", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.Atoi(r.URL.Query().Get("n"))
		if err != nil || n <= 0 {
			n = 120
		}
		out := map[string]any{}
		monitorsMu.Lock()
		ms := make([]*Monitor, 0, len(monitors))
		for _, m := range monitors {
			ms = append(ms, m)
		}
		monitorsMu.Unlock()
		for _, m := range ms {
			out[m.target] = m.snapshot(n)
		}
		writeJSON(w, out)
	})

	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) {
		t := r.URL.Query().Get("target")
		start, end := window(r)
		h := buildHist(t, start, end)
		writeJSON(w, map[string]any{"summary": summary(t, h), "distribution": distribution(h, r.URL.Query().Get("mode")),
			"start": start, "end": end})
	})

	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		start, end := window(r)
		writeJSON(w, eventsIn(r.URL.Query().Get("target"), start, end, 500))
	})

	mux.HandleFunc("GET /api/samples", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		start, _ := strconv.ParseFloat(q.Get("from"), 64)
		end, _ := strconv.ParseFloat(q.Get("to"), 64)
		now := float64(time.Now().UnixNano()) / 1e9
		if end <= 0 || end > now {
			end = now
		}
		if start <= 0 || start >= end {
			start = end - 120
		}
		maxPts, err := strconv.Atoi(q.Get("max"))
		if err != nil || maxPts < 100 {
			maxPts = 1500
		}
		writeJSON(w, samplesAPI(q.Get("target"), start, end, maxPts))
	})

	mux.HandleFunc("GET /api/pattern", func(w http.ResponseWriter, r *http.Request) {
		start, end := window(r)
		writeJSON(w, pattern(r.URL.Query().Get("target"), start, end))
	})

	mux.HandleFunc("GET /api/report", func(w http.ResponseWriter, r *http.Request) {
		start, end := window(r)
		mode := r.URL.Query().Get("mode")
		if mode == "" {
			mode = "ranges"
		}
		targets, picked := reqTargets(r)
		sections := []map[string]any{}
		for _, t := range targets {
			h := buildHist(t, start, end)
			sections = append(sections, map[string]any{
				"summary": summary(t, h), "distribution": distribution(h, mode), "events": eventsIn(t, start, end, 1000),
				"pattern": pattern(t, start, end)})
		}
		cfgMu.Lock()
		c := cfg
		cfgMu.Unlock()
		incs := buildIncidents(start, end)
		if picked {
			incs = incidentsFor(incs, targets)
		}
		var keep map[string]bool // chosen targets: incident rows show only these hosts
		if picked {
			keep = targetSet(targets)
		}
		multi := []Incident{}
		for _, inc := range incs {
			if len(inc.Hosts) >= 2 && len(multi) < 300 {
				s := logSummaryFor(inc, keep)
				inc.Logs = &s
				if picked {
					inc = focusIncident(inc, keep)
				}
				multi = append(multi, inc)
			}
		}
		writeJSON(w, map[string]any{"start": start, "end": end, "generated": float64(time.Now().Unix()),
			"mode": mode, "cfg": c, "sections": sections, "version": version, "picked": picked, "all_targets": targetList(),
			"incidents": map[string]any{"multi_host": multi, "total": len(incs), "finding": incidentPattern(incs, start, end)},
			"health":    healthAPI(start, end)})
	})

	mux.HandleFunc("GET /api/export/events.csv", func(w http.ResponseWriter, r *http.Request) {
		start, end := window(r)
		evs := eventsIn("", start, end, 1<<30)
		if targets, picked := reqTargets(r); picked {
			keep := map[string]bool{}
			for _, t := range targets {
				keep[t] = true
			}
			kept := evs[:0]
			for _, e := range evs {
				if keep[e.Target] {
					kept = append(kept, e)
				}
			}
			evs = kept
		}
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", "attachment; filename=lagsleuth_deviations.csv")
		cw := csv.NewWriter(w)
		cw.Write([]string{"target", "start", "end", "duration_s", "deviating_pings", "timeouts", "peak_ms", "avg_ms", "baseline_ms", "threshold_ms"})
		for i := len(evs) - 1; i >= 0; i-- {
			e := evs[i]
			endS := "ongoing"
			if e.EndTS != nil {
				endS = fmtTS(*e.EndTS)
			}
			cw.Write([]string{e.Target, fmtTS(e.StartTS), endS, strconv.FormatFloat(math.Round(e.DurationS*10)/10, 'f', -1, 64),
				strconv.Itoa(e.DevSamples), strconv.Itoa(e.Timeouts), num(e.PeakMs), num(e.AvgMs), num(e.BaselineMs), num(e.ThresholdMs)})
		}
		cw.Flush()
	})

	mux.HandleFunc("GET /api/export/samples.csv", func(w http.ResponseWriter, r *http.Request) {
		start, end := window(r)
		targets, _ := reqTargets(r)
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Disposition", "attachment; filename=lagsleuth_samples.csv")
		cw := csv.NewWriter(w)
		cw.Write([]string{"timestamp", "target", "rtt_ms", "deviation"})
		for _, t := range targets {
			Scan(t, start, end, func(s Sample) {
				rtt := ""
				if !s.timeout() {
					rtt = strconv.FormatFloat(s.RTT, 'f', -1, 32)
				}
				dev := "0"
				if s.Dev {
					dev = "1"
				}
				cw.Write([]string{fmtTS(s.TS), t, rtt, dev})
			})
		}
		cw.Flush()
	})
	return mux
}

// ---------------------------------------------------------------------------- main

func main() {
	dataFlag := flag.String("data", "", "folder for config.json and data (default: next to the exe, or %LOCALAPPDATA%\\LagSleuth)")
	portFlag := flag.Int("port", 0, "override web port")
	noBrowser := flag.Bool("no-browser", false, "don't open the browser on start")
	flag.Parse()

	dataDir = resolveDataDir(*dataFlag)
	cfgPath = filepath.Join(dataDir, "config.json")
	loadConfig()
	if *portFlag > 0 {
		cfg.Port = *portFlag
	}
	loadEvents()
	loadStalls()
	loadLogs()

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Printf("\nCould not start the web server on %s:\n  %v\n\nIs LagSleuth already running? Otherwise change \"port\" in %s\n", addr, err, cfgPath)
		pauseBeforeExit()
		os.Exit(1)
	}

	for _, t := range cfg.Targets {
		startMonitor(t)
	}
	go eventSaver()
	go housekeeping()
	go stallWatch()
	go logCollector()
	go metricsManager()
	go metricsHousekeeping()

	url := fmt.Sprintf("http://localhost:%d", cfg.Port)
	mode := "live"
	if simulate {
		mode = "SIMULATED"
	}
	fmt.Printf("LagSleuth %s  (%s pings)\n\n  Dashboard: %s\n  Data:      %s\n  Targets:   %s\n\nLeave this window open while monitoring. Close it (or Ctrl+C) to stop.\n",
		version, mode, url, dataDir, strings.Join(cfg.Targets, ", "))
	if cfg.OpenBrowser && !*noBrowser {
		openBrowser(url)
	}
	srv := &http.Server{Handler: routes(), ReadHeaderTimeout: 10 * time.Second}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
