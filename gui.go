package main

import (
	"bytes"
	"context"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed ui.html
var uiHTML []byte

const gameURL = "https://game.granbluefantasy.jp/"

// 仅作兼容用途保留（例如 macOS / 移动端 Wi-Fi 手动配置）；Windows 默认使用标准手动 HTTP 代理串联模式。
const pacTemplate = `function FindProxyForURL(url, host) {
  if (shExpMatch(host, "prd-game-a*-granbluefantasy.akamaized.net") ||
      shExpMatch(host, "prd-game-a*-granbluefantasy-steam.akamaized.net") ||
      shExpMatch(host, "prd-game-a*-gbf.akamaized.net") ||
      host == "granbluefantasy.akamaized.net" || shExpMatch(host, "*.granbluefantasy.akamaized.net") ||
      host == "granbluefantasy-steam.akamaized.net" || shExpMatch(host, "*.granbluefantasy-steam.akamaized.net") ||
      host == "gbf.akamaized.net" || shExpMatch(host, "*.gbf.akamaized.net"))
    return "PROXY %s; DIRECT";
  return "%s";
}
`

// 检测页：浏览器访问它时，如果经过本程序就由本程序直接回答；没经过就会落到 Akamai，显示对方的错误页
const (
	checkPath       = "/__gbf-dock-check"
	legacyCheckPath = "/__gbf-cache-check"
	checkURL        = "https://prd-game-a-granbluefantasy.akamaized.net" + checkPath
	checkPage       = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><title>GBF-Dock 检测通过</title>
<style>body{margin:0;min-height:100vh;display:grid;place-items:center;font:16px/1.6 "Segoe UI Variable Text","Segoe UI","Microsoft YaHei UI",sans-serif;background:#f3f5f9;color:#18202b}
div{background:#fff;border:1px solid #e2e8f0;border-radius:12px;padding:28px 34px;max-width:520px;box-shadow:0 4px 18px rgba(0,0,0,.06)}h1{margin:0 0 8px;font-size:20px;color:#0f7b0f}
@media(prefers-color-scheme:dark){body{background:#181a1f;color:#f3f6fb}div{background:#232730;border-color:#383f4d}h1{color:#6ccb5f}}</style></head>
<body><div><h1>✓ 检测通过</h1>这个浏览器访问碧蓝幻想资源时会经过 GBF-Dock，根证书也受信任。<br>可以关掉这个页面，正常玩游戏了。</div></body></html>`
)

// pacFallback：系统原本开着手动代理（如 Clash/v2rayN 的系统代理）时，其他网站继续走它，而不是改成直连。
// server 是 Windows 的 ProxyServer 格式："host:port" 或 "http=h:p;https=h:p;socks=h:p"。
func pacFallback(server string) string {
	if server == "" {
		return "DIRECT"
	}
	if !strings.Contains(server, "=") {
		return "PROXY " + server + "; DIRECT"
	}
	byScheme := map[string]string{}
	for _, part := range strings.Split(server, ";") {
		if k, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok && v != "" {
			byScheme[strings.ToLower(k)] = v
		}
	}
	switch {
	case byScheme["https"] != "":
		return "PROXY " + byScheme["https"] + "; DIRECT"
	case byScheme["http"] != "":
		return "PROXY " + byScheme["http"] + "; DIRECT"
	case byScheme["socks"] != "":
		return "SOCKS5 " + byScheme["socks"] + "; DIRECT"
	}
	return "DIRECT"
}

// app 是控制面板：默认与代理服务采用物理双端口隔离，各自承担管理面与数据面职责。
type app struct {
	p          *proxy
	pacURL     string
	configPath string
	backupPath string
	logs       *logRing
	quit       chan struct{}
	once       sync.Once
	cleanOnce  sync.Once
	tray       bool // 托盘图标可用：关窗后留在后台；不可用时关窗即退出

	adminAddr string
	adminMux  http.Handler

	cfgMu          sync.Mutex
	cfg            appConfig
	browserInfo    gameBrowserInfo
	cliOverride    bool
	upstreamSource string
	tunAdapter     string
	upstreamAlive  bool
	aliveCheckedAt time.Time
	lastAutoDetect time.Time
	detecting      atomic.Bool

	mu    sync.Mutex
	sys   sysState
	sysAt time.Time
	sysFn func() sysState

	importMu     sync.Mutex
	importState  importProgress
	importCancel context.CancelFunc

	lnMu       sync.Mutex
	activeLn   net.Listener
	lastPingMs atomic.Int64
	lastPingAt atomic.Int64

	lanIPsMu sync.Mutex
	lanIPs   []string
	lanIPsAt time.Time
}

type sysState struct {
	CertTrusted   bool   `json:"certTrusted"`
	OldCerts      int    `json:"oldCerts"`      // 旧版本留下的历史根证书张数
	ProxyEnabled  bool   `json:"proxyEnabled"`  // 当前 Windows 手动代理是否由本程序接管
	LegacyPAC     bool   `json:"legacyPAC"`     // 是否残留了旧版 PAC 设置
	PAC           string `json:"pac"`           // 系统当前的自动配置脚本地址
	ManualProxy   string `json:"manualProxy"`   // 系统手动代理（开启时）
	SavedUpstream string `json:"savedUpstream"` // proxy_backup.json 记录的原代理
	AutoStart     bool   `json:"autoStart"`     // 是否已启用开机静默自启（托盘最小化）
}

func newApp(p *proxy, logs *logRing) *app {
	cfgDir := filepath.Dir(p.certDir)
	a := &app{
		p:          p,
		pacURL:     "http://" + p.addr + "/proxy.pac",
		configPath: filepath.Join(cfgDir, "config.json"),
		backupPath: filepath.Join(cfgDir, "proxy_backup.json"),
		logs:       logs,
		quit:       make(chan struct{}),
		cfg:        appConfig{UpstreamMode: "auto"},
	}

	adminRoutes := http.NewServeMux()
	adminRoutes.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Write(uiHTML)
	})
	adminRoutes.HandleFunc("GET /api/status", a.status)
	adminRoutes.HandleFunc("GET /api/browsers", a.browsersHandler)
	adminRoutes.HandleFunc("GET /api/usage", a.usage)
	adminRoutes.HandleFunc("GET /api/cache-breakdown", a.cacheBreakdownHandler)
	adminRoutes.HandleFunc("GET /api/cache-detect-old", a.cacheDetectOldHandler)
	adminRoutes.HandleFunc("GET /api/cache-import-progress", a.cacheImportProgressHandler)
	adminRoutes.HandleFunc("POST /api/{action}", a.action)

	a.adminMux = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loopback, _ := classifyRemoteAddr(r.RemoteAddr)
		if !loopback || !a.trustedFor(r, a.controlAddr()) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		adminRoutes.ServeHTTP(w, r)
	})

	dataRoutes := http.NewServeMux()
	dataRoutes.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		loopback, _ := classifyRemoteAddr(r.RemoteAddr)
		if !loopback {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = fmt.Fprint(w, mobileGuideHTML(r, p.addr))
			return
		}
		if a.isDualPort() {
			http.Redirect(w, r, "http://"+a.controlAddr()+"/", http.StatusFound)
			return
		}
		a.adminMux.ServeHTTP(w, r)
	})
	dataRoutes.HandleFunc("GET /proxy.pac", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ns-proxy-autoconfig")
		w.Header().Set("Cache-Control", "no-store")
		loopback, _ := classifyRemoteAddr(r.RemoteAddr)
		if !loopback {
			fmt.Fprintf(w, pacTemplate, lanPacHostPort(r.Host, p.addr), "DIRECT")
			return
		}
		fmt.Fprintf(w, pacTemplate, a.localProxyHostPort(), a.pacFallbackAddr())
	})
	dataRoutes.HandleFunc("GET /ca.crt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-x509-ca-cert")
		w.Write(p.caPEM)
	})
	dataRoutes.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if a.isDualPort() || strings.HasPrefix(r.URL.Path, "/api/") {
			if a.isDualPort() {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		a.adminMux.ServeHTTP(w, r)
	})

	p.local = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if a.isDualPort() {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			a.adminMux.ServeHTTP(w, r)
			return
		}
		dataRoutes.ServeHTTP(w, r)
	})
	return a
}

func (a *app) setAdminAddr(addr string) {
	a.adminAddr = strings.TrimSpace(addr)
	if a.p != nil {
		a.p.setAdminAddr(a.adminAddr)
	}
}

func (a *app) controlAddr() string {
	if a.adminAddr != "" {
		return a.adminAddr
	}
	return a.p.addr
}

func (a *app) isDualPort() bool {
	return a.adminAddr != "" && !isSelfHostPort(a.adminAddr, a.p.addr)
}

func (a *app) adminHandler() http.Handler {
	return a.adminMux
}

func (a *app) getAllowLAN() bool {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	return a.cfg.AllowLAN
}

func (a *app) initUpstream(cliSet bool, cliVal string) {
	a.cfgMu.Lock()
	if b, err := os.ReadFile(a.configPath); err == nil {
		var loaded appConfig
		if json.Unmarshal(b, &loaded) == nil {
			if loaded.UpstreamMode != "" {
				a.cfg.UpstreamMode = loaded.UpstreamMode
			}
			if loaded.UpstreamProxy != "" {
				a.cfg.UpstreamProxy = loaded.UpstreamProxy
			}
			a.cfg.AllowLAN = loaded.AllowLAN
			a.p.allowLAN.Store(loaded.AllowLAN)
			a.cfg.CacheDir = strings.TrimSpace(loaded.CacheDir)
			a.cfg.GameBrowserPath = strings.TrimSpace(loaded.GameBrowserPath)
			a.cfg.GameBrowserLabel = strings.TrimSpace(loaded.GameBrowserLabel)
			a.cfg.GameBrowserProfileMode = normalizeBrowserProfileMode(loaded.GameBrowserProfileMode)
			if a.cfg.CacheDir != "" {
				if _, cErr := a.p.setCacheRoot(a.cfg.CacheDir); cErr != nil {
					log.Printf("加载自定义缓存目录 %s 失败，回退默认目录：%v", a.cfg.CacheDir, cErr)
					a.cfg.CacheDir = ""
				}
			}
		}
	}
	a.refreshBrowserInfoLocked()

	if cliSet {
		a.cliOverride = true
		if err := a.p.setUpstream(cliVal); err != nil {
			log.Printf("命令行上游代理参数无效（%v），回退到直连", err)
			_ = a.p.setUpstream("")
			a.cfg.UpstreamMode = "direct"
			a.cfg.UpstreamProxy = ""
			a.upstreamSource = "手动直连"
			a.upstreamAlive = false
		} else if a.p.getUpstream() == "" {
			a.cfg.UpstreamMode = "direct"
			a.cfg.UpstreamProxy = ""
			a.upstreamSource = "命令行直连"
			a.upstreamAlive = false
		} else {
			a.cfg.UpstreamMode = "custom"
			a.cfg.UpstreamProxy = a.p.getUpstream()
			a.upstreamSource = "命令行参数"
			a.upstreamAlive = isUpstreamURLAlive(a.cfg.UpstreamProxy, 150*time.Millisecond)
		}
		a.tunAdapter = detectTUNAdapter()
		a.aliveCheckedAt = time.Now()
		a.cfgMu.Unlock()
		return
	}

	mode := a.cfg.UpstreamMode
	if mode == "" {
		mode = "auto"
		a.cfg.UpstreamMode = mode
	}
	prevProxy := a.cfg.UpstreamProxy
	a.cfgMu.Unlock()

	sys := a.readSysState()

	switch mode {
	case "direct":
		_ = a.p.setUpstream("")
		tun := detectTUNAdapter()
		a.cfgMu.Lock()
		a.upstreamSource = "手动直连"
		a.upstreamAlive = false
		a.tunAdapter = tun
		a.aliveCheckedAt = time.Now()
		a.cfgMu.Unlock()

	case "custom":
		a.cfgMu.Lock()
		if err := a.p.setUpstream(a.cfg.UpstreamProxy); err != nil || a.p.getUpstream() == "" {
			prev := a.cfg.UpstreamProxy
			a.cfgMu.Unlock()
			det := a.detectUpstream(sys, prev)
			a.cfgMu.Lock()
			a.applyDetectionLocked(det)
			a.saveConfigLocked()
			a.cfgMu.Unlock()
		} else {
			up := a.p.getUpstream()
			a.cfg.UpstreamProxy = up
			a.cfgMu.Unlock()
			alive := isUpstreamURLAlive(up, 150*time.Millisecond)
			tun := detectTUNAdapter()
			a.cfgMu.Lock()
			a.upstreamSource = "手动指定"
			a.upstreamAlive = alive
			a.tunAdapter = tun
			a.aliveCheckedAt = time.Now()
			a.cfgMu.Unlock()
		}

	default: // "auto"
		det := a.detectUpstream(sys, prevProxy)
		a.cfgMu.Lock()
		a.applyDetectionLocked(det)
		a.saveConfigLocked()
		a.cfgMu.Unlock()
	}

	// 启动自愈：如果系统残留了旧版 PAC，自动平滑迁移为手动代理串联模式
	if runtime.GOOS == "windows" && sys.LegacyPAC {
		log.Printf("检测到旧版系统 PAC 残留，正在迁移为手动代理串联模式（%s -> %s）", a.p.addr, a.upstreamLabel())
		if err := enableSystemProxy(a.p.addr, a.pacURL, a.backupPath); err != nil {
			log.Printf("迁移旧版 PAC 失败：%v", err)
		}
	}
}

func (a *app) saveConfigLocked() {
	if err := os.MkdirAll(filepath.Dir(a.configPath), 0o700); err != nil {
		return
	}
	b, err := json.MarshalIndent(a.cfg, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(a.configPath, b, 0o600)
}

func (a *app) applyDetectionLocked(det upstreamDetection) {
	_ = a.p.setUpstream(det.Proxy)
	a.cfg.UpstreamMode = "auto"
	a.cfg.UpstreamProxy = det.Proxy
	a.upstreamSource = det.Source
	a.upstreamAlive = det.Alive
	a.tunAdapter = det.TUN
	now := time.Now()
	a.lastAutoDetect = now
	a.aliveCheckedAt = now
}

func formatAutoDetectMessage(det upstreamDetection) string {
	if det.Proxy != "" {
		return fmt.Sprintf("已自动连接上游代理：%s（来源：%s）", det.Proxy, det.Source)
	}
	if det.TUN != "" {
		return fmt.Sprintf("未检测到本地代理端口，已切换为直连（检测到加速器 / TUN 网卡：%s）", det.TUN)
	}
	return "未检测到本地代理端口，已切换为直连回源模式。"
}

func (a *app) triggerDetectUpstream() (string, error) {
	return a.runAutoDetectWithHint("")
}

func (a *app) runAutoDetectWithHint(rawHint string) (string, error) {
	a.invalidateSystem()
	sys := a.readSysState()

	a.cfgMu.Lock()
	prev := a.cfg.UpstreamProxy
	a.cfgMu.Unlock()

	if strings.TrimSpace(rawHint) != "" {
		if norm, u, err := normalizeUpstreamURL(rawHint); err == nil && u != nil && !isSelfHostPort(u.Host, a.p.addr) {
			prev = norm
		}
	}

	det := a.detectUpstream(sys, prev)

	a.cfgMu.Lock()
	a.cliOverride = false
	a.applyDetectionLocked(det)
	a.saveConfigLocked()
	a.cfgMu.Unlock()

	return formatAutoDetectMessage(det), nil
}

func (a *app) upstreamSnapshot() (mode, src, tun string, alive bool) {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	mode = a.cfg.UpstreamMode
	if mode == "" {
		mode = "auto"
	}
	return mode, a.upstreamSource, a.tunAdapter, a.upstreamAlive
}

func (a *app) getUpstreamMode() string {
	mode, _, _, _ := a.upstreamSnapshot()
	return mode
}

func (a *app) upstreamLabel() string {
	if up := a.p.getUpstream(); up != "" {
		return up
	}
	_, _, tun, _ := a.upstreamSnapshot()
	if tun != "" {
		return "TUN / 加速器透明通道 (" + tun + ")"
	}
	return "直连回源（未检测到本地代理）"
}

// autoSyncUpstream 处于自动模式时，后台检测并随系统代理、常见本地端口或加速器/TUN 状态自动切换上游通道。
func (a *app) autoSyncUpstream(sys sysState) {
	a.cfgMu.Lock()
	cli := a.cliOverride
	mode := a.cfg.UpstreamMode
	if mode == "" {
		mode = "auto"
	}
	curProxy := a.cfg.UpstreamProxy
	lastAuto := a.lastAutoDetect
	lastAliveCheck := a.aliveCheckedAt
	a.cfgMu.Unlock()

	if cli {
		return
	}

	if mode != "auto" {
		if mode == "custom" && curProxy != "" && time.Since(lastAliveCheck) > 4*time.Second {
			if a.detecting.CompareAndSwap(false, true) {
				go func(addr string) {
					defer a.detecting.Store(false)
					alive := isUpstreamURLAlive(addr, 120*time.Millisecond)
					tun := detectTUNAdapter()
					a.cfgMu.Lock()
					if a.cfg.UpstreamMode == "custom" && a.cfg.UpstreamProxy == addr {
						a.upstreamAlive = alive
						a.tunAdapter = tun
						a.aliveCheckedAt = time.Now()
					}
					a.cfgMu.Unlock()
				}(curProxy)
			}
		}
		return
	}

	// 快速路径：未接管且系统存在非本机的活跃手动代理时立即跟随
	if sys.ManualProxy != "" && !sys.ProxyEnabled {
		if ex := extractUpstreamURL(sys.ManualProxy); ex != "" && ex != curProxy && isUpstreamURLAlive(ex, 120*time.Millisecond) {
			if err := a.p.setUpstream(ex); err == nil {
				tun := detectTUNAdapter()
				a.cfgMu.Lock()
				if a.cfg.UpstreamMode == "auto" && !a.cliOverride {
					a.applyDetectionLocked(upstreamDetection{
						Proxy:  ex,
						Source: "系统代理",
						Alive:  true,
						TUN:    tun,
					})
					a.saveConfigLocked()
					log.Printf("已自动同步系统上游代理节点：%s", ex)
				}
				a.cfgMu.Unlock()
				return
			}
		}
	}

	if time.Since(lastAuto) <= 8*time.Second {
		return
	}
	if !a.detecting.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer a.detecting.Store(false)
		cur := a.p.getUpstream()
		if cur != "" && isUpstreamURLAlive(cur, 120*time.Millisecond) {
			tun := detectTUNAdapter()
			a.cfgMu.Lock()
			if a.cfg.UpstreamMode == "auto" && !a.cliOverride && a.p.getUpstream() == cur {
				now := time.Now()
				a.upstreamAlive = true
				a.aliveCheckedAt = now
				a.lastAutoDetect = now
				a.tunAdapter = tun
			}
			a.cfgMu.Unlock()
			return
		}

		det := a.detectUpstream(a.readSysState(), "")
		a.cfgMu.Lock()
		defer a.cfgMu.Unlock()
		if a.cfg.UpstreamMode != "auto" || a.cliOverride {
			return
		}
		prevTUN := a.tunAdapter
		prevProxy := a.cfg.UpstreamProxy
		a.applyDetectionLocked(det)
		a.saveConfigLocked()
		if det.Proxy != prevProxy || det.TUN != prevTUN {
			if det.Proxy != "" {
				log.Printf("已自动切换上游代理通道：%s（%s）", det.Proxy, det.Source)
			} else if det.TUN != "" {
				log.Printf("已自动切换为 TUN / 加速器直连通道：%s", det.TUN)
			} else if prevProxy != "" {
				log.Printf("原上游代理 %s 已关闭，已自动切回直连模式", prevProxy)
			}
		}
	}()
}

func (a *app) isProxyActive() bool {
	return a.system().ProxyEnabled
}

// trusted 挡住网页对本地接口的跨站调用：Host 必须是本机地址（防 DNS 重绑定），
// 写操作必须带自定义请求头（别的网站发不出这种请求）。
func (a *app) trusted(r *http.Request) bool {
	return a.trustedFor(r, a.controlAddr())
}

func (a *app) trustedFor(r *http.Request, expectAddr string) bool {
	_, port, _ := net.SplitHostPort(expectAddr)
	if !slices.Contains([]string{expectAddr, "127.0.0.1:" + port, "localhost:" + port}, r.Host) {
		return false
	}
	return r.Method == http.MethodGet || r.Header.Get(csrfHeaderName) == "1"
}

func (a *app) status(w http.ResponseWriter, r *http.Request) {
	p := a.p
	sys := a.system()
	a.autoSyncUpstream(sys)
	mode, upSrc, tun, upAlive := a.upstreamSnapshot()
	tlsSessionCache := p.tr != nil && p.tr.TLSClientConfig != nil && p.tr.TLSClientConfig.ClientSessionCache != nil
	ramItems, ramBytes, ramMaxBytes := p.ram.Stats()
	writeJSON(w, map[string]any{
		"version":         appVersion,
		"addr":            p.addr,
		"adminAddr":       a.controlAddr(),
		"dualPort":        a.isDualPort(),
		"tlsSessionCache": tlsSessionCache,
		"pacURL":          a.pacURL,
		"windows":         runtime.GOOS == "windows",
		"certPath":        filepath.Join(p.certDir, "ca.crt"),
		"cacheDir":        p.getCacheRoot(),
		"defaultCacheDir": p.defaultCacheRoot(),
		"autoStart":       sys.AutoStart,
		"tray":            a.tray,
		"checkURL":        checkURL,
		"checkedAt":       p.checkedAt.Load(),
		"hits":            p.hits.Load(),
		"hits304":         p.hits304.Load(),
		"ramHits":         p.ramHits.Load(),
		"ramItems":        ramItems,
		"ramBytes":        ramBytes,
		"ramMaxBytes":     ramMaxBytes,
		"metaIndexReady":  p.metaIdxReady.Load(),
		"misses":          p.misses.Load(),
		"merged":          p.merged.Load(),
		"revalidated":     p.revalidated.Load(),
		"errors":          p.errors.Load(),
		"passthrough":     p.passthrough.Load(),
		"servedBytes":     p.servedBytes.Load(),
		"cachedFiles":     p.cachedFiles.Load(),
		"cachedBytes":     p.cachedBytes.Load(),
		"upstreamMode":    mode,
		"upstreamProxy":   p.getUpstream(),
		"upstreamSource":  upSrc,
		"upstreamAlive":   upAlive,
		"tunAdapter":      tun,
		"allowLAN":        p.allowLAN.Load(),
		"lanIPs":          a.cachedLANIPv4s(false),
		"lastPingMs":      a.lastPingMs.Load(),
		"gameBrowser":     a.getBrowserInfo(),
		"system":          sys,
		"logs":            a.logs.snapshot(),
	})
}

func (a *app) readSysState() sysState {
	if a.sysFn != nil {
		return a.sysFn()
	}
	pac, manual, enabled, legacyPAC, saved := proxyState(a.p.addr, a.pacURL, a.backupPath)
	return sysState{
		CertTrusted:   a.p.caTrusted(),
		OldCerts:      countCerts(a.isOldCA),
		ProxyEnabled:  enabled,
		LegacyPAC:     legacyPAC,
		PAC:           pac,
		ManualProxy:   manual,
		SavedUpstream: saved,
		AutoStart:     getAutoStart(),
	}
}

// system 读取证书和系统代理状态；要调用系统 API，所以最多 3 秒读一次。
func (a *app) system() sysState {
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Since(a.sysAt) > 3*time.Second {
		a.sys = a.readSysState()
		a.sysAt = time.Now()
	}
	return a.sys
}

func (a *app) invalidateSystem() {
	a.mu.Lock()
	a.sysAt = time.Time{}
	a.mu.Unlock()
}

func (a *app) usage(w http.ResponseWriter, r *http.Request) {
	if a.p.metaIdxReady.Load() && r.URL.Query().Get("rescan") != "1" {
		writeJSON(w, map[string]any{
			"files": a.p.cachedFiles.Load(),
			"bytes": a.p.cachedBytes.Load(),
		})
		return
	}
	files, size := a.p.scanCacheUsage()
	writeJSON(w, map[string]any{"files": files, "bytes": size})
}

func (a *app) cacheBreakdownHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.p.cacheBreakdown())
}

func (a *app) cacheDetectOldHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"candidates": a.p.detectOldCacheDirs()})
}

func (a *app) cacheImportProgressHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, a.getImportProgress())
}

func (a *app) action(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("action")
	cacheDir := a.p.getCacheRoot()
	var err error
	var msg string
	switch name {
	case "cert-install":
		msg = "根证书已安装。请彻底关闭所有浏览器窗口后再重新打开。"
		err = installCert(a.p.ca.Raw)
	case "cert-uninstall":
		msg = "根证书已移除。"
		_, err = removeCerts(func(der []byte) bool { return bytes.Equal(der, a.p.ca.Raw) })
	case "cert-cleanup":
		var n int
		n, err = removeCerts(a.isOldCA)
		msg = fmt.Sprintf("已清理 %d 张旧证书。", n)
	case "proxy-on", "pac-on":
		a.invalidateSystem()
		a.autoSyncUpstream(a.system())
		a.cfgMu.Lock()
		canAuto := !a.cliOverride && (a.cfg.UpstreamMode == "" || a.cfg.UpstreamMode == "auto")
		a.cfgMu.Unlock()
		if canAuto && !isUpstreamURLAlive(a.p.getUpstream(), 120*time.Millisecond) {
			_, _ = a.triggerDetectUpstream()
		}
		err = enableSystemProxy(a.p.addr, a.pacURL, a.backupPath)
		msg = fmt.Sprintf("已开启代理接管（%s）：静态资源本地缓存，游戏数据及其他软件走 %s。", a.p.addr, a.upstreamLabel())
	case "proxy-off", "pac-off":
		err = disableSystemProxy(a.p.addr, a.pacURL, a.backupPath, a.p.getUpstream())
		msg = "已关闭代理接管并恢复原系统代理。"
	case "proxy-reset":
		err = resetSystemProxy(a.p.addr, a.pacURL, a.backupPath, a.p.getUpstream())
		msg = "已重置并恢复系统代理设置。"
	case "detect-upstream":
		msg, err = a.triggerDetectUpstream()
	case "set-upstream":
		var body struct {
			Mode  string `json:"mode"`
			Proxy string `json:"proxy"`
		}
		if err = json.NewDecoder(r.Body).Decode(&body); err != nil {
			err = fmt.Errorf("参数格式错误：%w", err)
			break
		}
		msg, err = a.applyUpstreamConfig(body.Mode, body.Proxy)
	case "open-game", "launch-game":
		var body struct {
			RestartBrowser bool `json:"restartBrowser"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		var out launchOutcome
		out, err = a.launchGame(body.RestartBrowser)
		if err == nil && out.NeedRestart {
			writeJSON(w, map[string]any{
				"needRestart":  true,
				"browserLabel": out.BrowserLabel,
				"message":      out.Message,
			})
			return
		}
		msg = out.Message
	case "check-browser":
		var body struct {
			Mode           string `json:"mode"`
			RestartBrowser bool   `json:"restartBrowser"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		target := checkURL + "?t=" + strconv.FormatInt(time.Now().Unix(), 10) // 每次地址都不同，避免浏览器拿出缓存的旧结果
		mode := strings.ToLower(strings.TrimSpace(body.Mode))
		if mode == "isolated" {
			if !a.hasValidGameBrowser() {
				err = fmt.Errorf("请先在控制面板选择一个用于独立加速启动的浏览器或快捷方式")
				break
			}
			var out launchOutcome
			out, err = a.launchGameBrowser(target, true, body.RestartBrowser)
			if err == nil && out.NeedRestart {
				writeJSON(w, map[string]any{
					"needRestart":  true,
					"browserLabel": out.BrowserLabel,
					"message":      out.Message,
				})
				return
			}
			msg = out.Message
		} else if mode == "" && a.hasValidGameBrowser() && !a.system().ProxyEnabled {
			var out launchOutcome
			out, err = a.launchGameBrowser(target, true, body.RestartBrowser)
			if err == nil && out.NeedRestart {
				writeJSON(w, map[string]any{
					"needRestart":  true,
					"browserLabel": out.BrowserLabel,
					"message":      out.Message,
				})
				return
			}
			msg = out.Message
		} else {
			msg = "已在默认浏览器打开检测页。"
			err = openURL(target)
		}
	case "browser-pick":
		var body struct {
			Title string `json:"title"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		title := strings.TrimSpace(body.Title)
		if title == "" {
			title = "选择用于独立加速启动 GBF 的浏览器 (.exe) 或快捷方式 (.lnk)"
		}
		var picked string
		var cancelled bool
		picked, cancelled, err = pickBrowserFileDialog(title)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		if cancelled || strings.TrimSpace(picked) == "" {
			writeJSON(w, map[string]any{"cancelled": true})
			return
		}
		a.cfgMu.Lock()
		curProfMode := a.cfg.GameBrowserProfileMode
		a.cfgMu.Unlock()
		info, applyMsg, applyErr := a.applyGameBrowser(picked, "", curProfMode, false)
		if applyErr != nil {
			writeJSON(w, map[string]any{"error": applyErr.Error(), "browser": info})
			return
		}
		a.invalidateSystem()
		log.Print(applyMsg)
		writeJSON(w, map[string]any{"message": applyMsg, "browser": info})
		return
	case "browser-set":
		var body struct {
			Path        string `json:"path"`
			Label       string `json:"label"`
			ProfileMode string `json:"profileMode"`
			KeepPath    bool   `json:"keepPath"`
		}
		if r.Body == nil || json.NewDecoder(r.Body).Decode(&body) != nil {
			err = fmt.Errorf("参数格式错误")
			break
		}
		info, applyMsg, applyErr := a.applyGameBrowser(body.Path, body.Label, body.ProfileMode, body.KeepPath)
		if applyErr != nil {
			writeJSON(w, map[string]any{"error": applyErr.Error(), "browser": info})
			return
		}
		a.invalidateSystem()
		log.Print(applyMsg)
		writeJSON(w, map[string]any{"message": applyMsg, "browser": info})
		return
	case "browser-profile-clear":
		var removedDirs int
		var freedBytes int64
		removedDirs, freedBytes, err = clearBrowserProfiles(a.configBaseDir())
		a.cfgMu.Lock()
		info := a.refreshBrowserInfoLocked()
		a.cfgMu.Unlock()
		if err != nil {
			writeJSON(w, map[string]any{"error": err.Error(), "removedDirs": removedDirs, "freedBytes": freedBytes, "browser": info})
			return
		}
		if removedDirs == 0 && freedBytes == 0 {
			msg = "独立浏览器配置目录当前为空，无需清理。"
		} else {
			msg = fmt.Sprintf("已清理 %d 个独立浏览器配置目录，释放 %.1f MB 磁盘空间。", removedDirs, float64(freedBytes)/(1024*1024))
		}
		a.invalidateSystem()
		log.Print(msg)
		writeJSON(w, map[string]any{"message": msg, "removedDirs": removedDirs, "freedBytes": freedBytes, "browser": info})
		return
	case "open-folder":
		if err = os.MkdirAll(cacheDir, 0o755); err == nil {
			err = openFolder(cacheDir)
		}
	case "cache-clear":
		msg = "缓存已清空。"
		err = a.p.clearCache()
	case "cache-inspect":
		var checked, removed, reclaimed int64
		checked, removed, reclaimed, err = a.p.inspectCache()
		if err == nil {
			if removed == 0 {
				msg = fmt.Sprintf("缓存体检完成：共扫描 %d 个文件，全部健康（未发现损坏或异常缓存）。", checked)
			} else {
				msg = fmt.Sprintf("缓存体检完成：共扫描 %d 个文件，清理损坏/异常文件 %d 个（释放 %.1f KB）。", checked, removed, float64(reclaimed)/1024)
			}
		}
	case "cache-prune":
		days := 60
		var body struct {
			Days int `json:"days"`
		}
		if r.Body != nil && json.NewDecoder(r.Body).Decode(&body) == nil && body.Days >= 1 {
			days = body.Days
		}
		var removed, reclaimed int64
		removed, reclaimed, err = a.p.pruneColdCache(time.Duration(days) * 24 * time.Hour)
		if err == nil {
			msg = fmt.Sprintf("已清理超过 %d 天未访问的冷门缓存 %d 个，释放 %.1f MB。", days, removed, float64(reclaimed)/(1024*1024))
		}
	case "cache-clear-category":
		var body struct {
			Category string `json:"category"`
		}
		if r.Body == nil || json.NewDecoder(r.Body).Decode(&body) != nil || strings.TrimSpace(body.Category) == "" {
			err = fmt.Errorf("请指定要清理的资源分类")
			break
		}
		var removed, reclaimed int64
		var label string
		removed, reclaimed, label, err = a.p.clearCacheCategory(body.Category)
		if err == nil {
			msg = fmt.Sprintf("已清理「%s」分类缓存 %d 个文件，释放 %.1f MB。", label, removed, float64(reclaimed)/(1024*1024))
		}
	case "health-check":
		rep := a.runHealthCheck()
		a.invalidateSystem()
		writeJSON(w, rep)
		return
	case "cache-pick-folder":
		var body struct {
			Title string `json:"title"`
		}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		title := strings.TrimSpace(body.Title)
		if title == "" {
			title = "选择 GBF 静态资源缓存目录"
		}
		var picked string
		var cancelled bool
		picked, cancelled, err = pickFolderDialog(title)
		if err != nil {
			writeJSON(w, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, map[string]any{"path": picked, "cancelled": cancelled})
		return
	case "cache-import-preview":
		var body struct {
			Path     string `json:"path"`
			Strategy string `json:"strategy"`
		}
		if r.Body == nil || json.NewDecoder(r.Body).Decode(&body) != nil {
			err = fmt.Errorf("参数格式错误")
			break
		}
		var prev importPreview
		prev, err = a.p.previewImportCache(body.Path, body.Strategy)
		if err == nil {
			writeJSON(w, prev)
			return
		}
	case "cache-import-merge":
		var body struct {
			Path     string `json:"path"`
			Strategy string `json:"strategy"`
			Async    bool   `json:"async"`
		}
		if r.Body == nil || json.NewDecoder(r.Body).Decode(&body) != nil {
			err = fmt.Errorf("参数格式错误")
			break
		}
		if body.Async {
			if jErr := a.startImportJob(body.Path, body.Strategy); jErr != nil {
				err = jErr
				break
			}
			writeJSON(w, map[string]any{
				"started":  true,
				"progress": a.getImportProgress(),
			})
			return
		}
		var res importResult
		res, err = a.p.mergeCacheFrom(r.Context(), body.Path, body.Strategy, nil)
		if err == nil {
			if res.Message != "" {
				log.Print(res.Message)
			}
			a.invalidateSystem()
			writeJSON(w, res)
			return
		}
	case "cache-import-cancel":
		cancelled := a.cancelImportJob()
		if cancelled {
			msg = "已发送取消指令，正在安全收尾当前并入任务。"
		} else {
			msg = "当前没有正在进行的并入任务。"
		}
		writeJSON(w, map[string]any{"cancelled": cancelled, "message": msg})
		return
	case "set-cache-dir":
		if a.getImportProgress().Running {
			err = fmt.Errorf("正在并入旧缓存，请等待完成或先取消后再更改存储目录")
			break
		}
		var body struct {
			Path string `json:"path"`
		}
		if r.Body == nil || json.NewDecoder(r.Body).Decode(&body) != nil {
			err = fmt.Errorf("参数格式错误")
			break
		}
		var applied string
		applied, err = a.p.setCacheRoot(body.Path)
		if err == nil {
			a.cfgMu.Lock()
			if strings.TrimSpace(body.Path) == "" {
				a.cfg.CacheDir = ""
			} else {
				a.cfg.CacheDir = applied
			}
			a.saveConfigLocked()
			a.cfgMu.Unlock()
			if strings.TrimSpace(body.Path) == "" {
				msg = fmt.Sprintf("已恢复默认缓存目录：%s", applied)
			} else {
				msg = fmt.Sprintf("已切换缓存存储目录：%s（现有缓存文件：%d 个）", applied, a.p.cachedFiles.Load())
			}
			a.invalidateSystem()
			writeJSON(w, map[string]any{
				"message":         msg,
				"cacheDir":        a.p.getCacheRoot(),
				"defaultCacheDir": a.p.defaultCacheRoot(),
			})
			log.Print(msg)
			return
		}
	case "autostart-on":
		err = setAutoStart(true)
		if err == nil {
			msg = "已开启开机静默自启（随系统登录自动驻留托盘）。"
		}
	case "autostart-off":
		err = setAutoStart(false)
		if err == nil {
			msg = "已关闭开机自启。"
		}
	case "ping-upstream":
		_, msg, err = a.pingUpstream()
	case "lan-on":
		msg, err = a.applyAllowLAN(true)
	case "lan-off":
		msg, err = a.applyAllowLAN(false)
	case "show":
		a.showWindow()
	case "quit":
		msg = "程序已退出。"
		time.AfterFunc(120*time.Millisecond, a.exit) // 先把响应发回去
	default:
		http.NotFound(w, r)
		return
	}
	a.invalidateSystem()
	if err != nil {
		log.Printf("操作失败（%s）：%v", name, err)
		writeJSON(w, map[string]string{"error": err.Error()})
		return
	}
	if msg != "" {
		log.Print(msg)
	}
	writeJSON(w, map[string]string{"message": msg})
}

func (a *app) applyUpstreamConfig(mode, rawProxy string) (string, error) {
	mode = strings.TrimSpace(strings.ToLower(mode))
	if mode == "" {
		mode = "custom"
	}

	switch mode {
	case "direct":
		if err := a.p.setUpstream(""); err != nil {
			return "", err
		}
		tun := detectTUNAdapter()
		a.cfgMu.Lock()
		a.cliOverride = false
		a.cfg.UpstreamMode = "direct"
		a.upstreamSource = "手动直连"
		a.upstreamAlive = false
		a.tunAdapter = tun
		a.aliveCheckedAt = time.Now()
		a.saveConfigLocked()
		a.cfgMu.Unlock()
		return "已切换为直连回源模式（不再走代理节点）。", nil

	case "custom":
		if strings.TrimSpace(rawProxy) == "" {
			return "", fmt.Errorf("请输入上游代理地址（例如 127.0.0.1:7890 或 socks5://127.0.0.1:10808）")
		}
		if err := a.p.setUpstream(rawProxy); err != nil {
			return "", err
		}
		up := a.p.getUpstream()
		if up == "" {
			return "", fmt.Errorf("自定义模式请填写有效的代理端口（如需直连请选择直连模式）")
		}
		alive := isUpstreamURLAlive(up, 150*time.Millisecond)
		tun := detectTUNAdapter()
		a.cfgMu.Lock()
		a.cliOverride = false
		a.cfg.UpstreamMode = "custom"
		a.cfg.UpstreamProxy = up
		a.upstreamSource = "手动指定"
		a.upstreamAlive = alive
		a.tunAdapter = tun
		a.aliveCheckedAt = time.Now()
		a.saveConfigLocked()
		a.cfgMu.Unlock()
		if !alive {
			return "已设定上游代理节点：" + up + "（注意：该端口当前未响应，请确认代理软件已启动）", nil
		}
		return "已设定上游代理节点：" + up, nil

	case "auto":
		return a.runAutoDetectWithHint(rawProxy)

	default:
		return "", fmt.Errorf("未知上游模式：%s", mode)
	}
}

// showWindow 打开或前置唤醒控制面板窗口（严格单窗口）。
func (a *app) showWindow() {
	if err := launchWindow("http://" + a.controlAddr() + "/"); err != nil {
		log.Printf("无法打开窗口：%v", err)
	}
}

func (a *app) localProxyHostPort() string {
	host, port, err := net.SplitHostPort(strings.TrimSpace(a.p.addr))
	if err != nil || port == "" {
		return a.p.addr
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func (a *app) localPACURL() string {
	return "http://" + a.localProxyHostPort() + "/proxy.pac"
}

// pacFallbackAddr 为独立启动浏览器的 PAC 脚本选择非 CDN 流量的回退路由：
// 优先使用 GBF-Dock 当前接入的上游代理（如 FlClash / v2rayN），其次检查系统手动代理，直连或 TUN 模式返回 DIRECT。
func (a *app) pacFallbackAddr() string {
	if up := a.p.getUpstream(); up != "" {
		return upstreamToPACReturn(up)
	}
	if a.getUpstreamMode() == "direct" {
		return "DIRECT"
	}
	_, manual := pacState()
	if isProxyServerSelf(manual, a.p.addr) {
		manual = savedBackupServer(a.backupPath, a.p.addr)
	}
	if manual != "" && !isProxyServerSelf(manual, a.p.addr) {
		return pacFallback(manual)
	}
	return "DIRECT"
}

func (a *app) configBaseDir() string {
	if d := strings.TrimSpace(filepath.Dir(a.configPath)); d != "" && d != "." {
		return d
	}
	return a.p.dir
}

func (a *app) refreshBrowserInfoLocked() gameBrowserInfo {
	info := inspectGameBrowser(a.cfg.GameBrowserPath, a.cfg.GameBrowserLabel, a.cfg.GameBrowserProfileMode, a.configBaseDir(), a.localPACURL())
	a.browserInfo = info
	return info
}

func (a *app) getBrowserInfo() gameBrowserInfo {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	info := a.browserInfo
	if info.PACURL == "" {
		info.PACURL = a.localPACURL()
	}
	if info.ProfileMode == "" {
		info.ProfileMode = normalizeBrowserProfileMode(a.cfg.GameBrowserProfileMode)
	}
	return info
}

func (a *app) hasValidGameBrowser() bool {
	info := a.getBrowserInfo()
	return info.Configured && info.Error == "" && info.TargetExe != ""
}

func (a *app) browsersHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"detected": detectInstalledBrowsers(),
		"selected": a.getBrowserInfo(),
	})
}

func (a *app) applyGameBrowser(rawPath, rawLabel, rawProfileMode string, keepPath bool) (gameBrowserInfo, string, error) {
	a.cfgMu.Lock()
	curPath := a.cfg.GameBrowserPath
	curLabel := a.cfg.GameBrowserLabel
	curMode := normalizeBrowserProfileMode(a.cfg.GameBrowserProfileMode)
	a.cfgMu.Unlock()

	if keepPath {
		rawPath = curPath
		rawLabel = curLabel
	}
	nextMode := curMode
	if strings.TrimSpace(rawProfileMode) != "" {
		nextMode = normalizeBrowserProfileMode(rawProfileMode)
	}

	trimmed := strings.TrimSpace(rawPath)
	if trimmed == "" {
		a.cfgMu.Lock()
		a.cfg.GameBrowserPath = ""
		a.cfg.GameBrowserLabel = ""
		a.cfg.GameBrowserProfileMode = nextMode
		info := a.refreshBrowserInfoLocked()
		a.saveConfigLocked()
		a.cfgMu.Unlock()
		if keepPath {
			modeTxt := "保留原浏览器配置（登录态/扩展）"
			if nextMode == browserProfileModeIsolated {
				modeTxt = "独立空白配置（可与主浏览器双开）"
			}
			return info, "已切换默认启动策略为「" + modeTxt + "」（尚未选定具体浏览器）", nil
		}
		return info, "已恢复使用系统默认浏览器启动游戏", nil
	}

	info := inspectGameBrowser(trimmed, rawLabel, nextMode, a.configBaseDir(), a.localPACURL())
	if info.Error != "" {
		return info, "", fmt.Errorf("%s", info.Error)
	}

	a.cfgMu.Lock()
	a.cfg.GameBrowserPath = info.Path
	a.cfg.GameBrowserLabel = info.Label
	a.cfg.GameBrowserProfileMode = info.ProfileMode
	a.browserInfo = info
	a.saveConfigLocked()
	a.cfgMu.Unlock()

	modeDesc := "保留原浏览器配置与登录态，关闭浏览器后自动清除加速参数"
	if info.HasCustomDir {
		modeDesc = "沿用快捷方式自带 --user-data-dir 与登录态，关闭后自动清除加速参数"
	} else if info.ProfileMode == browserProfileModeIsolated {
		modeDesc = "独立空白配置目录（不冲突可双开），可在配置页随时清理"
	}
	return info, fmt.Sprintf("已选定独立加速浏览器：%s（%s）", info.Label, modeDesc), nil
}

type launchOutcome struct {
	Message      string
	NeedRestart  bool
	BrowserLabel string
}

func (a *app) launchGameBrowser(targetURL string, forceTargetURL, restartBrowser bool) (launchOutcome, error) {
	a.cfgMu.Lock()
	rawPath := a.cfg.GameBrowserPath
	rawLabel := a.cfg.GameBrowserLabel
	profMode := normalizeBrowserProfileMode(a.cfg.GameBrowserProfileMode)
	a.cfgMu.Unlock()

	if strings.TrimSpace(rawPath) == "" {
		return launchOutcome{}, fmt.Errorf("未配置独立加速浏览器")
	}
	pacURL := a.localPACURL()
	info := inspectGameBrowser(rawPath, rawLabel, profMode, a.configBaseDir(), pacURL)

	a.cfgMu.Lock()
	if strings.EqualFold(a.cfg.GameBrowserPath, rawPath) {
		a.browserInfo = info
	}
	a.cfgMu.Unlock()

	if info.Error != "" {
		return launchOutcome{}, fmt.Errorf("%s", info.Error)
	}

	explicitDir := ""
	checkConflict := false
	if info.HasCustomDir {
		explicitDir = extractUserDataDirFromArgs(info.RawArgs)
		checkConflict = true
	} else if info.ProfileMode == browserProfileModeOriginal {
		checkConflict = true
	}

	if checkConflict && isBrowserInstanceRunning(info.TargetExe, explicitDir) {
		if !restartBrowser {
			return launchOutcome{
				NeedRestart:  true,
				BrowserLabel: info.Label,
				Message:      fmt.Sprintf("检测到「%s」当前正在运行（含后台进程）。保留原配置加速启动需先关闭它的旧进程，这样 --proxy-pac-url 才会生效；关闭浏览器后该参数会自动清除。", info.Label),
			}, nil
		}
		if err := closeBrowserProcessesGracefully(info.TargetExe, explicitDir); err != nil {
			return launchOutcome{}, err
		}
	}

	args, hasCustomProfile, err := buildBrowserLaunchArgs(info.TargetExe, info.RawArgs, pacURL, info.ProfileDir, targetURL, forceTargetURL, info.ProfileMode)
	if err != nil {
		return launchOutcome{}, err
	}
	if !hasCustomProfile && info.ProfileMode == browserProfileModeIsolated && info.ProfileDir != "" {
		if mErr := os.MkdirAll(info.ProfileDir, 0o755); mErr != nil {
			return launchOutcome{}, fmt.Errorf("创建浏览器独立配置目录失败：%w", mErr)
		}
	}
	if err := launchBrowserProcess(info.TargetExe, args, info.WorkDir); err != nil {
		return launchOutcome{}, err
	}

	profTag := "保留原浏览器配置/登录态"
	if hasCustomProfile {
		profTag = "快捷方式原配置目录"
	} else if info.ProfileMode == browserProfileModeIsolated {
		profTag = "独立空白配置目录"
	}
	actionDesc := "已在独立加速浏览器中打开游戏"
	if forceTargetURL {
		actionDesc = "已在独立加速浏览器中打开连通检测页"
	}
	return launchOutcome{
		Message: fmt.Sprintf("%s：%s（%s · 参数随窗口关闭自动失效）", actionDesc, info.Label, profTag),
	}, nil
}

func (a *app) launchGame(restartBrowser bool) (launchOutcome, error) {
	if a.hasValidGameBrowser() {
		return a.launchGameBrowser(gameURL, false, restartBrowser)
	}
	if err := openURL(gameURL); err != nil {
		return launchOutcome{}, err
	}
	return launchOutcome{Message: "已在系统默认浏览器打开碧蓝幻想。"}, nil
}

// isOldCA：以前版本（含旧版 GBF Cache）或别的解压目录生成的历史根证书，不是当前正在用的这张。
func (a *app) isOldCA(der []byte) bool {
	if bytes.Equal(der, a.p.ca.Raw) {
		return false
	}
	c, err := x509.ParseCertificate(der)
	return err == nil && (c.Subject.CommonName == caName || c.Subject.CommonName == legacyCAName)
}

func (a *app) exit() { a.once.Do(func() { close(a.quit) }) }

func (a *app) cleanupOnExit() {
	a.cleanOnce.Do(func() {
		a.p.flushMetaIndex()
		closePanelWindow(panelTitle)
		if err := disableSystemProxy(a.p.addr, a.pacURL, a.backupPath, a.p.getUpstream()); err != nil {
			log.Printf("退出还原系统代理失败：%v", err)
		}
		removeTray()
	})
}

// askRunningInstanceToShow：端口已被占用时，看看是不是本程序已在运行，是就让它打开窗口。
func askRunningInstanceToShow(addr string) bool {
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/show", nil)
	if err != nil {
		return false
	}
	req.Header.Set(csrfHeaderName, "1")
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// caTrusted 用系统证书库验证一张本程序签发的证书，以此判断根证书是否已被信任。
func (p *proxy) caTrusted() bool {
	const probe = "prd-game-a-granbluefantasy.akamaized.net"
	leaf, err := p.leaf(probe)
	if err != nil {
		return false
	}
	c, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		return false
	}
	_, err = c.Verify(x509.VerifyOptions{DNSName: probe}) // 不指定 Roots = 使用系统证书库
	return err == nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

// logRing 保留最近 200 行日志给界面显示。
type logRing struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRing) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, strings.TrimRight(string(b), "\n"))
	if len(l.lines) > 200 {
		l.lines = l.lines[len(l.lines)-200:]
	}
	return len(b), nil
}

func (l *logRing) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.lines)
}
