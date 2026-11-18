# LagSleuth

Portable Windows multi-host ping monitor (Go, standard library only, no CGO).
User guide: `README.txt`.
Renamed from PingWatch in Nov 2026. Keep the compatibility shims in `main.go`: the old
`PINGWATCH_SIMULATE` env var and the old `%LOCALAPPDATA%\PingWatch` data folder are still honoured.

Built to investigate latency spikes that recur at the top of every hour: several
VMs on one virtualisation host stall for 10-30 s at :00, while traffic leaving the
host is unaffected.

## Layout
- `source/main.go` - config, storage, monitors, deviation/event logic, stats, pattern analysis, HTTP API
- `source/ping_windows.go` - native ICMP via iphlpapi `IcmpSendEcho`
- `source/ping_other.go` - Linux/macOS fallback via system `ping` (testing only)
- `source/web/` - dashboard and report (vanilla JS + bundled Chart.js 4.4.4), embedded at build time

## Build and check (run from `source/`, Go 1.22+)
```
gofmt -l .            # must print nothing
go vet .
GOOS=windows go vet .
go test ./...         # incident grouping and pattern tests (synthetic data)
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/LagSleuth-amd64.exe .
```
Also build `386` and `arm64` for releases. Bump `var version` in main.go each release.

## Release assets (GitHub Releases, via `gh release create`)
- `LagSleuth-<version>-windows.zip`: `LagSleuth/LagSleuth.exe` (amd64), `LagSleuth/other-builds/LagSleuth-32bit.exe`
  and `LagSleuth-ARM64.exe`, `LagSleuth/README.txt`, `LagSleuth/source/`. Inside the zip the 64-bit exe stays `LagSleuth.exe`.
- Standalone exes are uploaded as `LagSleuth-64bit.exe`, `LagSleuth-32bit.exe` and `LagSleuth-ARM64.exe`
  (the 64-bit one is always uploaded with the `-64bit` suffix, never as plain `LagSleuth.exe`).

## Test without a network
`LAGSLEUTH_SIMULATE=1 go run . -no-browser -port 5077 -data /tmp/pwtest`
Synthetic history for pattern tests: write `data/<host>/YYYY-MM-DD.bin` files directly (13-byte records: float64 unix time, float32 RTT ms with NaN = timeout, 1-byte deviation flag).

## Rules
- Exes, `data/` and `config.json` are not committed. Publish exes as GitHub Releases.
- Keep it dependency-free and a single portable exe.
- `README.txt` ships with CRLF line endings.
