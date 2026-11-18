package main

import (
	"math"
	"os"
	"strings"
	"testing"
	"time"
)

// setupSynthetic writes 6 hours of samples for four targets and an hourly incident at :00 that hits
// a, b and c (d stays normal), plus one single-host blip on a at :20.
func setupSynthetic(t *testing.T) (start, end float64) {
	t.Helper()
	dataDir = t.TempDir()
	cfg = defaultConfig()
	cfg.Targets = []string{"a", "b", "c", "d"}
	store = &Store{files: map[string]*os.File{}, days: map[string]string{}}
	events, nextEventID = nil, 1

	now := time.Now()
	h0 := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, time.Local).Add(-6 * time.Hour)
	start, end = float64(h0.Unix()), float64(h0.Add(6*time.Hour).Unix())
	for _, tg := range cfg.Targets {
		for ts := start; ts < end; ts += 10 {
			store.Append(tg, Sample{TS: ts, RTT: 1})
		}
		store.Close(tg)
	}
	add := func(target string, s, d, peak float64, timeouts int) {
		e := &Event{ID: nextEventID, Target: target, StartTS: s, LastTS: s + d, DevSamples: int(d) + 1, Timeouts: timeouts,
			PeakMs: fptr(peak), SumMs: peak * float64(int(d)+1-timeouts)}
		e.EndTS = fptr(s + d)
		nextEventID++
		events = append(events, e)
	}
	for h := 1; h <= 5; h++ {
		top := start + float64(h*3600)
		add("a", top+1, 20, 400, 0)
		add("b", top+2, 25, 600, 1)
		add("c", top+3, 18, 300, 0)
		add("c", top+30, 5, 150, 0) // second burst on c, still the same incident
	}
	add("a", start+float64(2*3600+20*60), 0, 90, 0) // single-host blip
	return start, end
}

func TestBuildIncidentsGroupsHosts(t *testing.T) {
	start, end := setupSynthetic(t)
	incs := buildIncidents(start, end)
	multi := 0
	for _, inc := range incs {
		if len(inc.Hosts) < 2 {
			continue
		}
		multi++
		if len(inc.Hosts) != 3 {
			t.Fatalf("want 3 hosts per hourly incident, got %d", len(inc.Hosts))
		}
		if inc.Hosts[0].Target != "b" || *inc.PeakMs != 600 || inc.PeakTarget != "b" {
			t.Errorf("worst host should be b at 600 ms, got %s %v", inc.Hosts[0].Target, *inc.PeakMs)
		}
		if len(inc.Clean) != 1 || inc.Clean[0] != "d" || inc.Monitored != 4 {
			t.Errorf("want d clean of 4 monitored, got %v of %d", inc.Clean, inc.Monitored)
		}
		if inc.OnsetSpreadS != 2 {
			t.Errorf("want onset spread 2 s, got %v", inc.OnsetSpreadS)
		}
		for _, h := range inc.Hosts {
			if h.Target == "c" && h.DevSamples != 25 {
				t.Errorf("c's two bursts should merge to 25 deviating pings, got %d", h.DevSamples)
			}
		}
		if inc.Timeouts != 1 {
			t.Errorf("want 1 timeout, got %d", inc.Timeouts)
		}
	}
	if multi != 5 || len(incs) != 6 {
		t.Fatalf("want 5 multi-host incidents and 6 total, got %d and %d", multi, len(incs))
	}
	if incs[0].StartTS < incs[len(incs)-1].StartTS {
		t.Error("incidents should be newest first")
	}
}

func TestIncidentPatternFindsTopOfHour(t *testing.T) {
	start, end := setupSynthetic(t)
	f := incidentPattern(buildIncidents(start, end), start, end)
	if f.Status != "pattern" || f.Range == nil || f.Range[0] != 0 || f.Range[1] != 0 {
		t.Fatalf("want pattern at :00, got %s %v: %s", f.Status, f.Range, f.Text)
	}
	if f.HoursWith != 5 || f.HoursObserved != 6 || f.TypicalHosts != 3 || *f.WorstMs != 600 {
		t.Errorf("unexpected finding: %+v", f)
	}
}

func TestIncidentPatternInsufficient(t *testing.T) {
	start, _ := setupSynthetic(t)
	f := incidentPattern(buildIncidents(start, start+2*3600), start, start+2*3600)
	if f.Status != "insufficient" {
		t.Fatalf("want insufficient with 2 hours, got %s", f.Status)
	}
}

func TestParseEvents(t *testing.T) {
	out := []byte("<Events>\r\n<Event xmlns='http://schemas.microsoft.com/win/2004/08/events/event'><System><Provider Name='VSS'/><EventID Qualifiers='0'>8224</EventID><Level>4</Level><TimeCreated SystemTime='2026-11-18T12:00:01.5000000Z'/><Channel>Application</Channel><Computer>BED01</Computer></System><EventData><Data>x</Data></EventData><RenderingInfo Culture='en-GB'><Message>The VSS service is shutting down.</Message></RenderingInfo></Event>\r\n" +
		"<Event xmlns='http://schemas.microsoft.com/win/2004/08/events/event'><System><Provider Name='disk'/><EventID>153</EventID><Level>3</Level><TimeCreated SystemTime='2026-11-18T12:00:02Z'/><Channel>System</Channel><Computer>BED01</Computer></System><EventData><Data>a</Data><Data>b</Data></EventData></Event>\r\n</Events>")
	evs, err := parseEvents(out)
	if err != nil || len(evs) != 2 {
		t.Fatalf("parse: %v, %d events", err, len(evs))
	}
	e := evs[0]
	if e.Provider != "VSS" || e.ID != 8224 || e.Level != 4 || e.Channel != "Application" || e.Message != "The VSS service is shutting down." {
		t.Errorf("event 0 wrong: %+v", e)
	}
	if want := float64(time.Date(2026, 11, 18, 12, 0, 1, 5e8, time.UTC).UnixNano()) / 1e9; e.TS != want {
		t.Errorf("ts %v want %v", e.TS, want)
	}
	if evs[1].Message != "a · b" {
		t.Errorf("fallback message wrong: %q", evs[1].Message)
	}
	if q := logQuery(logChannels[0], 0, 1); !strings.Contains(q, "@SystemTime>='1970-01-01T00:00:00.000Z'") {
		t.Errorf("query: %s", q)
	}
	if logEligible("8.8.8.8") || !logEligible("10.20.30.40") || !logEligible("fileserver01") {
		t.Error("logEligible wrong")
	}
}

func TestMetricStoreSeriesAndPeaks(t *testing.T) {
	dataDir = t.TempDir()
	cfg = defaultConfig()
	mstore = &metricStore{files: map[string]*os.File{}, days: map[string]string{}}
	now := time.Now()
	base := float64(time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, time.Local).Add(-time.Hour).Unix())
	n := len(metricDefs)
	for s := 0; s < 600; s++ {
		v := make([]float64, n)
		for i := range v {
			v[i] = 1
		}
		v[2] = math.NaN()        // no VMware counter
		v[3] = 2000              // memory available
		if s >= 100 && s < 110 { // a 10 s stall
			v[7], v[3] = 500, 900
		}
		mstore.append("h1", base+float64(s), v)
	}
	mstore.close("h1")
	p := metricPeaks("h1", base+95, base+115)
	if p["disk_read"] != 500 || p["mem_avail"] != 900 || p["cpu"] != 1 {
		t.Fatalf("peaks wrong: %v", p)
	}
	if _, ok := p["cpu_stolen"]; ok {
		t.Error("NaN metric should be absent from peaks")
	}
	size, pts := metricSeries("h1", base, base+600, 60, metricKeyIndex([]string{"disk_read", "mem_avail"}))
	if size != 10 || len(pts) != 60 {
		t.Fatalf("want 60 buckets of 10 s, got %d of %v", len(pts), size)
	}
	if *pts[10].V[0] != 500 || *pts[10].V[1] != 900 || *pts[9].V[0] != 1 {
		t.Errorf("bucket should keep worst values: %v %v / %v", *pts[10].V[0], *pts[10].V[1], *pts[9].V[0])
	}
}

func TestProcStoreAndSummary(t *testing.T) {
	dataDir = t.TempDir()
	cfg = defaultConfig()
	now := time.Now()
	base := float64(time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, time.Local).Add(-time.Hour).Unix())
	for s := 0; s < 720; s++ { // one hour every 5 s
		ts := base + float64(s*5)
		ps := []ProcSample{{"sqlservr", 5, 3000}, {"svchost", 1, 50}}
		if s >= 100 && s < 104 {
			ps = []ProcSample{{"VSSVC", 40, 6}, {"sqlservr", 20, 3100}, {"svchost", 1, 50}}
		}
		storeProcs("h", ts, ps)
	}
	got, n := procSummary("h", base+490, base+530)
	if n != 9 || got[0].Name != "VSSVC" || got[0].MaxCPU != 40 || got[1].Name != "sqlservr" || got[1].MaxCPU != 20 || got[1].MaxMem != 3100 {
		t.Fatalf("summary wrong (n=%d): %+v", n, got)
	}
	all, n := procSummary("h", base, base+3600)
	if n != 720 || all[0].Name != "VSSVC" || all[0].Seen != 4 {
		t.Fatalf("full-hour summary wrong (n=%d): %+v", n, all[:2])
	}
	tp := topProcesses(map[string]float64{"_Total": 400, "Idle": 300, "a": 80, "b": 20}, map[string]float64{"a": 1048576 * 10})
	if len(tp) != 2 || tp[0].Name != "a" || tp[0].CPU != 20 || tp[0].MemMB != 10 || tp[1].CPU != 5 {
		t.Fatalf("topProcesses wrong: %+v", tp)
	}
}
