<div align="center">

# GBF-Dock

**Accelerate assets, never cross boundaries.** Zero-dependency local static asset cache proxy for *Granblue Fantasy*.

[![Release](https://img.shields.io/github/v/release/LORdDuk/GBF-Dock?style=flat-square&color=2f7cf6)](https://github.com/LORdDuk/GBF-Dock/releases/latest)
[![CI](https://img.shields.io/github/actions/workflow/status/LORdDuk/GBF-Dock/ci.yml?branch=main&style=flat-square)](https://github.com/LORdDuk/GBF-Dock/actions)
[![Go](https://img.shields.io/badge/Go-1.24%2B-00ADD8?style=flat-square&logo=go&logoColor=white)](https://go.dev/)
[![License](https://img.shields.io/github/license/LORdDuk/GBF-Dock?style=flat-square)](LICENSE)

[Quick Start](#-quick-start) · [Features](#-key-features) · [Security](#-security-boundaries) · [Changelog](CHANGELOG.md) · [简体中文](README.md)

</div>

---

## Overview

**GBF-Dock** caches *Granblue Fantasy* static assets (images, audio, scripts, animations) from Akamai CDNs locally in RAM and on disk for instant sub-millisecond loading. It **only** intercepts official `*.akamaized.net` asset hosts—all game APIs (`game.granbluefantasy.jp`, login endpoints) and unrelated desktop traffic are tunneled untouched.

---

## Quick Start

1. Download `GBF-Dock-v1.2.0-windows-amd64.zip` from [Releases](https://github.com/LORdDuk/GBF-Dock/releases/latest) and extract it.
2. Run **`GBF-Dock.exe`**, open **Network & Security**, install the Local CA certificate, confirm the Windows prompt, and **restart your browser completely**.
3. Choose one mutually exclusive acceleration mode on the Dashboard:
   - **Option A · Isolated Browser Mode (Recommended, zero system-proxy changes)**: Open the **Browser** tab to pick any Chromium browser or `.lnk` game shortcut (preserves logins & extensions by default; one-time `--proxy-pac-url` clears automatically when closed). Switching to this mode automatically turns off system proxy takeover.
   - **Option B · System Proxy Mode**: Switch to **System Proxy** on the Dashboard, enable the system proxy toggle, and run a connection check (compatible with Steam GBF and all browsers).
4. Supports **Follow System / Light / Dark** 3-way theme switching and responsive 3-tier window adaptation.

> **Upgrading from legacy `GBFCache`**: Existing certificates and configs in `%APPDATA%\GBFCache` are automatically migrated to `%APPDATA%\GBF-Dock` on first launch (no re-installation needed).

---

## Key Features

| Area | Details |
|---|---|
| **Hot RAM & Streaming** | `96 MB` SLRU in-memory hot cache (`<= 512 KB` items) serving `200`/`206`/`304`, Stream-Through misses (including known-length `Range: bytes=0-` `206` streaming), and singleflight dedup. |
| **Isolated Browser Launcher** | Choose any Chromium `.exe` or `.lnk` shortcut; preserves original logins & extensions by default with automatic zero-residue flag cleanup on exit, graceful restart detection, or switchable isolated side-by-side profile with one-click cleanup. |
| **Fast Binary Metadata Index** | CRC32-verified `.meta_index.v1` (v2) with decoupled TTL verification time vs. last-access tracking. |
| **CDN Shard Fallback** | Covers browser, Steam, and `-gbf` asset hosts; automatically falls back and caches retired `prd-game-a1`~`a5` shard requests to the primary CDN host. |
| **Proxy / VPN Chaining** | Auto-detects system proxy, common local HTTP/SOCKS5 ports (Clash, v2rayN, Sing-box, etc.), or TUN adapters; restores original proxy cleanly on exit. |

## Security Boundaries

- **Static-Only Interception**: Strictly limited to `*.akamaized.net` asset domains; never decrypts game APIs.
- **X.509 Name Constraints**: Local ECDSA P-256 CA enforces `PermittedDNSDomainsCritical` on `akamaized.net`.
- **Dual-Port Isolation & SSRF Defense**: Admin UI/API (`127.0.0.1:8766`) is physically separated from the data proxy (`127.0.0.1:8765`), with `X-GBF-Dock` CSRF checks and LAN-to-loopback blocking on HTTP & `CONNECT`.

<details>
<summary><b>Build from Source (Go 1.24+, zero external dependencies)</b></summary>

```bash
go vet ./...
go test -v -count=1 ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -H windowsgui" -o GBF-Dock.exe .
```

</details>

## License

[MIT](LICENSE)
