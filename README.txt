LagSleuth 1.6 - multi-host ping monitor
=======================================

Runs on any Windows 10 / 11 or Windows Server 2016+ machine.
No install, no Python, no admin rights needed.

QUICK START
  1. Copy this folder anywhere (desktop, USB stick, a server share copied locally).
  2. Double-click LagSleuth.exe.
     It opens the dashboard in your browser at http://localhost:5055.
  3. Add the hosts / IPs to watch. You can paste a list separated by commas or spaces.
  Keep the black console window open while monitoring. Closing it stops LagSleuth.

WHICH EXE?
  LagSleuth.exe                      normal 64-bit Windows (almost every PC/server)
  other-builds\LagSleuth-32bit.exe   old 32-bit Windows 10
  other-builds\LagSleuth-ARM64.exe   ARM laptops (Surface Pro X / Snapdragon)

FIRST RUN PROMPTS
  - "Windows protected your PC" (SmartScreen): the exe isn't code-signed.
    Click More info > Run anyway.
  - Windows Firewall may ask to allow network access. This only matters if you
    want to open the dashboard from another PC (http://<this-pc>:5055).
    To keep it local only, set "host": "127.0.0.1" in config.json.

WHERE THINGS ARE SAVED
  config.json and a data\ folder are created next to LagSleuth.exe.
  If that folder is read-only (e.g. Program Files), they go to
  %LOCALAPPDATA%\LagSleuth instead. The console window shows which.
  Samples are kept for 30 days (retention_days).

COMMAND LINE OPTIONS
  LagSleuth.exe -port 8080         use a different port
  LagSleuth.exe -no-browser        don't open the browser (e.g. when started at logon)
  LagSleuth.exe -data D:\pingdata  keep config + data somewhere else

  To start it automatically at logon, put a shortcut to
  "LagSleuth.exe -no-browser" in shell:startup.

PING GRAPH: PAUSE AND HISTORY
  The graph runs live by default (green LIVE badge).
  - Pause (or Space) freezes it, ready for screenshots.
    "Back to live" (or Space again) resumes.
  - Drag the graph left/right to scroll back through history. Use the mouse
    wheel to zoom from 30 seconds out to 30 days.
  - Use the dropdown for a set width (2 min to 7 days), and the arrows / arrow
    keys to step back and forward.
  - Type a date and time and press Go to jump straight to that moment.
  - With "All targets", click a host in the legend under the graph to show
    only that host; click it again to show all hosts. Ctrl+click (or
    Shift+click) adds or removes a single host. Your choice is remembered.
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

SERVER METRICS (CPU, MEMORY, DISK)
  LagSleuth reads Windows performance counters every second from this PC and
  from every monitored host with a private address, and shows them live:
  - On each host card: CPU, disk latency and free memory right now.
  - In the Server metrics panel: one collapsible graph per metric, on the
    same timeline as the ping graph (pause, drag, zoom and jumping to an
    incident move them too; hiding a host in the legend hides it here too).
    Disk latency, CPU and CPU stolen are open by default; click a title to
    open or close a graph. The dashed line is this PC.
  - Metrics: CPU %, CPU queue, CPU stolen (VMware: time the host made the VM
    wait), memory available / committed / paging / ballooned (VMware), disk
    read and write latency, disk queue and disk I/O.
  - Each incident lists the worst disk latency, CPU and CPU stolen time of
    every affected host during the incident (hover for a per-host breakdown).
  - "Sample every" (1 s to 60 s) changes the interval immediately and saves it.
  - Reading a remote server uses the Windows account LagSleuth runs as. On
    each server that account must be in "Performance Monitor Users" (or an
    admin), the Remote Registry service must be running and the "File and
    Printer Sharing" firewall rule allowed. Servers without access show
    "no access" and are retried every minute.
  - Metrics are kept for metrics_retention_days (default 7): about 4.5 MB per
    host per day at 1-second sampling.
  Every panel below the targets can be collapsed with the arrow by its title.

SHOWING ONLY SOME SERVERS
  Each host card has a tick box. Untick a host to hide it from the ping graph
  (All targets), the server metric graphs and their legends. Ctrl+click a tick
  box to show only that host; "show all hosts" in the Server metrics panel
  brings them all back. Clicking a host in any legend does the same as in the
  ping graph legend. Your choice is remembered in the browser.

TOP PROCESSES (WHAT IS USING THE CPU)
  Every 5 seconds (process_interval_seconds) LagSleuth also records the 10
  busiest processes on each machine, with CPU as a share of the whole machine
  (like Task Manager) and private memory.
  - Host cards show the busiest processes right now ("top: sqlservr 22%").
  - The Top processes panel shows, for the machine picked at the top right
    (default: the selected card), a CPU graph of the busiest processes over
    the ping graph's time window, and a table: peak CPU, average CPU, peak
    memory and how often it was among the 10 busiest.
  - Each incident shows the busiest process during it, and the event log
    panel and the report list the top 3 per host.
  Uses the same access as the server metrics. Kept as long as the metrics.

REPORT AND PDF
  Report opens a printable page for the stats window (or, if the graph is
  paused, exactly the period shown on the graph). Pick a period or a custom
  From / To range at the top, and use the Targets list to report on one host
  or a few (hosts unticked on the dashboard are left out already). It
  contains: summary, ping graph for the period, server metric graphs,
  incidents with per-host disk / CPU figures and event log counts, then one
  section per host. "Export PDF" opens the print
  dialog: choose "Save as PDF" or "Microsoft Print to PDF" (A4 landscape).

EVENT LOGS AROUND INCIDENTS
  After an incident, LagSleuth reads the Windows event logs from this PC and
  from every affected host, from 3 minutes before the incident started to 3
  minutes after it ended, and collects them once that time has passed. Change
  the window with "event_log_before_s" and "event_log_after_s" in config.json
  (seconds). Click "Event logs" on the incident row to see them, grouped by
  host, with the time relative to the incident start.
  - Collected: Critical, Error and Warning events from System and Application,
    plus events that often explain a stall: clock changes, shadow copies /
    backups (VSS, volsnap, VMware Tools), scheduled tasks starting, Windows
    Update, Storport and Time-Service.
  - If Sysmon is installed, every program that started in that window, with
    the program that launched it and its command line. This names short
    scripts and scans that finish before a process sample can see them.
  - Short single-host blips are not collected automatically; use "Collect".
  - Public addresses (e.g. 8.8.8.8) are skipped.
  - Reading a remote server's logs uses the Windows account LagSleuth runs as.
    On each server, that account must be in the "Event Log Readers" group (or
    be an admin) and the "Remote Event Log Management" firewall rule must be
    enabled. Servers without access show "no access" and are retried after
    30 minutes or when you click "Collect again".
  - Some logs (e.g. Storport) can only be read by an admin; they are listed as
    "not readable" and the rest are still collected.
  - Set "event_logs": false in config.json to turn this off.

CREDENTIALS FOR A SERVER (MEMORY ONLY)
  For a server you can ping but not read, click the key button in the top
  bar (or "credentials" next to "no access") and enter a user name
  (DOMAIN\user, SERVER\user or user@domain) and password. They are used for
  that server's event logs and metrics.
  - Never stored: not in config.json, the data folder, Windows Credential
    Manager or any log. Closing LagSleuth wipes them.
  - The password is kept encrypted in memory, is never shown again and never
    appears on a command line.
  - Can only be entered on the PC running LagSleuth, at http://localhost:5055.

MONITOR SELF-CHECK
  The badge in the header checks that LagSleuth itself kept running on time.
  If the LagSleuth machine is paused for more than 250 ms (busy, frozen VM,
  sleep), the badge turns amber ("Monitor paused 2x") and any incident at that
  moment is marked "paused". Pings then may look slow because of the
  monitoring machine rather than the network.

UPGRADING
  Replace LagSleuth.exe with the new one. config.json and the data folder are
  kept, so existing history and events carry over.

HOW "NORMAL" AND "DEVIATION" WORK
  Normal    = the median of each host's last 300 successful pings (~5 minutes).
  Deviation = a timeout, OR a ping slower than 3x normal AND at least 10 ms above it.
              (A 3 ms host isn't flagged at 5 ms, but is flagged at 14 ms.)
  Optional  "high_ms": e.g. 100 means anything >= 100 ms always counts.
  An event opens on the first deviating ping and closes after 5 normal pings in a row.

config.json SETTINGS (edit, then restart LagSleuth)
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
  event_logs               true   collect Windows event logs around incidents
  metrics                  true   read CPU / memory / disk counters from the hosts
  metrics_interval_seconds 1      how often (also changeable on the dashboard)
  metrics_retention_days   7      older metric and process files are deleted
  process_interval_seconds 5      how often the busiest processes are recorded
  host / port              0.0.0.0 / 5055
  open_browser             true

SOURCE
  source\ holds the Go code. To rebuild, install Go 1.22+ and run (from source\):
    go build -ldflags "-s -w" -o LagSleuth.exe .
