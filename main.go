// GBF-Dock：碧蓝幻想（Granblue Fantasy）静态资源本地缓存代理。
//
// 只解密并缓存 CDN 静态资源（prd-game-a*-granbluefantasy.akamaized.net 等上的 JS、图片、动画、音频）。
// 游戏 API（game.granbluefantasy.jp）及其他软件流量（如桌面端 Gemini）绝不解密，原样经上游代理节点转发。
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	appName             = "GBF-Dock"
	appVersion          = "1.0.0"
	panelTitle          = appName + " v" + appVersion + " 控制面板"
	defaultUpstream     = "http://127.0.0.1:7890"
	caName              = "GBF-Dock Local CA"
	legacyCAName        = "gbf-cache local CA"
	appConfigDirName    = "GBF-Dock"
	legacyConfigDirName = "GBFCache"
	csrfHeaderName      = "X-GBF-Dock"
	deadShardTTL        = 30 * time.Minute
)

var (
	// 碧蓝幻想静态资源用的 Akamai CDN：prd-game-a*-granbluefantasy(-steam)|gbf、*.granbluefantasy(-steam)、*.gbf
	assetHost = regexp.MustCompile(`^(prd-game-a\d*-(granbluefantasy(-steam)?|gbf)|([a-z0-9-]+\.)*(granbluefantasy(-steam)?|gbf))\.akamaized\.net$`)
	// 路径里的 10 位时间戳视为版本号（如 /assets_en/1729002592/js/），和查询串一样不参与决定缓存位置
	versionSegment = regexp.MustCompile(`/1\d{9}/`)
	// 多 CDN 分片节点归一化（prd-game-a1~a5 -> prd-game-a），避免不同分片重复缓存并用于失效分片自动回退主域名
	cdnShardPrefix = regexp.MustCompile(`(?i)^(https?://)prd-game-a\d+-(granbluefantasy(?:-steam)?|gbf)\.akamaized\.net`)
)

func normalizeCDNShardURL(raw string) string {
	return cdnShardPrefix.ReplaceAllString(raw, "${1}prd-game-a-${2}.akamaized.net")
}

func extractHostFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

var hopHeaders = map[string]bool{
	"Connection": true, "Proxy-Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true,
	"Proxy-Authorization": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
	"Content-Length": true,
}

type appConfig struct {
	UpstreamMode  string `json:"upstreamMode"`       // "auto" | "custom" | "direct"
	UpstreamProxy string `json:"upstreamProxy"`      // 默认 ""（由自动检测决定）
	AllowLAN      bool   `json:"allowLAN"`           // 是否允许局域网手机/iPad共享缓存
	CacheDir      string `json:"cacheDir,omitempty"` // 自定义缓存存储目录（为空则用默认程序目录下的 cache）
}

type proxy struct {
	dir, addr string
	certDir   string // ca.crt / ca.key 所在目录
	cacheMu   sync.RWMutex
	cacheDir  string // 自定义缓存根目录（为空时使用 filepath.Join(dir, "cache")）
	ttl       time.Duration
	ca        *x509.Certificate
	caKey     *ecdsa.PrivateKey
	caPEM     []byte
	tlsConfig *tls.Config
	dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	tr        *http.Transport // CDN 静态资源拉取
	forwardTr *http.Transport // 非 CDN 普通 HTTP 请求透传
	mitm      chanListener
	local     http.Handler // 发给本机的请求：控制面板、接口、PAC、证书下载
	certs     sync.Map     // host -> *tls.Certificate
	fetching  sync.Map     // 缓存文件路径 -> 下载完成时关闭的 chan
	inflight  sync.Map     // 缓存文件路径 -> 正在后台验证
	metaIdx           sync.Map // 缓存文件路径 -> *cachedMeta
	ram               *slruCache
	allowLAN          atomic.Bool
	adminAddrMu       sync.RWMutex
	adminAddr         string
	metaIdxReady      atomic.Bool
	metaIndexDirty    atomic.Bool
	metaIndexWrites   atomic.Int64
	metaLastSaved     atomic.Int64
	metaSaveScheduled atomic.Bool
	metaSaveMu        sync.Mutex
	metaUpdateMu      sync.Mutex

	upMu        sync.RWMutex
	upstreamURL *url.URL
	upstreamRaw string

	usageMu                  sync.Mutex
	cachedFiles, cachedBytes atomic.Int64

	hits, misses, merged, revalidated, errors, passthrough, servedBytes, hits304, ramHits atomic.Int64
	checkedAt                                                                    atomic.Int64 // 最近一次“检测浏览器”通过的时间
	seenHosts                                                                    sync.Map     // 已收到过请求的 CDN 域名
	deadShards                                                                   sync.Map     // 已确认下线的旧分片 host -> time.Time
}

// 缓存文件第一行
type entryMeta struct {
	URL        string      `json:"url"`
	Header     http.Header `json:"header"`
	LooseQuery bool        `json:"looseQuery,omitempty"` // 仅原始 CDN 目录导入时为 true，允许忽略缺失的查询串差异
}

func (p *proxy) defaultCacheRoot() string {
	return filepath.Join(p.dir, "cache")
}

func (p *proxy) getCacheRoot() string {
	p.cacheMu.RLock()
	d := p.cacheDir
	p.cacheMu.RUnlock()
	if d != "" {
		return d
	}
	return p.defaultCacheRoot()
}

func (p *proxy) setCacheRoot(dir string) (string, error) {
	trimmed := strings.TrimSpace(dir)
	if trimmed == "" {
		p.flushMetaIndex()
		p.cacheMu.Lock()
		p.cacheDir = ""
		p.cacheMu.Unlock()
		if p.ram != nil {
			p.ram.Clear()
		}
		p.metaIdx.Clear()
		p.metaIdxReady.Store(false)
		p.metaIndexDirty.Store(false)
		if _, _, ok := p.loadMetaIndex(); !ok {
			p.scanCacheUsage()
		}
		return p.defaultCacheRoot(), nil
	}
	abs, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("缓存路径无效：%w", err)
	}
	abs = filepath.Clean(abs)
	if p.certDir != "" {
		if isSameOrNestedPath(abs, p.certDir) {
			return "", errors.New("缓存目录不能设在证书目录内")
		}
		cfgRoot := filepath.Dir(p.certDir)
		cfgBase := filepath.Base(cfgRoot)
		if sameCleanPath(abs, cfgRoot) || ((strings.EqualFold(cfgBase, appConfigDirName) || strings.EqualFold(cfgBase, legacyConfigDirName)) && isSameOrNestedPath(abs, cfgRoot)) {
			return "", errors.New("缓存目录不能设在程序配置目录内")
		}
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return "", fmt.Errorf("无法创建缓存目录：%w", err)
	}
	probe, err := os.CreateTemp(abs, "probe-*")
	if err != nil {
		return "", fmt.Errorf("缓存目录不可写：%w", err)
	}
	probeName := probe.Name()
	_ = probe.Close()
	_ = os.Remove(probeName)

	p.flushMetaIndex()
	p.cacheMu.Lock()
	p.cacheDir = abs
	p.cacheMu.Unlock()
	if p.ram != nil {
		p.ram.Clear()
	}
	p.metaIdx.Clear()
	p.metaIdxReady.Store(false)
	p.metaIndexDirty.Store(false)
	if _, _, ok := p.loadMetaIndex(); !ok {
		p.scanCacheUsage()
	}
	return abs, nil
}

func main() {
	exe, _ := os.Executable()
	addr := flag.String("addr", "127.0.0.1:8765", "监听地址")
	adminAddrFlag := flag.String("admin-addr", "", "本机控制面板监听地址（默认自动使用代理端口+1，仅绑定 127.0.0.1）")
	dir := flag.String("dir", filepath.Dir(exe), "数据目录（存放 cache/）")
	ttl := flag.Duration("ttl", 24*time.Hour, "缓存超过这个时长后，下次使用时在后台向 CDN 确认是否有更新（不阻塞加载）")
	upstreamFlag := flag.String("upstream-proxy", "", "上游代理地址（默认自动检测系统代理、本地代理端口或加速器/TUN；可填 direct 直连）")
	minimized := flag.Bool("minimized", false, "启动后静默驻留托盘，不自动弹出控制面板窗口")
	flag.Parse()

	upstreamSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "upstream-proxy" {
			upstreamSet = true
		}
	})

	logs := &logRing{}
	log.SetFlags(log.Ltime)
	log.SetOutput(io.MultiWriter(logs, os.Stderr)) // 图形版没有控制台，日志在界面里看

	resolvedAdmin, err := resolveAdminAddr(*addr, *adminAddrFlag)
	if err != nil {
		fatal(err.Error())
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		if (resolvedAdmin != "" && askRunningInstanceToShow(resolvedAdmin)) || askRunningInstanceToShow(*addr) {
			return // 已经在运行：让它把窗口打开即可
		}
		fatal(fmt.Sprintf("无法使用 %s：%v\n\n可能有其他程序占用了这个端口。", *addr, err))
	}
	var adminLn net.Listener
	if resolvedAdmin != "" && !isSelfHostPort(resolvedAdmin, *addr) {
		adminLn, err = net.Listen("tcp", resolvedAdmin)
		if err != nil {
			_ = ln.Close()
			if askRunningInstanceToShow(resolvedAdmin) {
				return
			}
			fatal(fmt.Sprintf("无法使用控制面板端口 %s：%v\n\n可能有其他程序占用了这个端口，可通过 -admin-addr 指定其他端口。", resolvedAdmin, err))
		}
	}

	p, err := newProxy(*dir, certsDir(*dir), *addr, *ttl)
	if err != nil {
		fatal(err.Error())
	}
	a := newApp(p, logs)
	if adminLn != nil {
		a.setAdminAddr(resolvedAdmin)
	}
	a.initUpstream(upstreamSet, *upstreamFlag)

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		a.exit()
	}()

	a.tray = startTray(a)
	if hint := filepath.Join(filepath.Dir(p.certDir), "tray-hint-shown"); a.tray && !*minimized && !fileExists(hint) {
		trayNotify("GBF-Dock 在托盘里运行", "单击图标可快速查看命中率与缓存大小，双击打开控制面板，右键彻底退出。")
		os.WriteFile(hint, nil, 0o644) // 只提醒第一次
	}
	a.serveListener(ln)
	if adminLn != nil {
		a.serveAdminListener(adminLn)
	}
	if a.getAllowLAN() {
		if msg, lErr := a.applyAllowLAN(true); lErr != nil {
			log.Printf("启动开启局域网共享失败（已保持仅本机监听）：%v", lErr)
		} else {
			log.Print(msg)
		}
	}
	go p.logStats()
	log.Printf("GBF-Dock v%s 已启动 数据 %s · 控制 %s，缓存目录：%s，上游代理：%s", appVersion, *addr, a.controlAddr(), p.getCacheRoot(), a.upstreamLabel())
	if !*minimized || !a.tray {
		a.showWindow()
	}
	<-a.quit
	a.cleanupOnExit()
}

// migrateLegacyUserConfig 从旧版 GBFCache 用户配置目录（%APPDATA%\GBFCache）平滑迁移证书与配置至 GBF-Dock 目录。
// 只复制新目录中尚不存在的文件，保留原目录完整不动，老用户升级无需重装证书或重设参数。
func migrateLegacyUserConfig(base, newCfgDir string) {
	legacyCfgDir := filepath.Join(base, legacyConfigDirName)
	st, err := os.Stat(legacyCfgDir)
	if err != nil || !st.IsDir() {
		return
	}
	newCertDir := filepath.Join(newCfgDir, "certs")
	legacyCertDir := filepath.Join(legacyCfgDir, "certs")
	if !fileExists(filepath.Join(newCertDir, "ca.crt")) && fileExists(filepath.Join(legacyCertDir, "ca.crt")) {
		if mErr := migrateCA(legacyCertDir, newCertDir); mErr != nil {
			log.Printf("从旧版 %s 迁移证书失败：%v", legacyConfigDirName, mErr)
		} else {
			log.Printf("已从旧版 %s 自动迁移根证书至 %s，无需重新安装", legacyConfigDirName, newCertDir)
		}
	}
	if err := os.MkdirAll(newCfgDir, 0o700); err != nil {
		return
	}
	for _, name := range []string{"config.json", "proxy_backup.json", "window.json", "tray-hint-shown"} {
		dst := filepath.Join(newCfgDir, name)
		src := filepath.Join(legacyCfgDir, name)
		if fileExists(dst) || !fileExists(src) {
			continue
		}
		if b, rErr := os.ReadFile(src); rErr == nil {
			_ = os.WriteFile(dst, b, 0o600)
		}
	}
}

// certsDir：证书放在固定的用户目录（Windows 为 %APPDATA%\GBF-Dock\certs），
// 换新版本、解压到别的文件夹都沿用同一张证书，不用重新安装。
// 首次从旧版 GBFCache 或程序目录 certs\ 升级时会自动迁移过来继续使用。
func certsDir(dataDir string) string {
	old := filepath.Join(dataDir, "certs")
	base, err := os.UserConfigDir()
	if err != nil {
		return old
	}
	cfgDir := filepath.Join(base, appConfigDirName)
	migrateLegacyUserConfig(base, cfgDir)
	dir := filepath.Join(cfgDir, "certs")
	if !fileExists(filepath.Join(dir, "ca.crt")) && fileExists(filepath.Join(old, "ca.crt")) {
		if err := migrateCA(old, dir); err != nil {
			log.Printf("无法迁移旧证书，继续使用 %s：%v", old, err)
			return old
		}
		log.Printf("已把证书从 %s 迁移到 %s，无需重新安装", old, dir)
	}
	return dir
}

func migrateCA(from, to string) error {
	if err := os.MkdirAll(to, 0o700); err != nil {
		return err
	}
	// 先写私钥再写证书：有 ca.crt 就一定有 ca.key，中途失败也不会留下半套
	for _, f := range []struct {
		name string
		mode os.FileMode
	}{{"ca.key", 0o600}, {"ca.crt", 0o644}} {
		b, err := os.ReadFile(filepath.Join(from, f.name))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(to, f.name), b, f.mode); err != nil {
			return err
		}
	}
	return nil
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

func newProxy(dir, certDir, addr string, ttl time.Duration) (*proxy, error) {
	p := &proxy{
		dir:     dir,
		certDir: certDir,
		addr:    addr,
		ttl:     ttl,
		mitm:    make(chanListener),
		local:   http.NotFoundHandler(),
		ram:     newSLRUCache(defaultRAMCacheMaxBytes, defaultRAMCacheMaxItemBytes),
	}
	p.dial = (&net.Dialer{Timeout: 15 * time.Second}).DialContext
	if err := p.loadCA(); err != nil {
		return nil, fmt.Errorf("无法创建或读取根证书（%s）：%w", certDir, err)
	}
	p.tlsConfig = &tls.Config{
		NextProtos: []string{"h2", "http/1.1"},
		GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if !assetHost.MatchString(h.ServerName) {
				return nil, fmt.Errorf("拒绝为 %q 签发证书", h.ServerName)
			}
			return p.leaf(h.ServerName)
		},
	}
	p.tr = &http.Transport{
		Proxy:                 p.transportProxy,
		DialContext:           func(ctx context.Context, n, a string) (net.Conn, error) { return p.dial(ctx, n, a) },
		TLSClientConfig:       &tls.Config{ClientSessionCache: tls.NewLRUClientSessionCache(64)},
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
	}
	p.forwardTr = &http.Transport{
		Proxy:                 p.transportProxy,
		DialContext:           p.dialForForward,
		TLSClientConfig:       &tls.Config{ClientSessionCache: tls.NewLRUClientSessionCache(64)},
		ForceAttemptHTTP2:     true,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	if _, _, ok := p.loadMetaIndex(); !ok {
		go p.bootstrapMetaIndex()
	}
	go p.startMetaIndexFlusher()
	// 解密后的 CDN 连接交给这个内部 server 处理（支持 HTTP/2）
	go (&http.Server{Handler: p}).Serve(p.mitm)
	return p, nil
}

func (p *proxy) transportProxy(req *http.Request) (*url.URL, error) {
	if req != nil && req.URL != nil && isLoopbackHost(req.URL.Hostname()) {
		return nil, nil
	}
	p.upMu.RLock()
	u := p.upstreamURL
	p.upMu.RUnlock()
	return u, nil
}

func (p *proxy) getUpstream() string {
	p.upMu.RLock()
	defer p.upMu.RUnlock()
	return p.upstreamRaw
}

func (p *proxy) setUpstream(raw string) error {
	norm, u, err := normalizeUpstreamURL(raw)
	if err != nil {
		return err
	}
	if u != nil && isSelfHostPort(u.Host, p.addr) {
		return fmt.Errorf("上游代理不能指向本程序监听地址 %s（会造成死循环）", p.addr)
	}
	p.upMu.Lock()
	changed := p.upstreamRaw != norm
	p.upstreamRaw = norm
	p.upstreamURL = u
	p.upMu.Unlock()
	if changed {
		if p.tr != nil {
			p.tr.CloseIdleConnections()
		}
		if p.forwardTr != nil {
			p.forwardTr.CloseIdleConnections()
		}
	}
	return nil
}

func normalizeUpstreamURL(raw string) (string, *url.URL, error) {
	s := strings.TrimSpace(raw)
	if s == "" || strings.EqualFold(s, "direct") || strings.EqualFold(s, "none") {
		return "", nil, nil
	}
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", nil, fmt.Errorf("代理地址格式无效：%w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return "", nil, fmt.Errorf("不支持的代理协议 %q（仅支持 http / https / socks5）", u.Scheme)
	}
	if u.Hostname() == "" || u.Port() == "" {
		return "", nil, fmt.Errorf("代理地址需要包含主机和端口（例如 127.0.0.1:7890）")
	}
	return u.String(), u, nil
}

var errForbiddenLANTarget = errors.New("forbidden LAN target")

type lanClientCtxKey struct{}

func withLANClient(ctx context.Context) context.Context {
	return context.WithValue(ctx, lanClientCtxKey{}, true)
}

func isLANClientCtx(ctx context.Context) bool {
	v, _ := ctx.Value(lanClientCtxKey{}).(bool)
	return v
}

func (p *proxy) setAdminAddr(addr string) {
	p.adminAddrMu.Lock()
	p.adminAddr = strings.TrimSpace(addr)
	p.adminAddrMu.Unlock()
}

func (p *proxy) getAdminAddr() string {
	p.adminAddrMu.RLock()
	addr := p.adminAddr
	p.adminAddrMu.RUnlock()
	return addr
}

func (p *proxy) getUpstreamURL() *url.URL {
	p.upMu.RLock()
	u := p.upstreamURL
	p.upMu.RUnlock()
	return u
}

func isNumericLoopbackOrZero(h string) bool {
	if h == "" || strings.Contains(h, "_") || strings.ContainsAny(h, "+-") {
		return false
	}
	parts := strings.Split(h, ".")
	if len(parts) < 1 || len(parts) > 4 {
		return false
	}
	vals := make([]uint64, len(parts))
	allZero := true
	for i, part := range parts {
		if part == "" {
			return false
		}
		v, err := strconv.ParseUint(part, 0, 32)
		if err != nil {
			return false
		}
		if i < len(parts)-1 && v > 255 {
			return false
		}
		vals[i] = v
		if v != 0 {
			allZero = false
		}
	}
	if allZero {
		return true
	}
	if len(parts) == 1 {
		return (vals[0] >> 24) == 127
	}
	return vals[0] == 127
}

func isLoopbackHost(host string) bool {
	h := strings.TrimSuffix(strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]"), ".")
	if h == "" || h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return isNumericLoopbackOrZero(h)
}

func (p *proxy) isForbiddenLANTarget(host, port string) bool {
	if isLoopbackHost(host) {
		return true
	}
	if port == "" {
		return false
	}
	hp := net.JoinHostPort(host, port)
	if isSelfHostPort(hp, p.addr) {
		return true
	}
	admin := p.getAdminAddr()
	if admin != "" {
		if isSelfHostPort(hp, admin) {
			return true
		}
		if _, adminPort, err := net.SplitHostPort(admin); err == nil && adminPort != "" && port == adminPort {
			normHost := strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
			for _, lip := range localLANIPv4s() {
				if normHost == lip {
					return true
				}
			}
		}
	}
	return false
}

func (p *proxy) dialForForward(ctx context.Context, network, addr string) (net.Conn, error) {
	if isLANClientCtx(ctx) {
		up := p.getUpstreamURL()
		if up == nil || !isSelfHostPort(addr, up.Host) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				host = addr
			}
			if isLoopbackHost(host) {
				return nil, errForbiddenLANTarget
			}
			conn, dErr := p.dial(ctx, network, addr)
			if dErr != nil {
				return nil, dErr
			}
			cleanHost := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
			if !assetHost.MatchString(cleanHost) {
				if ta, ok := conn.RemoteAddr().(*net.TCPAddr); ok && (ta.IP.IsLoopback() || ta.IP.IsUnspecified()) {
					_ = conn.Close()
					return nil, errForbiddenLANTarget
				}
			}
			return conn, nil
		}
	}
	return p.dial(ctx, network, addr)
}

func isSelfHostPort(hostPort, selfAddr string) bool {
	h1, p1, err1 := net.SplitHostPort(strings.TrimSpace(hostPort))
	h2, p2, err2 := net.SplitHostPort(strings.TrimSpace(selfAddr))
	if err1 != nil || err2 != nil || p1 == "" || p1 != p2 {
		return false
	}
	if strings.EqualFold(strings.Trim(h1, "[]"), strings.Trim(h2, "[]")) {
		return true
	}
	return isLoopbackHost(h1) && isLoopbackHost(h2)
}

// extractUpstreamURL 从 Windows ProxyServer 字符串（"127.0.0.1:7890" 或 "http=...;https=...;socks=..."）提取可用代理地址。
func extractUpstreamURL(server string) string {
	s := strings.TrimSpace(server)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "=") {
		if norm, _, err := normalizeUpstreamURL("http://" + s); err == nil {
			return norm
		}
		return ""
	}
	byScheme := map[string]string{}
	for _, part := range strings.Split(s, ";") {
		if k, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok && v != "" {
			byScheme[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	switch {
	case byScheme["http"] != "":
		if norm, _, err := normalizeUpstreamURL("http://" + byScheme["http"]); err == nil {
			return norm
		}
	case byScheme["https"] != "":
		if norm, _, err := normalizeUpstreamURL("http://" + byScheme["https"]); err == nil {
			return norm
		}
	case byScheme["socks"] != "":
		if norm, _, err := normalizeUpstreamURL("socks5://" + byScheme["socks"]); err == nil {
			return norm
		}
	}
	return ""
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	loopback, privateLAN := classifyRemoteAddr(r.RemoteAddr)
	if !loopback && (!p.allowLAN.Load() || !privateLAN) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch {
	case r.Method == http.MethodConnect:
		p.connect(w, r, loopback)
	case r.TLS != nil: // 已解密的 CDN 请求
		p.serve(w, r, "https://"+r.Host)
	case r.URL.IsAbs(): // 普通 http:// 代理请求
		if !loopback {
			if p.isForbiddenLANTarget(r.URL.Hostname(), r.URL.Port()) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if r.Host != "" {
				rh, rp, err := net.SplitHostPort(r.Host)
				if err != nil {
					rh = r.Host
					rp = ""
				}
				if p.isForbiddenLANTarget(rh, rp) {
					http.Error(w, "forbidden", http.StatusForbidden)
					return
				}
			}
			r = r.WithContext(withLANClient(r.Context()))
		}
		p.serve(w, r, "http://"+r.URL.Host)
	default:
		p.local.ServeHTTP(w, r)
	}
}

func (p *proxy) connect(w http.ResponseWriter, r *http.Request, loopback bool) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !loopback && p.isForbiddenLANTarget(host, port) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if isSelfHostPort(r.Host, p.addr) {
		http.Error(w, "gbf-dock: loop detected", http.StatusBadRequest)
		return
	}
	// 排查用：日志里出现这一行，说明浏览器确实把请求交给了本程序（之后若有 TLS 报错就是证书问题）
	if _, seen := p.seenHosts.LoadOrStore("connect "+host, true); !seen && assetHost.MatchString(host) {
		log.Printf("收到浏览器对 %s 的连接", host)
	}
	var upstream net.Conn
	if port != "443" || !assetHost.MatchString(host) {
		// 非静态资源流量（游戏 API、桌面端 Gemini 等）：绝不解密，原样经上游代理节点隧道透传
		dialCtx := r.Context()
		if !loopback {
			dialCtx = withLANClient(dialCtx)
		}
		if upstream, err = p.dialUpstreamTunnel(dialCtx, r.Host, host); err != nil {
			if errors.Is(err, errForbiddenLANTarget) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		if upstream != nil {
			upstream.Close()
		}
		return
	}
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		conn.Close()
		if upstream != nil {
			upstream.Close()
		}
		return
	}
	client := bufConn{conn, brw.Reader}
	if upstream == nil {
		p.mitm <- tls.Server(client, p.tlsConfig)
		return
	}
	relayTunnel(client, upstream)
}

func (p *proxy) dialUpstreamTunnel(ctx context.Context, targetAddr, targetHost string) (net.Conn, error) {
	if isLANClientCtx(ctx) && isLoopbackHost(targetHost) {
		return nil, errForbiddenLANTarget
	}
	p.upMu.RLock()
	up := p.upstreamURL
	p.upMu.RUnlock()

	if up == nil || isLoopbackHost(targetHost) {
		conn, err := p.dial(ctx, "tcp", targetAddr)
		if err != nil {
			return nil, err
		}
		cleanHost := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(targetHost)), ".")
		if isLANClientCtx(ctx) && !assetHost.MatchString(cleanHost) {
			if ta, ok := conn.RemoteAddr().(*net.TCPAddr); ok && (ta.IP.IsLoopback() || ta.IP.IsUnspecified()) {
				_ = conn.Close()
				return nil, errForbiddenLANTarget
			}
		}
		return conn, nil
	}

	raw, err := p.dial(ctx, "tcp", up.Host)
	if err != nil {
		return nil, fmt.Errorf("连接上游代理 %s 失败：%w", up.Host, err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		raw.SetDeadline(deadline)
		defer raw.SetDeadline(time.Time{})
	} else {
		raw.SetDeadline(time.Now().Add(20 * time.Second))
		defer raw.SetDeadline(time.Time{})
	}

	scheme := strings.ToLower(up.Scheme)
	switch scheme {
	case "http", "https":
		var conn net.Conn = raw
		if scheme == "https" {
			tc := tls.Client(raw, &tls.Config{ServerName: up.Hostname()})
			if err := tc.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, fmt.Errorf("上游 HTTPS 代理握手失败：%w", err)
			}
			conn = tc
		}
		var b strings.Builder
		fmt.Fprintf(&b, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Connection: keep-alive\r\n", targetAddr, targetAddr)
		if up.User != nil {
			pass, _ := up.User.Password()
			cred := base64.StdEncoding.EncodeToString([]byte(up.User.Username() + ":" + pass))
			fmt.Fprintf(&b, "Proxy-Authorization: Basic %s\r\n", cred)
		}
		b.WriteString("\r\n")
		if _, err := io.WriteString(conn, b.String()); err != nil {
			conn.Close()
			return nil, fmt.Errorf("发送 CONNECT 到上游代理失败：%w", err)
		}
		br := bufio.NewReader(conn)
		resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("读取上游代理响应失败：%w", err)
		}
		if resp.StatusCode != http.StatusOK {
			conn.Close()
			return nil, fmt.Errorf("上游代理拒绝连接到 %s：%s", targetAddr, resp.Status)
		}
		return bufConn{Conn: conn, r: br}, nil

	case "socks5", "socks5h":
		if err := socks5Handshake(raw, up.User, targetAddr); err != nil {
			raw.Close()
			return nil, err
		}
		return raw, nil

	default:
		raw.Close()
		return nil, fmt.Errorf("不支持的上游代理协议：%s", up.Scheme)
	}
}

func socks5Handshake(conn net.Conn, user *url.Userinfo, targetAddr string) error {
	host, portStr, err := net.SplitHostPort(targetAddr)
	if err != nil {
		return err
	}
	portNum, err := strconv.Atoi(portStr)
	if err != nil || portNum < 1 || portNum > 65535 {
		return fmt.Errorf("无效端口：%s", portStr)
	}

	methods := []byte{0x00}
	if user != nil {
		methods = []byte{0x00, 0x02}
	}
	greet := append([]byte{0x05, byte(len(methods))}, methods...)
	if _, err := conn.Write(greet); err != nil {
		return err
	}
	var sel [2]byte
	if _, err := io.ReadFull(conn, sel[:]); err != nil {
		return err
	}
	if sel[0] != 0x05 {
		return errors.New("SOCKS5 代理协议版本异常")
	}
	switch sel[1] {
	case 0x00:
	case 0x02:
		if user == nil {
			return errors.New("SOCKS5 代理需要用户名密码")
		}
		u := user.Username()
		pw, _ := user.Password()
		if len(u) > 255 || len(pw) > 255 {
			return errors.New("SOCKS5 认证信息过长")
		}
		auth := make([]byte, 0, 3+len(u)+len(pw))
		auth = append(auth, 0x01, byte(len(u)))
		auth = append(auth, u...)
		auth = append(auth, byte(len(pw)))
		auth = append(auth, pw...)
		if _, err := conn.Write(auth); err != nil {
			return err
		}
		var aresp [2]byte
		if _, err := io.ReadFull(conn, aresp[:]); err != nil {
			return err
		}
		if aresp[1] != 0x00 {
			return errors.New("SOCKS5 用户名或密码认证失败")
		}
	default:
		return errors.New("SOCKS5 代理拒绝支持的认证方式")
	}

	req := make([]byte, 0, 7+len(host))
	req = append(req, 0x05, 0x01, 0x00)
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, 0x01)
			req = append(req, ip4...)
		} else {
			req = append(req, 0x04)
			req = append(req, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return errors.New("SOCKS5 目标域名过长")
		}
		req = append(req, 0x03, byte(len(host)))
		req = append(req, host...)
	}
	var pbuf [2]byte
	binary.BigEndian.PutUint16(pbuf[:], uint16(portNum))
	req = append(req, pbuf[:]...)
	if _, err := conn.Write(req); err != nil {
		return err
	}

	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return err
	}
	if hdr[1] != 0x00 {
		return fmt.Errorf("SOCKS5 连接失败（状态码 %d）", hdr[1])
	}
	var addrLen int
	switch hdr[3] {
	case 0x01:
		addrLen = 4
	case 0x04:
		addrLen = 16
	case 0x03:
		var ln [1]byte
		if _, err := io.ReadFull(conn, ln[:]); err != nil {
			return err
		}
		addrLen = int(ln[0])
	default:
		return errors.New("SOCKS5 响应地址类型无效")
	}
	tail := make([]byte, addrLen+2)
	_, err = io.ReadFull(conn, tail)
	return err
}

func relayTunnel(client, upstream net.Conn) {
	done1 := make(chan struct{})
	done2 := make(chan struct{})
	go func() {
		io.Copy(upstream, client)
		closeWrite(upstream)
		close(done1)
	}()
	go func() {
		io.Copy(client, upstream)
		closeWrite(client)
		close(done2)
	}()
	select {
	case <-done1:
		select {
		case <-done2:
		case <-time.After(10 * time.Second):
		}
	case <-done2:
		select {
		case <-done1:
		case <-time.After(10 * time.Second):
		}
	}
	client.Close()
	upstream.Close()
}

type closeWriter interface {
	CloseWrite() error
}

func closeWrite(c net.Conn) {
	if bc, ok := c.(bufConn); ok {
		c = bc.Conn
	}
	if cw, ok := c.(closeWriter); ok {
		cw.CloseWrite()
	}
}

func (p *proxy) serve(w http.ResponseWriter, r *http.Request, origin string) {
	target := origin + r.URL.RequestURI()
	host := strings.TrimPrefix(strings.TrimPrefix(origin, "https://"), "http://")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if (r.Method != http.MethodGet && r.Method != http.MethodHead) || !assetHost.MatchString(host) {
		p.forward(w, r, target, assetHost.MatchString(host))
		return
	}
	if r.URL.Path == checkPath || r.URL.Path == legacyCheckPath { // “检测浏览器”：能看到这一页，说明浏览器走了本程序且信任证书
		p.checkedAt.Store(time.Now().Unix())
		log.Printf("浏览器检测通过：浏览器经过了 GBF-Dock，证书受信任")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		io.WriteString(w, checkPage)
		return
	}
	if _, seen := p.seenHosts.LoadOrStore(host, true); !seen {
		log.Printf("开始缓存来自 %s 的游戏资源", host)
	}
	path := p.cachePath(target)
	if p.serveCached(w, r, target, path, true) {
		return
	}
	if r.Method == http.MethodHead {
		p.forward(w, r, target, true)
		return
	}

	// 同一文件已有请求在下载：等它下完直接读缓存，不重复下载
	wait := make(chan struct{})
	if ch, busy := p.fetching.LoadOrStore(path, wait); busy {
		select {
		case <-ch.(chan struct{}):
		case <-r.Context().Done():
			return
		}
		p.merged.Add(1)
		if p.serveCached(w, r, target, path, false) {
			return
		}
		p.forward(w, r, target, true) // 领头的请求没能缓存（如 404，或是另一个版本）：自己直接取
		return
	}
	defer func() { p.fetching.Delete(path); close(wait) }()

	p.misses.Add(1)
	// 浏览器中途取消也把文件下完：缓存不白下，等待中的合并请求也能用上
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancel()
	reqRange := r.Header.Get("Range")
	openRangeZero := isOpenRangeFromZero(reqRange)
	if reqRange == "" || openRangeZero {
		delivered, err := p.streamFetchAndStore(ctx, w, r, target, path, openRangeZero)
		if delivered {
			return
		}
		if err == nil {
			if p.serveCached(w, r, target, path, false) {
				return
			}
			p.forward(w, r, target, true)
			return
		}
		p.errors.Add(1)
		log.Printf("下载失败 %s：%v", target, err)
		http.Error(w, fmt.Sprint("gbf-dock: ", err), http.StatusBadGateway)
		return
	}
	resp, err := p.fetch(ctx, r.Header, target, path)
	if resp != nil {
		relay(w, resp)
		return
	}
	if p.serveCached(w, r, target, path, false) {
		return
	}
	if err == nil {
		p.forward(w, r, target, true) // 极少见：刚存好就被同一路径的另一个版本覆盖
		return
	}
	p.errors.Add(1)
	log.Printf("下载失败 %s：%v", target, err)
	http.Error(w, fmt.Sprint("gbf-dock: ", err), http.StatusBadGateway)
}

func (p *proxy) isShardMarkedDead(host string) bool {
	if host == "" {
		return false
	}
	v, ok := p.deadShards.Load(host)
	if !ok {
		return false
	}
	ts, ok := v.(time.Time)
	if !ok || time.Since(ts) > deadShardTTL {
		p.deadShards.Delete(host)
		return false
	}
	return true
}

func (p *proxy) markShardDead(host string) {
	if host != "" {
		p.deadShards.Store(host, time.Now())
	}
}

func (p *proxy) resolveUpstreamTargets(target string) (primary, fallback string) {
	canonical := normalizeCDNShardURL(target)
	if canonical == target {
		return target, ""
	}
	if p.isShardMarkedDead(extractHostFromURL(target)) {
		return canonical, ""
	}
	return target, canonical
}

// fetch 向 CDN 取完整原文。可缓存的 200 写入缓存并返回 nil；其余响应原样返回给调用方转交。
func (p *proxy) fetch(ctx context.Context, src http.Header, target, path string) (*http.Response, error) {
	primary, fallback := p.resolveUpstreamTargets(target)
	for attempt := 0; attempt < 2; attempt++ {
		curTarget := primary
		if attempt > 0 && fallback != "" {
			curTarget = fallback
		}
		req, reqErr := upstreamGet(ctx, curTarget, src)
		if reqErr != nil {
			return nil, reqErr
		}
		if attempt > 0 {
			req.Close = true
		}
		resp, err := p.tr.RoundTrip(req)
		if err != nil {
			if attempt == 0 && ctx.Err() == nil {
				if fallback != "" && isFallbackEligibleErr(err) {
					p.markShardDead(extractHostFromURL(primary))
					log.Printf("旧分片 %s 不可达，已自动回退主域名 %s 拉取（%v）", extractHostFromURL(primary), extractHostFromURL(fallback), err)
					continue
				}
				if fallback == "" && isTransientNetErr(err) {
					if p.tr != nil {
						p.tr.CloseIdleConnections()
					}
					continue
				}
			}
			return nil, err
		}
		if !cacheable(resp) {
			p.passthrough.Add(1)
			return resp, nil
		}
		storeErr := p.store(path, target, resp)
		_ = resp.Body.Close()
		if storeErr == nil {
			return nil, nil
		}
		if attempt == 0 && ctx.Err() == nil {
			if fallback != "" && isFallbackEligibleErr(storeErr) {
				p.markShardDead(extractHostFromURL(primary))
				log.Printf("旧分片 %s 传输中断，已自动回退主域名 %s 重试（%v）", extractHostFromURL(primary), extractHostFromURL(fallback), storeErr)
				continue
			}
			if fallback == "" && isTransientNetErr(storeErr) {
				if p.tr != nil {
					p.tr.CloseIdleConnections()
				}
				continue
			}
		}
		return nil, storeErr
	}
	return nil, errors.New("上游重试后仍未成功")
}

// revalidate 在后台用 ETag / Last-Modified 向 CDN 确认缓存是否仍是最新。
func (p *proxy) revalidate(target, path string, src, cached http.Header) {
	if _, busy := p.inflight.LoadOrStore(path, true); busy {
		return
	}
	defer p.inflight.Delete(path)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	primary, fallback := p.resolveUpstreamTargets(target)
	tryReval := func(u string) (*http.Response, error) {
		req, err := upstreamGet(ctx, u, src)
		if err != nil {
			return nil, err
		}
		if v := cached.Get("Etag"); v != "" {
			req.Header.Set("If-None-Match", v)
		}
		if v := cached.Get("Last-Modified"); v != "" {
			req.Header.Set("If-Modified-Since", v)
		}
		return p.tr.RoundTrip(req)
	}
	resp, err := tryReval(primary)
	if err != nil && fallback != "" && isFallbackEligibleErr(err) && ctx.Err() == nil {
		p.markShardDead(extractHostFromURL(primary))
		resp, err = tryReval(fallback)
	}
	if err != nil {
		return
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified:
		now := time.Now()
		os.Chtimes(path, now, now) // 文件修改时间 = 最后一次确认时间
		p.metaUpdateMu.Lock()
		if v, ok := p.metaIdx.Load(path); ok {
			cm := v.(*cachedMeta)
			acc := now
			if cm.lastAccess.After(acc) {
				acc = cm.lastAccess
			}
			p.metaIdx.Store(path, &cachedMeta{
				url:        cm.url,
				header:     cm.header,
				headerLen:  cm.headerLen,
				bodySize:   cm.bodySize,
				modTime:    now,
				lastAccess: acc,
				looseQuery: cm.looseQuery,
			})
			p.markMetaIndexDirty()
		}
		p.metaUpdateMu.Unlock()
		if p.ram != nil {
			p.ram.Touch(path, now)
		}
	case cacheable(resp):
		_ = p.store(path, target, resp)
	} // 其他情况（404、5xx、断网）保留旧缓存，下次再试
	p.revalidated.Add(1)
}

func upstreamGet(ctx context.Context, target string, src http.Header) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	// 总是向 CDN 要完整原文（gzip 传输、由 Go 自动解压）；304 和断点续传由本地处理
	copyHeader(req.Header, src, "Range", "If-Range", "If-None-Match", "If-Modified-Since", "Accept-Encoding")
	return req, nil
}

func cacheable(resp *http.Response) bool {
	return resp.StatusCode == http.StatusOK &&
		resp.Header.Get("Content-Encoding") == "" && // 未被自动解压的编码不缓存
		!strings.Contains(resp.Header.Get("Cache-Control"), "no-store")
}

// 缓存文件格式：第一行是 entryMeta JSON，之后是原样的响应体。写临时文件再改名，中断不会留下半个文件；
// 同一路径的新版本直接覆盖旧版本。
func (p *proxy) store(path, target string, resp *http.Response) error {
	h := http.Header{}
	copyHeader(h, resp.Header, "Set-Cookie", "Date", "Age")
	meta, err := json.Marshal(entryMeta{URL: target, Header: h})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // 改名成功后是空操作
	headerBytes := append(meta, '\n')
	_, err = tmp.Write(headerBytes)
	var written int64
	var head [512]byte
	hn := 0
	keepRAM := p.ram != nil && p.ram.maxItemBytes > 0 && (resp.ContentLength < 0 || resp.ContentLength <= p.ram.maxItemBytes)
	var ramBuf bytes.Buffer
	if err == nil {
		buf := make([]byte, 32*1024)
		for {
			nRead, rErr := resp.Body.Read(buf)
			if nRead > 0 {
				chunk := buf[:nRead]
				if hn < len(head) {
					cp := copy(head[hn:], chunk)
					hn += cp
				}
				written += int64(nRead)
				if keepRAM {
					if written <= p.ram.maxItemBytes {
						ramBuf.Write(chunk)
					} else {
						keepRAM = false
						ramBuf.Reset()
					}
				}
				if _, wErr := tmp.Write(chunk); wErr != nil {
					err = wErr
					break
				}
			}
			if rErr != nil {
				if rErr != io.EOF {
					err = rErr
				}
				break
			}
		}
	}
	if err == nil && (written <= 0 || (resp.ContentLength >= 0 && written != resp.ContentLength)) {
		err = errors.New("响应长度异常或为空")
	}
	if err == nil {
		uPath := ""
		if u, uErr := url.Parse(target); uErr == nil {
			uPath = u.Path
		}
		if ok, reason := validateAssetBytes(uPath, resp.Header, head[:hn], written <= int64(hn)); !ok {
			err = fmt.Errorf("校验未通过（%s）", reason)
		}
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}

	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	var oldSize int64
	existed := false
	if st, statErr := os.Stat(path); statErr == nil {
		existed = true
		oldSize = st.Size()
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	if st, statErr := os.Stat(path); statErr == nil {
		p.cachedBytes.Add(st.Size() - oldSize)
		if !existed {
			p.cachedFiles.Add(1)
		}
		now := time.Now()
		p.metaUpdateMu.Lock()
		p.metaIdx.Store(path, &cachedMeta{
			url:        target,
			header:     h.Clone(),
			headerLen:  int64(len(headerBytes)),
			bodySize:   written,
			modTime:    st.ModTime(),
			lastAccess: now,
		})
		p.metaUpdateMu.Unlock()
		if p.ram != nil {
			if keepRAM && int64(ramBuf.Len()) == written {
				p.ram.Put(path, target, h, ramBuf.Bytes(), st.ModTime(), false)
			} else {
				p.ram.Delete(path)
			}
		}
		p.markMetaIndexDirty()
	}
	return nil
}

func (p *proxy) scanCacheUsage() (files, size int64) {
	filepath.WalkDir(p.getCacheRoot(), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if isInternalCacheFile(d.Name()) {
			if strings.HasPrefix(d.Name(), "tmp-") && time.Since(info.ModTime()) > time.Hour {
				os.Remove(path) // 程序被强制结束时残留的半截下载
			}
			return nil
		}
		files++
		size += info.Size()
		return nil
	})
	p.cachedFiles.Store(files)
	p.cachedBytes.Store(size)
	p.metaIdxReady.Store(true)
	return files, size
}

func (p *proxy) clearCache() error {
	p.usageMu.Lock()
	defer p.usageMu.Unlock()
	err := os.RemoveAll(p.getCacheRoot())
	if err == nil {
		p.cachedFiles.Store(0)
		p.cachedBytes.Store(0)
		if p.ram != nil {
			p.ram.Clear()
		}
		p.metaIdx.Clear()
		p.metaIndexDirty.Store(false)
		p.metaIdxReady.Store(true)
	} else {
		go p.scanCacheUsage()
	}
	return err
}

func (p *proxy) forward(w http.ResponseWriter, r *http.Request, target string, isAsset bool) {
	tr := p.forwardTr
	if isAsset || tr == nil {
		tr = p.tr
	}

	if isAsset && (r.Method == http.MethodHead || r.Method == http.MethodGet) {
		primary, fallback := p.resolveUpstreamTargets(target)
		req, err := http.NewRequestWithContext(r.Context(), r.Method, primary, nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		copyHeader(req.Header, r.Header)
		resp, err := tr.RoundTrip(req)
		if err != nil && fallback != "" && isFallbackEligibleErr(err) && r.Context().Err() == nil {
			p.markShardDead(extractHostFromURL(primary))
			log.Printf("旧分片 %s 不可达，已自动回退主域名 %s 转发（%v）", extractHostFromURL(primary), extractHostFromURL(fallback), err)
			if req2, err2 := http.NewRequestWithContext(r.Context(), r.Method, fallback, nil); err2 == nil {
				copyHeader(req2.Header, r.Header)
				req2.Close = true
				resp, err = tr.RoundTrip(req2)
			}
		}
		if err != nil {
			http.Error(w, "gbf-dock: "+err.Error(), http.StatusBadGateway)
			return
		}
		relay(w, resp)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	copyHeader(req.Header, r.Header)
	req.ContentLength = r.ContentLength
	resp, err := tr.RoundTrip(req)
	if err != nil {
		if errors.Is(err, errForbiddenLANTarget) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		http.Error(w, "gbf-dock: "+err.Error(), http.StatusBadGateway)
		return
	}
	relay(w, resp)
}

func relay(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if fl, ok := w.(http.Flusher); ok {
		buf := make([]byte, 32*1024)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				fl.Flush()
			}
			if err != nil {
				return
			}
		}
	}
	io.Copy(w, resp.Body)
}

func copyHeader(dst, src http.Header, skip ...string) {
	for k, vv := range src {
		if !hopHeaders[k] && !slices.Contains(skip, k) {
			dst[k] = append([]string(nil), vv...)
		}
	}
}

// cachePath 只由“归一化域名 + 去掉版本号的路径”决定，所以同一资源的新旧版本及 a/a1~a5 分片共用一个文件。
func (p *proxy) cachePath(target string) string {
	norm := normalizeCDNShardURL(target)
	key, _, _ := strings.Cut(norm, "?")
	sum := sha256.Sum256([]byte(versionSegment.ReplaceAllString(key, "/*/")))
	name := hex.EncodeToString(sum[:])
	return filepath.Join(p.getCacheRoot(), name[:2], name)
}

// legacyCachePath 保留未归一化分片前缀的旧版路径算法，用于无缝读取旧缓存。
func (p *proxy) legacyCachePath(target string) string {
	key, _, _ := strings.Cut(target, "?")
	sum := sha256.Sum256([]byte(versionSegment.ReplaceAllString(key, "/*/")))
	name := hex.EncodeToString(sum[:])
	return filepath.Join(p.getCacheRoot(), name[:2], name)
}

func (p *proxy) loadCA() error {
	certPath, keyPath := filepath.Join(p.certDir, "ca.crt"), filepath.Join(p.certDir, "ca.key")
	if _, err := os.Stat(certPath); errors.Is(err, os.ErrNotExist) {
		if err := genCA(certPath, keyPath); err != nil {
			return err
		}
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return err
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return errors.New("ca.key 不是 ECDSA 私钥")
	}
	if p.ca, err = x509.ParseCertificate(pair.Certificate[0]); err != nil {
		return err
	}
	p.caKey = key
	p.caPEM, err = os.ReadFile(certPath)
	return err
}

func genCA(certPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          randSerial(),
		Subject:               pkix.Name{CommonName: caName, Organization: []string{"GBF-Dock (generated locally)"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		// 名称约束：即使私钥泄露，这张根证书也只能用于 *.akamaized.net，冒充不了银行、邮箱等网站
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{"akamaized.net"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(certPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func (p *proxy) leaf(host string) (*tls.Certificate, error) {
	if c, ok := p.certs.Load(host); ok {
		return c.(*tls.Certificate), nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: randSerial(),
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(0, 0, 365),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		return nil, err
	}
	c, _ := p.certs.LoadOrStore(host, &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key})
	return c.(*tls.Certificate), nil
}

func randSerial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	return n
}

func (p *proxy) stats() string {
	return fmt.Sprintf("缓存命中 %d · 新下载 %d · 合并请求 %d · 后台确认更新 %d · 透传 %d · 本地提供 %.1f MB",
		p.hits.Load(), p.misses.Load(), p.merged.Load(), p.revalidated.Load(), p.passthrough.Load(), float64(p.servedBytes.Load())/1e6)
}

func (p *proxy) logStats() {
	last := ""
	for range time.Tick(10 * time.Minute) {
		if s := p.stats(); s != last {
			log.Print(s)
			last = s
		}
	}
}

// chanListener 把劫持来的 TLS 连接喂给内部 http.Server。
type chanListener chan net.Conn

func (l chanListener) Accept() (net.Conn, error) { return <-l, nil }
func (l chanListener) Close() error              { return nil }
func (l chanListener) Addr() net.Addr            { return &net.TCPAddr{} }

// bufConn 先读出 Hijack 时已缓冲的字节，避免丢掉客户端提前发来的 TLS 握手。
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c bufConn) Read(b []byte) (int, error) { return c.r.Read(b) }
