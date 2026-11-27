<div align="center">

# GBF-Dock

**只加速，不越界** —— 专为《碧蓝幻想（Granblue Fantasy）》打造的零依赖本地静态资源缓存代理

[![Release](https://img.shields.io/github/v/release/LORdDuk/GBF-Dock?style=flat-square&color=2f7cf6)](https://github.com/LORdDuk/GBF-Dock/releases/latest)
[![CI](https://img.shields.io/github/actions/workflow/status/LORdDuk/GBF-Dock/ci.yml?branch=main&style=flat-square)](https://github.com/LORdDuk/GBF-Dock/actions)
[![Go](https://img.shields.io/badge/Go-1.24%2B-00ADD8?style=flat-square&logo=go&logoColor=white)](https://go.dev/)
[![Platform](https://img.shields.io/badge/Platform-Windows%20%7C%20macOS-4b5563?style=flat-square)](https://github.com/LORdDuk/GBF-Dock/releases)
[![License](https://img.shields.io/github/license/LORdDuk/GBF-Dock?style=flat-square)](LICENSE)

[快速安装](#-快速安装与使用) · [核心特性](#-核心特性) · [安全边界](#-安全边界) · [更新日志](CHANGELOG.md) · [English](README.en.md)

</div>

---

## 简介

**GBF-Dock** 将远端 Akamai CDN 上的游戏图片、音频、动画与脚本资源停泊在本地磁盘与内存热缓存中，命中后直接毫秒级本地返回，减少海外跨区加载等待与重复流量消耗。

它严格守住代理边界：**只处理官方 `*.akamaized.net` 静态资源域名**，游戏登录与战斗 API（`game.granbluefantasy.jp` 等）及其他桌面客户端流量全部原样隧道透传，绝不解密、绝不篡改。

---

## 快速安装与使用

1. **下载解压**：前往 [Releases](https://github.com/LORdDuk/GBF-Dock/releases/latest) 下载 `GBF-Dock-v1.1.0-windows-amd64.zip`，解压到任意英文或中文目录。
2. **安装证书**：双击运行 **`GBF-Dock.exe`**，在仪表盘「本机证书」卡片点击 **「安装」**，安全提示选择 **「是」**，随后**彻底关闭并重启浏览器**。
3. **开启加速（二选一）**：
   - **方式 A · 独立加速启动（推荐，免接管系统代理）**：在仪表盘「独立加速启动」下拉框选择任意 Chromium 浏览器（如 Chrome / Edge / Brave / 百分浏览器），或点击 **「选程序 / .lnk」** 选取桌面已有的 GBF 游戏快捷方式；默认 **保留原浏览器配置与登录态**，且代理参数仅当次启动有效、关闭浏览器即自动清除（其他软件与 FlClash 等梯子完全不受影响）。也支持切换「独立空白配置」双开并随时一键清理。
   - **方式 B · 全局代理接管**：打开 **「代理接管」** 开关，点击 **「检测浏览器」** 看到「✓ 检测通过」即可启动游戏。

> **旧版 GBF Cache 用户无痛迁移**  
> 首次启动会自动将 `%APPDATA%\GBFCache` 中的根证书与配置文件迁移到 `%APPDATA%\GBF-Dock`（保留原目录不动，**无需重装证书**）；旧版 `cache/` 与 `.meta_index.v1` 亦可直接复制沿用或通过控制面板「导入旧缓存」无损并入。

更多图解、局域网手机/iPad 共用与常见问题请查看 [详细使用说明](docs/USAGE.md)。

---

## 核心特性

| 模块 | 能力说明 |
|---|---|
| **热缓存 + 流式回源** | 内置 `96 MB` RAM SLRU 内存热缓存（`<= 512 KB` 静态资源直出 `200`/`206`/`304`）；未命中支持边下边传（含已知长度 `Range: bytes=0-` 流式 `206`）与并发请求合并。 |
| **独立加速浏览器启动** | 支持自选本机 Chromium 浏览器（`.exe`）或游戏快捷方式（`.lnk`），默认保留原配置与登录态（关窗即自动清参数），支持在运行冲突时一键温和重启或切换「独立空白配置」双开并一键清理数据。 |
| **秒级持久化索引** | 二进制 `.meta_index.v1`（v2）带 IEEE CRC32 校验，重启秒级恢复；彻底解耦「TTL 确认时间」与「最后访问时间」，确保 24h 后台校验按时触发。 |
| **CDN 适配与失效回退** | 覆盖网页版 / Steam 版 / `-gbf` 官方静态 CDN；遇到老插件请求已下线旧分片（`a1`~`a5`）时自动回退主域名并记忆 30 分钟。 |
| **梯子 / 加速器全兼容** | 自动识别系统代理、常见本地代理端口（Clash / FlClash / Verge / v2rayN / Sing-box / 岛风GO 等 HTTP & SOCKS5）与 TUN 虚拟网卡，也支持自定义或直连。 |
| **完整工具箱** | 自定义缓存目录、旧缓存与原始 CDN 目录异步并入、5 分类清理、6 维体检、Win11 托盘悬浮状态卡、局域网向导与开机静默自启（`-minimized`）。 |

---

## 安全边界

- **仅解密静态 CDN**：仅匹配 `*.akamaized.net` 游戏静态素材主机名；游戏接口与第三方应用全量原样透传。
- **X.509 名称约束**：本机生成 `GBF-Dock Local CA`（ECDSA P-256），强制写入 `PermittedDNSDomainsCritical` (`akamaized.net`)，结构上无法签发其他域名的受信任证书。
- **双端口隔离 + SSRF 防护**：代理数据面（`127.0.0.1:8765`）与控制面板/API（`127.0.0.1:8766`）物理分港；即使开启局域网共享，也严禁外部设备经数据面访问回环或管理端口。
- **标准手动 HTTP 代理**：Windows 端不使用系统 PAC，避免干扰其他桌面客户端联网；开启自动备份原代理，关闭或退出自动还原。

---

<details>
<summary><b>命令行参数与从源码构建（纯 Go 标准库，零第三方依赖）</b></summary>

### 命令行参数

```text
GBF-Dock.exe
  -addr 127.0.0.1:8765           # 代理数据监听地址
  -admin-addr 127.0.0.1:8766     # 本机控制面板监听地址（默认代理端口+1，仅限本机回环）
  -dir <程序目录>                # 默认数据根目录（含 cache/）
  -ttl 24h                       # 后台 ETag / Last-Modified 复核周期
  -upstream-proxy <addr|direct>  # 覆盖自动检测，指定上游代理或直连
  -minimized                     # 启动后静默驻留托盘，不自动弹出窗口
```

### 从源码构建（需要 Go 1.24+）

```powershell
go vet ./...
go test -v -count=1 ./...

$env:CGO_ENABLED = "0"
$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -trimpath -ldflags "-s -w -H windowsgui" -o GBF-Dock.exe .
```

</details>

---

## 更新日志与开源协议

- 完整版本变更记录见 [CHANGELOG.md](CHANGELOG.md)。
- 本项目基于 [MIT License](LICENSE) 开源。
