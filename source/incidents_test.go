package main

import (
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
	if logEligible("8.8.8.8") || !logEligible("172.17.220.98") || !logEligible("fileserver01") {
		t.Error("logEligible wrong")
	}
}
