package main

// Top processes: which applications use the CPU (and memory) on each server. Sampled every
// process_interval_seconds (default 5 s) alongside the server metrics; the top 10 by CPU are kept per sample
// so you can look back at what was busy during an incident.

import (
	"bufio"
	"encoding/json"
	"io"
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

const procKeep = 10 // processes stored per sample

// ProcSample is one process at one moment. CPU is a share of the whole machine (0-100 %), like Task Manager.
type ProcSample struct {
	Name  string  `json:"name"`
	CPU   float64 `json:"cpu"`
	MemMB float64 `json:"mem_mb"`
}

// topProcesses turns raw "\Process(*)" counter values into the busiest processes. The raw % Processor Time
// of a process is per core, so it is divided by _Total (all processes incl. Idle = 100 % x cores).
func topProcesses(cpu, mem map[string]float64) []ProcSample {
	total := cpu["_Total"]
	if total <= 0 {
		total = 100
	}
	out := make([]ProcSample, 0, len(cpu))
	for name, v := range cpu {
		if name == "_Total" || name == "Idle" {
			continue
		}
		out = append(out, ProcSample{Name: name, CPU: math.Round(v/total*1000) / 10, MemMB: math.Round(mem[name]/1048576*10) / 10})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CPU != out[j].CPU {
			return out[i].CPU > out[j].CPU
		}
		return out[i].MemMB > out[j].MemMB
	})
	if len(out) > procKeep {
		out = out[:procKeep]
	}
	return out
}

func procInterval() time.Duration {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	s := cfg.ProcessIntervalSeconds
	if s < 2 {
		s = 2
	}
	return time.Duration(s * float64(time.Second))
}

// ---------------------------------------------------------------------------- storage (JSON lines per host per day)

func procDir(host string) string {
	if host == metricLocal {
		return filepath.Join(dataDir, "processes", "_local")
	}
	return filepath.Join(dataDir, "processes", safeName(host))
}

// each line: {"t": unix time, "p": [[name, cpu %, memory MB], ...]}
var procFileMu sync.Mutex

func storeProcs(host string, ts float64, ps []ProcSample) {
	rows := make([][]any, len(ps))
	for i, p := range ps {
		rows[i] = []any{p.Name, p.CPU, p.MemMB}
	}
	b, _ := json.Marshal(map[string]any{"t": math.Round(ts*10) / 10, "p": rows})
	procFileMu.Lock()
	defer procFileMu.Unlock()
	os.MkdirAll(procDir(host), 0o755)
	f, err := os.OpenFile(filepath.Join(procDir(host), time.Unix(int64(ts), 0).Format("2006-01-02")+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	f.Write(append(b, '\n'))
	f.Close()
}

func decodeProcLine(line []byte) (float64, []ProcSample, bool) {
	var l struct {
		T float64 `json:"t"`
		P [][]any `json:"p"`
	}
	if json.Unmarshal(line, &l) != nil {
		return 0, nil, false
	}
	ps := make([]ProcSample, 0, len(l.P))
	for _, r := range l.P {
		if len(r) < 3 {
			continue
		}
		name, _ := r[0].(string)
		cpu, _ := r[1].(float64)
		mem, _ := r[2].(float64)
		ps = append(ps, ProcSample{name, cpu, mem})
	}
	return l.T, ps, true
}

// procScan calls fn for every stored sample of host in [start, end], in time order. Day files are sorted by
// time, so it bisects on byte offsets to the start instead of reading the whole day.
func procScan(host string, start, end float64, fn func(ts float64, ps []ProcSample)) {
	d := time.Unix(int64(start), 0)
	d = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.Local)
	last := time.Unix(int64(end), 0)
	for !d.After(last) {
		procScanFile(filepath.Join(procDir(host), d.Format("2006-01-02")+".jsonl"), start, end, fn)
		d = d.AddDate(0, 0, 1)
	}
}

func procScanFile(path string, start, end float64, fn func(float64, []ProcSample)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	// timestamp of the first complete line at or after byte offset off
	tsAt := func(off int64) (float64, int64) {
		f.Seek(off, io.SeekStart)
		r := bufio.NewReader(f)
		pos := off
		if off > 0 {
			skipped, err := r.ReadBytes('\n')
			if err != nil {
				return math.Inf(1), st.Size()
			}
			pos += int64(len(skipped))
		}
		line, err := r.ReadBytes('\n')
		if err != nil && len(line) == 0 {
			return math.Inf(1), st.Size()
		}
		ts, _, ok := decodeProcLine(line)
		if !ok {
			return math.Inf(1), st.Size()
		}
		return ts, pos
	}
	lo, hi := int64(0), st.Size()
	for hi-lo > 4096 {
		mid := (lo + hi) / 2
		if ts, _ := tsAt(mid); ts < start {
			lo = mid
		} else {
			hi = mid
		}
	}
	_, pos := tsAt(lo)
	f.Seek(pos, io.SeekStart)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		ts, ps, ok := decodeProcLine(sc.Bytes())
		if !ok {
			continue
		}
		if ts > end {
			return
		}
		if ts >= start {
			fn(ts, ps)
		}
	}
}

// ---------------------------------------------------------------------------- summaries

type ProcSummary struct {
	Name   string  `json:"name"`
	MaxCPU float64 `json:"max_cpu"`
	AvgCPU float64 `json:"avg_cpu"` // average over the samples in the window (0 when not in the top 10)
	MaxMem float64 `json:"max_mem_mb"`
	Seen   int     `json:"seen"` // samples in which it was among the top 10
}

// procSummary ranks the processes of host over [start, end] by peak CPU (then average).
func procSummary(host string, start, end float64) ([]ProcSummary, int) {
	acc := map[string]*ProcSummary{}
	sum := map[string]float64{}
	n := 0
	procScan(host, start, end, func(ts float64, ps []ProcSample) {
		n++
		for _, p := range ps {
			s := acc[p.Name]
			if s == nil {
				s = &ProcSummary{Name: p.Name}
				acc[p.Name] = s
			}
			s.Seen++
			s.MaxCPU = math.Max(s.MaxCPU, p.CPU)
			s.MaxMem = math.Max(s.MaxMem, p.MemMB)
			sum[p.Name] += p.CPU
		}
	})
	out := make([]ProcSummary, 0, len(acc))
	for name, s := range acc {
		if n > 0 {
			s.AvgCPU = math.Round(sum[name]/float64(n)*10) / 10
		}
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].MaxCPU != out[j].MaxCPU {
			return out[i].MaxCPU > out[j].MaxCPU
		}
		return out[i].AvgCPU > out[j].AvgCPU
	})
	return out, n
}

// topProcsDuring returns the 3 busiest processes of host during an incident, for the incidents table.
func topProcsDuring(host string, start, end float64) []ProcSummary {
	// process samples are 5 s apart: widen a short incident so it catches at least one sample
	if end-start < 10 {
		start, end = start-5, end+5
	}
	s, _ := procSummary(host, start, end)
	if len(s) > 3 {
		s = s[:3]
	}
	if len(s) == 0 {
		return nil
	}
	return s
}

// ---------------------------------------------------------------------------- API

func processRoutes(mux *http.ServeMux) {
	// GET /api/processes?host=&from=&to=&max= -> ranking for the window plus a CPU series for the top 8
	mux.HandleFunc("GET /api/processes", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		host := q.Get("host")
		if host == "" {
			host = metricLocal
		}
		start, _ := strconv.ParseFloat(q.Get("from"), 64)
		end, _ := strconv.ParseFloat(q.Get("to"), 64)
		now := float64(time.Now().UnixNano()) / 1e9
		if end <= 0 || end > now {
			end = now
		}
		if start <= 0 || start >= end {
			start = end - 300
		}
		maxPts, err := strconv.Atoi(q.Get("max"))
		if err != nil || maxPts < 50 {
			maxPts = 600
		}
		ranking, samples := procSummary(host, start, end)
		top := []string{}
		for i := 0; i < len(ranking) && i < 8; i++ {
			top = append(top, ranking[i].Name)
		}
		idx := map[string]int{}
		for i, n := range top {
			idx[n] = i
		}
		// bucket so long windows stay light; each bucket keeps a process's highest CPU
		size := 0.0
		if (end-start)/procInterval().Seconds() > float64(maxPts) {
			for _, b := range niceBuckets {
				size = b
				if (end-start)/b <= float64(maxPts) {
					break
				}
			}
		}
		type pt struct {
			TS float64    `json:"ts"`
			V  []*float64 `json:"v"`
		}
		pts := []pt{}
		procScan(host, start, end, func(ts float64, ps []ProcSample) {
			bts := ts
			if size > 0 {
				bts = math.Floor(ts/size)*size + size/2
			}
			if len(pts) == 0 || pts[len(pts)-1].TS != bts {
				v := make([]*float64, len(top))
				for i := range v {
					v[i] = fptr(0) // not in this sample's top 10 = (close to) idle
				}
				pts = append(pts, pt{TS: bts, V: v})
			}
			cur := pts[len(pts)-1].V
			for _, p := range ps {
				if i, ok := idx[p.Name]; ok && p.CPU > *cur[i] {
					*cur[i] = p.CPU
				}
			}
		})
		if len(ranking) > 25 {
			ranking = ranking[:25]
		}
		writeJSON(w, map[string]any{"host": host, "label": metricLabel(host), "start": start, "end": end, "samples": samples,
			"interval_s": procInterval().Seconds(), "ranking": ranking, "series": map[string]any{"names": top, "bucket_s": size, "points": pts}})
	})
}

// ---------------------------------------------------------------------------- simulation

func (s *simSource) processes() ([]ProcSample, error) {
	stall := s.host != metricLocal && simSharedStall(s.host)
	j := func(b, sp float64) float64 { return math.Max(0, math.Round((b+(rand.Float64()-0.5)*sp)*10)/10) }
	cpu := map[string]float64{"_Total": 100, "System": j(1, 1), "svchost": j(1.5, 2), "svchost#1": j(0.6, 1), "MsMpEng": j(0.8, 1.5),
		"vmtoolsd": j(0.3, 0.4), "explorer": j(0.2, 0.3), "w3wp": j(3, 4), "sqlservr": j(4, 5), "LagSleuth": j(0.4, 0.4), "lsass": j(0.2, 0.3), "VSSVC": 0}
	if strings.HasSuffix(s.host, ".13") {
		cpu["sqlservr"] = j(18, 12)
	}
	if stall {
		cpu["VSSVC"], cpu["vmtoolsd"], cpu["sqlservr"], cpu["System"] = j(14, 8), j(9, 6), j(22, 14), j(6, 4)
	}
	mem := map[string]float64{"System": 0.2, "svchost": 60, "svchost#1": 25, "MsMpEng": 210, "vmtoolsd": 28, "explorer": 70, "w3wp": 480,
		"sqlservr": 3100, "LagSleuth": 22, "lsass": 18, "VSSVC": 6}
	for k, v := range mem {
		mem[k] = v * 1048576
	}
	return topProcesses(cpu, mem), nil
}
