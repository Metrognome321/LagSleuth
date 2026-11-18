package main

// Server metrics: CPU, memory and disk performance counters from this PC and each monitored Windows host,
// sampled every metrics_interval_seconds (default 1 s, changeable live from the dashboard) and stored next to
// the pings so they can be drawn on the same timeline and compared with incidents.

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Metric is one performance counter. Path is the English counter path without the machine prefix.
type Metric struct {
	Key   string  `json:"key"`
	Label string  `json:"label"`
	Unit  string  `json:"unit"`
	Path  string  `json:"-"`
	Scale float64 `json:"-"`   // multiply the raw value (e.g. seconds -> ms)
	Agg   string  `json:"agg"` // how a time slice is summarised: max (worst) or min (worst for "available")
}

// The order is the on-disk record layout: never reorder or remove, only append.
var metricDefs = []Metric{
	{"cpu", "CPU", "%", `\Processor(_Total)\% Processor Time`, 1, "max"},
	{"cpu_queue", "CPU queue", "threads", `\System\Processor Queue Length`, 1, "max"},
	{"cpu_stolen", "CPU stolen (VMware)", "ms", `\VM Processor(_Total)\CPU stolen time`, 1, "max"},
	{"mem_avail", "Memory available", "MB", `\Memory\Available MBytes`, 1, "min"},
	{"mem_commit", "Memory committed", "%", `\Memory\% Committed Bytes In Use`, 1, "max"},
	{"mem_pages", "Paging", "pages/s", `\Memory\Pages/sec`, 1, "max"},
	{"mem_balloon", "Memory ballooned (VMware)", "MB", `\VM Memory\Memory Ballooned in MB`, 1, "max"},
	{"disk_read", "Disk read latency", "ms", `\PhysicalDisk(_Total)\Avg. Disk sec/Read`, 1000, "max"},
	{"disk_write", "Disk write latency", "ms", `\PhysicalDisk(_Total)\Avg. Disk sec/Write`, 1000, "max"},
	{"disk_queue", "Disk queue", "requests", `\PhysicalDisk(_Total)\Current Disk Queue Length`, 1, "max"},
	{"disk_iops", "Disk I/O", "IOPS", `\PhysicalDisk(_Total)\Disk Transfers/sec`, 1, "max"},
}

const metricLocal = "local" // storage/API name for the machine PingWatch runs on

var metricRec = 8 + 4*len(metricDefs) // float64 ts + float32 per metric (NaN = not available)

// ---------------------------------------------------------------------------- storage

func metricDir(host string) string {
	if host == metricLocal {
		return filepath.Join(dataDir, "metrics", "_local")
	}
	return filepath.Join(dataDir, "metrics", safeName(host))
}

type metricStore struct {
	mu    sync.Mutex
	files map[string]*os.File
	days  map[string]string
}

var mstore = &metricStore{files: map[string]*os.File{}, days: map[string]string{}}

func (s *metricStore) append(host string, ts float64, vals []float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := time.Unix(int64(ts), 0).Format("2006-01-02")
	f := s.files[host]
	if f == nil || s.days[host] != day {
		if f != nil {
			f.Close()
		}
		os.MkdirAll(metricDir(host), 0o755)
		var err error
		f, err = os.OpenFile(filepath.Join(metricDir(host), day+".bin"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		s.files[host], s.days[host] = f, day
	}
	buf := make([]byte, metricRec)
	binary.LittleEndian.PutUint64(buf, math.Float64bits(ts))
	for i := range metricDefs {
		v := math.NaN()
		if i < len(vals) {
			v = vals[i]
		}
		binary.LittleEndian.PutUint32(buf[8+4*i:], math.Float32bits(float32(v)))
	}
	f.Write(buf)
}

func (s *metricStore) close(host string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f := s.files[host]; f != nil {
		f.Close()
		delete(s.files, host)
	}
}

// metricScan calls fn for each stored sample of host in [start, end], in time order.
func metricScan(host string, start, end float64, fn func(ts float64, vals []float64)) {
	d := time.Unix(int64(start), 0)
	d = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.Local)
	last := time.Unix(int64(end), 0)
	vals := make([]float64, len(metricDefs))
	for !d.After(last) {
		metricScanFile(filepath.Join(metricDir(host), d.Format("2006-01-02")+".bin"), start, end, vals, fn)
		d = d.AddDate(0, 0, 1)
	}
}

func metricScanFile(path string, start, end float64, vals []float64, fn func(float64, []float64)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return
	}
	rs := int64(metricRec)
	n := st.Size() / rs
	var tsb [8]byte
	tsAt := func(i int64) float64 {
		if _, err := f.ReadAt(tsb[:], i*rs); err != nil {
			return math.Inf(1)
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(tsb[:]))
	}
	lo, hi := int64(0), n
	for lo < hi {
		mid := (lo + hi) / 2
		if tsAt(mid) < start {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	const chunk = 2048
	block := make([]byte, chunk*rs)
	for i := lo; i < n; i += chunk {
		cnt := n - i
		if cnt > chunk {
			cnt = chunk
		}
		got, _ := f.ReadAt(block[:cnt*rs], i*rs)
		for j := 0; j+int(rs) <= got; j += int(rs) {
			r := block[j:]
			ts := math.Float64frombits(binary.LittleEndian.Uint64(r))
			if ts > end {
				return
			}
			if ts < start {
				continue
			}
			for k := range vals {
				vals[k] = float64(math.Float32frombits(binary.LittleEndian.Uint32(r[8+4*k:])))
			}
			fn(ts, vals)
		}
	}
}

func metricsHousekeeping() {
	for {
		cfgMu.Lock()
		days := cfg.MetricsRetentionDays
		cfgMu.Unlock()
		if days <= 0 {
			days = 7
		}
		cutoff := time.Now().AddDate(0, 0, -days-1)
		root := filepath.Join(dataDir, "metrics")
		dirs, _ := os.ReadDir(root)
		for _, d := range dirs {
			files, _ := os.ReadDir(filepath.Join(root, d.Name()))
			for _, f := range files {
				day, err := time.ParseInLocation("2006-01-02", strings.TrimSuffix(f.Name(), ".bin"), time.Local)
				if err == nil && day.Before(cutoff) {
					os.Remove(filepath.Join(root, d.Name(), f.Name()))
				}
			}
		}
		time.Sleep(time.Hour)
	}
}

// ---------------------------------------------------------------------------- collectors

type metricLive struct {
	Host   string    `json:"host"`
	Label  string    `json:"label"`
	Status string    `json:"status"` // ok | connecting | no_access | error | skipped
	Error  string    `json:"error,omitempty"`
	TS     float64   `json:"ts"`
	Values []float64 `json:"-"`
	Avail  []string  `json:"available"` // metric keys this host provides
}

type metricCollector struct {
	host string
	stop chan struct{}
	mu   sync.Mutex
	live metricLive
}

var (
	collectors   = map[string]*metricCollector{}
	collectorsMu sync.Mutex
)

func metricsEnabled() bool {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return cfg.Metrics
}

func metricsInterval() time.Duration {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	s := cfg.MetricsIntervalSeconds
	if s < 1 {
		s = 1
	}
	return time.Duration(s * float64(time.Second))
}

func metricLabel(host string) string {
	if host == metricLocal {
		if n, err := os.Hostname(); err == nil {
			return "This PC (" + n + ")"
		}
		return "This PC"
	}
	return host
}

// metricsManager keeps one collector running per eligible target, plus this PC.
func metricsManager() {
	for {
		want := map[string]bool{}
		if metricsEnabled() {
			want[metricLocal] = true
			for _, t := range targetList() {
				if logEligible(t) {
					want[t] = true
				}
			}
		}
		collectorsMu.Lock()
		for h, c := range collectors {
			if !want[h] {
				close(c.stop)
				delete(collectors, h)
			}
		}
		for h := range want {
			if collectors[h] == nil {
				c := &metricCollector{host: h, stop: make(chan struct{}), live: metricLive{Host: h, Label: metricLabel(h), Status: "connecting"}}
				collectors[h] = c
				go c.run()
			}
		}
		collectorsMu.Unlock()
		time.Sleep(5 * time.Second)
	}
}

func (c *metricCollector) set(f func(l *metricLive)) {
	c.mu.Lock()
	f(&c.live)
	c.mu.Unlock()
}

func (c *metricCollector) run() {
	defer mstore.close(c.host)
	for {
		src, err := openCounters(c.host)
		if err != nil {
			status := "error"
			if isAccessError(err) {
				status = "no_access"
			}
			c.set(func(l *metricLive) { l.Status, l.Error = status, err.Error() })
			select { // retry later; a host that refuses us should not be hammered
			case <-c.stop:
				return
			case <-time.After(time.Minute):
			}
			continue
		}
		c.set(func(l *metricLive) { l.Status, l.Error, l.Avail = "ok", "", src.available() })
		failures := 0
		next := time.Now()
		for {
			next = next.Add(metricsInterval())
			if d := time.Until(next); d > 0 {
				select {
				case <-c.stop:
					src.close()
					return
				case <-time.After(d):
				}
			} else {
				next = time.Now() // fell behind (slow host): do not try to catch up
			}
			ts := float64(time.Now().UnixNano()) / 1e9
			vals, err := src.collect()
			if err != nil {
				failures++
				c.set(func(l *metricLive) { l.Error = err.Error() })
				if failures >= 10 { // reconnect after ~10 failed samples in a row
					src.close()
					c.set(func(l *metricLive) { l.Status = "error" })
					break
				}
				continue
			}
			failures = 0
			mstore.append(c.host, ts, vals)
			c.set(func(l *metricLive) { l.TS, l.Values, l.Error, l.Status = ts, vals, "", "ok" })
		}
	}
}

// counterSource is a connected set of counters for one machine (PDH on Windows, simulated otherwise).
type counterSource interface {
	collect() ([]float64, error)
	available() []string
	close()
}

// ---------------------------------------------------------------------------- simulation

type simSource struct{ host string }

func (s *simSource) available() []string {
	keys := make([]string, len(metricDefs))
	for i, m := range metricDefs {
		keys[i] = m.Key
	}
	return keys
}
func (s *simSource) close() {}
func (s *simSource) collect() ([]float64, error) {
	stall := time.Now().Unix()%180 < 15 && s.host != "10.0.0.6"
	j := func(base, spread float64) float64 { return math.Max(0, base+(rand.Float64()-0.5)*spread) }
	v := []float64{j(12, 10), j(1, 2), 0, j(1900, 50), j(45, 2), j(5, 8), 0, j(0.6, 0.4), j(0.8, 0.5), j(0.2, 0.4), j(80, 60)}
	if stall {
		v[0], v[2], v[7], v[8], v[9], v[10] = j(25, 20), j(40, 40), j(300, 400), j(400, 500), j(12, 8), j(20, 20)
	}
	return v, nil
}

// ---------------------------------------------------------------------------- API

type metricPoint struct {
	TS float64    `json:"ts"`
	V  []*float64 `json:"v"` // aligned with the requested metrics; null = no data
}

func metricKeyIndex(keys []string) []int {
	idx := []int{}
	for _, k := range keys {
		for i, m := range metricDefs {
			if m.Key == k {
				idx = append(idx, i)
			}
		}
	}
	return idx
}

// metricSeries returns one host's samples in [start, end], bucketed so there are at most maxPts points.
// Each bucket keeps the worst value (max, or min for "available" metrics) so short spikes stay visible.
func metricSeries(host string, start, end float64, maxPts int, idx []int) (float64, []metricPoint) {
	interval := metricsInterval().Seconds()
	size := 0.0
	if (end-start)/interval > float64(maxPts) {
		for _, b := range niceBuckets {
			size = b
			if (end-start)/b <= float64(maxPts) {
				break
			}
		}
	}
	pts := []metricPoint{}
	var cur *metricPoint
	metricScan(host, start, end, func(ts float64, vals []float64) {
		bts := ts
		if size > 0 {
			bts = math.Floor(ts/size)*size + size/2
		}
		if cur == nil || cur.TS != bts {
			pts = append(pts, metricPoint{TS: bts, V: make([]*float64, len(idx))})
			cur = &pts[len(pts)-1]
		}
		for j, i := range idx {
			v := vals[i]
			if math.IsNaN(v) {
				continue
			}
			if cur.V[j] == nil {
				cur.V[j] = fptr(v)
			} else if metricDefs[i].Agg == "min" {
				*cur.V[j] = math.Min(*cur.V[j], v)
			} else {
				*cur.V[j] = math.Max(*cur.V[j], v)
			}
		}
	})
	return size, pts
}

// metricPeaks summarises a host's metrics over [start, end] (worst value of each), for incidents and reports.
func metricPeaks(host string, start, end float64) map[string]float64 {
	out := map[string]float64{}
	metricScan(host, start, end, func(ts float64, vals []float64) {
		for i, m := range metricDefs {
			v := vals[i]
			if math.IsNaN(v) {
				continue
			}
			old, ok := out[m.Key]
			switch {
			case !ok:
				out[m.Key] = v
			case m.Agg == "min":
				out[m.Key] = math.Min(old, v)
			default:
				out[m.Key] = math.Max(old, v)
			}
		}
	})
	if len(out) == 0 {
		return nil
	}
	for k, v := range out {
		out[k] = math.Round(v*10) / 10
	}
	return out
}

func liveMetrics() map[string]any {
	collectorsMu.Lock()
	cs := make([]*metricCollector, 0, len(collectors))
	for _, c := range collectors {
		cs = append(cs, c)
	}
	collectorsMu.Unlock()
	hosts := map[string]any{}
	for _, c := range cs {
		c.mu.Lock()
		l := c.live
		vals := map[string]*float64{}
		for i, m := range metricDefs {
			if i < len(l.Values) && !math.IsNaN(l.Values[i]) {
				vals[m.Key] = fptr(math.Round(l.Values[i]*100) / 100)
			}
		}
		c.mu.Unlock()
		hosts[c.host] = map[string]any{"host": l.Host, "label": l.Label, "status": l.Status, "error": l.Error, "ts": l.TS,
			"values": vals, "available": l.Avail}
	}
	return hosts
}

func metricRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/metrics/defs", func(w http.ResponseWriter, r *http.Request) {
		cfgMu.Lock()
		on, iv := cfg.Metrics, cfg.MetricsIntervalSeconds
		cfgMu.Unlock()
		writeJSON(w, map[string]any{"metrics": metricDefs, "enabled": on, "interval_seconds": iv})
	})
	mux.HandleFunc("GET /api/metrics/live", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, liveMetrics()) })

	// GET /api/metrics?from=&to=&max=&metrics=cpu,disk_read -> series for every host with data
	mux.HandleFunc("GET /api/metrics", func(w http.ResponseWriter, r *http.Request) {
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
		if err != nil || maxPts < 50 {
			maxPts = 800
		}
		keys := strings.Split(q.Get("metrics"), ",")
		idx := metricKeyIndex(keys)
		valid := make([]string, len(idx))
		for j, i := range idx {
			valid[j] = metricDefs[i].Key
		}
		writeJSON(w, map[string]any{"start": start, "end": end, "metrics": valid, "hosts": allMetricSeries(start, end, maxPts, idx)})
	})

	// POST /api/settings {"metrics_interval_seconds": 5, "metrics": true} -> change live, saved to config.json
	mux.HandleFunc("POST /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Interval *float64 `json:"metrics_interval_seconds"`
			Metrics  *bool    `json:"metrics"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		cfgMu.Lock()
		if body.Interval != nil {
			cfg.MetricsIntervalSeconds = math.Min(math.Max(*body.Interval, 1), 300)
		}
		if body.Metrics != nil {
			cfg.Metrics = *body.Metrics
		}
		saveConfig()
		iv, on := cfg.MetricsIntervalSeconds, cfg.Metrics
		cfgMu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "metrics_interval_seconds": iv, "metrics": on})
	})
}

type hostSeries struct {
	Label   string        `json:"label"`
	BucketS float64       `json:"bucket_s"`
	Points  []metricPoint `json:"points"`
}

// allMetricSeries returns series for this PC and every host that has metric files.
func allMetricSeries(start, end float64, maxPts int, idx []int) map[string]hostSeries {
	hosts := []string{metricLocal}
	for _, t := range targetList() {
		if logEligible(t) {
			hosts = append(hosts, t)
		}
	}
	sort.Strings(hosts[1:])
	out := map[string]hostSeries{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, h := range hosts {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			size, pts := metricSeries(h, start, end, maxPts, idx)
			if len(pts) == 0 {
				return
			}
			mu.Lock()
			out[h] = hostSeries{Label: metricLabel(h), BucketS: size, Points: pts}
			mu.Unlock()
		}(h)
	}
	wg.Wait()
	return out
}
