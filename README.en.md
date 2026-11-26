# GBF-Dock v1.0.0

**Accelerate assets, never cross boundaries.** A zero-dependency local static asset cache proxy tailored for *Granblue Fantasy*.

[中文说明](README.md) · [Releases](https://github.com/LORdDuk/GBF-Dock/releases)

## Key Security & Architecture Guarantees

- **Strict CDN-Only Scope**: Intercepts only official `*.akamaized.net` static asset hosts (browser & Steam editions). Game APIs (`game.granbluefantasy.jp`, `gbf.game.mbga.jp`, `connect.mobage.jp`, etc.) and all other traffic are tunneled untouched.
- **X.509 Name-Constrained Root CA**: Generated locally (`GBF-Dock Local CA`, ECDSA P-256) with `PermittedDNSDomainsCritical` restricted to `akamaized.net`.
- **Physical Dual-Port Isolation**: Data proxy (`127.0.0.1:8765`) and Admin UI/API (`127.0.0.1:8766`) are isolated on separate listeners, with CSRF header checks (`X-GBF-Dock: 1`), `Host` validation, and LAN-to-loopback SSRF blocking.
- **Fast Hot Cache & Persistent Binary Index**: `96 MB` SLRU RAM cache (`<= 512 KB` items), CRC32-verified binary metadata index, decoupled verification TTL vs. last-access tracking, Stream-Through `Range: bytes=0-` support, and automatic fallback for retired CDN shards (`prd-game-a1`~`a5` -> `prd-game-a`).
- **Seamless Legacy Migration**: Automatically migrates certificates and settings from `%APPDATA%\GBFCache` when upgrading.

## Build from Source

Requires Go 1.24+ (pure Go standard library, zero external dependencies):

```bash
go vet ./...
go test -count=1 ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -H windowsgui" -o GBF-Dock.exe .
```

## License

[MIT](LICENSE)
