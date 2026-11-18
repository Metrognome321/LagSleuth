PingWatch 1.3 - multi-host ping monitor
=======================================

Runs on any Windows 10 / 11 or Windows Server 2016+ machine.
No install, no Python, no admin rights needed.

QUICK START
  1. Copy this folder anywhere (desktop, USB stick, a server share copied locally).
  2. Double-click PingWatch.exe.
     It opens the dashboard in your browser at http://localhost:5055.
  3. Add the hosts / IPs to watch. You can paste a list separated by commas or spaces.
  Keep the black console window open while monitoring. Closing it stops PingWatch.

WHICH EXE?
  PingWatch.exe                      normal 64-bit Windows (almost every PC/server)
  other-builds\PingWatch-32bit.exe   old 32-bit Windows 10
  other-builds\PingWatch-ARM64.exe   ARM laptops (Surface Pro X / Snapdragon)

FIRST RUN PROMPTS
  - "Windows protected your PC" (SmartScreen): the exe isn't code-signed.
    Click More info > Run anyway.
  - Windows Firewall may ask to allow network access. This only matters if you
    want to open the dashboard from another PC (http://<this-pc>:5055).
    To keep it local only, set "host": "127.0.0.1" in config.json.

WHERE THINGS ARE SAVED
  config.json and a data\ folder are created next to PingWatch.exe.
  If that folder is read-only (e.g. Program Files), they go to
  %LOCALAPPDATA%\PingWatch instead. The console window shows which.
  Samples are kept for 30 days (retention_days).

COMMAND LINE OPTIONS
  PingWatch.exe -port 8080         use a different port
  PingWatch.exe -no-browser        don't open the browser (e.g. when started at logon)
  PingWatch.exe -data D:\pingdata  keep config + data somewhere else

  To start it automatically at logon, put a shortcut to
  "PingWatch.exe -no-browser" in shell:startup.

PING GRAPH: PAUSE AND HISTORY
  The graph runs live by default (green LIVE badge).
  - Pause (or Space) freezes it, ready for screenshots.
    "Back to live" (or Space again) resumes.
  - Drag the graph left/right to scroll back through history. Use the mouse
    wheel to zoom from 30 seconds out to 30 days.
  - Use the dropdown for a set width (2 min to 7 days), and the arrows / arrow
    keys to step back and forward.
  - Type a date and time and press Go to jump straight to that moment.
  - Click any row in "Deviations from normal", or any heatmap cell, to jump
    the graph to that event, ping by ping.
  - Long periods show each point as the worst ping in that slice of time,
    so short spikes are never averaged away. Zoom in to see every ping.
  - "Copy image" puts the graph on the clipboard (with host + time range in
    the title) for pasting into Teams or email. "Save PNG" saves it as a file.

TIME PATTERN (e.g. "high ping every hour on the hour")
  The Time pattern panel stacks every hour on top of each other:
  - A finding at the top, e.g. "High ping recurs at :00-:01 past the hour.
    It was seen in 22 of 25 hours..."
    A minute is flagged when its deviation rate is at least 3x the overall rate
    (and at least 5%), AND it recurs in at least 40% of the hours (minimum 3).
    It needs 4+ hours of data, so use the 24h or 7d window.
  - By-minute bar chart: deviation % (or typical peak / average ms) for
    minutes :00-:59. Flagged minutes are orange.
  - Heatmap: one row per clock hour, one cell per minute, coloured by the
    worst ping in that minute (red = a timeout). An hourly problem shows as
    a vertical stripe. Hover a cell for its details.
  The printable Report includes the finding and the by-minute chart for every host.

INCIDENTS ACROSS HOSTS
  When several hosts go slow at the same moment, the Incidents panel shows it
  as one row: when it started, how long it lasted, which hosts were hit (worst
  first, with peak ms) and which hosts stayed normal.
  - Slow spells that start within incident_window_s (default 10 s) of each
    other, or while an incident is still in progress, are grouped together.
  - "Onset spread" is the time between the first and last host going slow:
    near 0 s means they all stalled at once.
  - A finding at the top spots incidents that recur at the same minute of the
    hour, e.g. "Incidents hitting several hosts start at :00 past the hour:
    seen in 5 of 5 hours". It needs 4+ hours of data.
  - "2+ hosts" hides single-host blips. Click a row to see all targets on the
    graph at that moment. Export CSV is ready to attach to a ticket.

MONITOR SELF-CHECK
  The badge in the header checks that PingWatch itself kept running on time.
  If the PingWatch machine is paused for more than 250 ms (busy, frozen VM,
  sleep), the badge turns amber ("Monitor paused 2x") and any incident at that
  moment is marked "paused". Pings then may look slow because of the
  monitoring machine rather than the network.

UPGRADING
  Replace PingWatch.exe with the new one. config.json and the data folder are
  kept, so existing history and events carry over.

HOW "NORMAL" AND "DEVIATION" WORK
  Normal    = the median of each host's last 300 successful pings (~5 minutes).
  Deviation = a timeout, OR a ping slower than 3x normal AND at least 10 ms above it.
              (A 3 ms host isn't flagged at 5 ms, but is flagged at 14 ms.)
  Optional  "high_ms": e.g. 100 means anything >= 100 ms always counts.
  An event opens on the first deviating ping and closes after 5 normal pings in a row.

config.json SETTINGS (edit, then restart PingWatch)
  targets                  hosts to ping (also editable in the dashboard)
  interval_seconds         1      gap between pings per host
  timeout_ms               1000
  baseline_window          300    samples used to work out "normal"
  deviation_factor         3
  deviation_min_delta_ms   10
  high_ms                  0      0 = off
  event_close_after        5
  retention_days           30
  range_buckets_ms         bucket edges for the "Ranges" pie view
  incident_window_s        10     slow spells starting this close together form one incident
  host / port              0.0.0.0 / 5055
  open_browser             true

SOURCE
  source\ holds the Go code. To rebuild, install Go 1.22+ and run (from source\):
    go build -ldflags "-s -w" -o PingWatch.exe .
