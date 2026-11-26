# GBF-Dock v1.0.0

**只加速，不越界。** 专为《碧蓝幻想（Granblue Fantasy）》打造的零依赖本地静态资源缓存代理。

[English](README.en.md) · [使用说明](docs/USAGE.md) · [GitHub Releases](https://github.com/LORdDuk/GBF-Dock/releases)

---

## 设计原则与安全边界

- **仅解密官方 CDN 静态资源**：只对网页版与 Steam 版 `*.akamaized.net` 静态素材域名生效；游戏 API（`game.granbluefantasy.jp`、`gbf.game.mbga.jp`、`connect.mobage.jp` 等）以及桌面端其他软件（如浏览器、聊天/AI 客户端）**绝不解密**，原样通过上游代理或直连隧道透传。
- **根证书名称约束（Name Constraints）**：本机生成的 ECDSA P-256 根证书（`GBF-Dock Local CA`）强制写入 `PermittedDNSDomainsCritical` (`akamaized.net`)，结构上无法为其他任何域名签发可信证书。
- **双端口物理隔离 + LAN SSRF 防护**：代理数据端口（默认 `127.0.0.1:8765`）与本机控制面板/管理接口（默认 `127.0.0.1:8766`）物理分离；即使开启局域网共享，外部设备也无法经数据端口向回环地址或管理端口发起越权访问。
- **标准手动 HTTP 代理串联**：Windows 端不使用系统 PAC，避免引发部分桌面客户端断网；开启接管自动备份原系统代理，关闭或退出程序自动完整还原。

---

## 核心特性

- **毫秒级命中与流式传输**：
  - 内置 `96 MB` RAM SLRU 分段热缓存（单文件 `<= 512 KB` 准入），直接从内存返回 `200`/`206`/`304`。
  - 未命中时支持边下边传（Stream-Through），支持 `Range: bytes=0-` 已知长度音频流式 `206` 返回。
  - 请求合并（Singleflight）避免同资源并发重复拉取。
- **持久化二进制元数据索引（`.meta_index.v1`）**：
  - 带 IEEE CRC32 校验与相对路径存储，重启秒级恢复索引与体积统计。
  - 将「TTL 确认时间」与「最后访问时间」完全解耦，保证 24 小时后台 `ETag` / `Last-Modified` 校验不会被日常访问冲掉。
- **CDN 适配与失效分片自动回退**：
  - 覆盖 `prd-game-a*-granbluefantasy.akamaized.net`、`prd-game-a*-granbluefantasy-steam.akamaized.net`、`*-gbf.akamaized.net` 等官方资源域名。
  - 针对老旧插件/扩展仍请求的已下线分片（如 `prd-game-a1`~`a5` 返回 `NXDOMAIN`/`EOF`），首次失败自动回退主域名拉取并缓存，30 分钟内记忆失效分片避免重复卡顿。
- **实用工具箱**：
  - 自定义缓存目录、旧版缓存/原始 CDN 目录无损并入（带异步进度条与取消）、5 分类明细清理、6 维系统全面体检、局域网移动端接入向导（PAC + 证书）、Windows 开机托盘静默自启（`-minimized`）。
- **旧版无痛迁移**：
  - 首次启动会自动检测 `%APPDATA%\GBFCache` 中的已有根证书与配置并平滑迁移至 `%APPDATA%\GBF-Dock`，老用户无需重新安装根证书即可继续使用，原有 `cache/` 与 `.meta_index.v1` 亦可直接沿用或通过“并入旧缓存”合并。

---

## 快速上手

1. 前往 [Releases](https://github.com/LORdDuk/GBF-Dock/releases) 下载 `GBF-Dock-v1.0.0-windows-amd64.zip` 并解压到任意文件夹。
2. 双击运行 **`GBF-Dock.exe`**，在「本机证书」卡片点击 **「安装」**，安全提示选择 **「是」**，然后彻底重启浏览器。
3. 开启 **「代理接管」**，点击 **「检测浏览器」** 看到「✓ 检测通过」即可正常游玩。

详细图文步骤与常见问题请阅读 [docs/USAGE.md](docs/USAGE.md)。

---

## 命令行参数

```text
GBF-Dock.exe
  -addr 127.0.0.1:8765           # 代理数据监听地址
  -admin-addr 127.0.0.1:8766     # 本机控制面板地址（默认代理端口+1，仅限本机回环）
  -dir <程序目录>                # 默认数据根目录（含 cache/）
  -ttl 24h                       # 后台 ETag / Last-Modified 复核周期
  -upstream-proxy <addr|direct>  # 覆盖自动检测，指定上游代理或直连
  -minimized                     # 启动后静默驻留托盘，不自动弹出控制面板
```

---

## 从源码构建（零第三方依赖）

仅依赖 Go 1.24+ 标准库，无需 `go get` 任何外部包：

```powershell
# 运行测试与静态检查
go vet ./...
go test -v -count=1 ./...

# 构建 Windows GUI 可执行文件
$env:CGO_ENABLED = "0"
$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -trimpath -ldflags "-s -w -H windowsgui" -o GBF-Dock.exe .
```

## 开源协议

[MIT License](LICENSE)
