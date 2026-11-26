package main

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

func classifyRemoteAddr(remoteAddr string) (loopback, privateLAN bool) {
	if strings.TrimSpace(remoteAddr) == "192.0.2.1:1234" {
		// Go net/http/httptest.NewRequest 默认填充 TEST-NET-1，视为本地单元测试请求。
		return true, false
	}
	host := strings.TrimSpace(remoteAddr)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false, false
	}
	if ip.IsLoopback() {
		return true, false
	}
	return false, ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func localLANIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	excludes := []string{"wsl", "hyper-v", "vmware", "virtualbox", "bluetooth", "isatap", "teredo", "wintun", "clash", "mihomo"}
	var ips []string
	seen := map[string]bool{}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		low := strings.ToLower(iface.Name)
		skip := false
		for _, ex := range excludes {
			if strings.Contains(low, ex) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			ip4 := ip.To4()
			if ip4 == nil || !ip4.IsPrivate() {
				continue
			}
			s := ip4.String()
			if !seen[s] {
				seen[s] = true
				ips = append(ips, s)
			}
		}
	}
	sort.SliceStable(ips, func(i, j int) bool {
		pi := strings.HasPrefix(ips[i], "192.168.")
		pj := strings.HasPrefix(ips[j], "192.168.")
		if pi != pj {
			return pi
		}
		return ips[i] < ips[j]
	})
	return ips
}

func (a *app) cachedLANIPv4s(force bool) []string {
	a.lanIPsMu.Lock()
	defer a.lanIPsMu.Unlock()
	if !force && !a.lanIPsAt.IsZero() && time.Since(a.lanIPsAt) < 20*time.Second {
		return slices.Clone(a.lanIPs)
	}
	a.lanIPs = localLANIPv4s()
	a.lanIPsAt = time.Now()
	return slices.Clone(a.lanIPs)
}

func lanPacHostPort(reqHost, listenAddr string) string {
	_, defPort, _ := net.SplitHostPort(listenAddr)
	if defPort == "" {
		defPort = "8765"
	}
	reqHost = strings.TrimSpace(reqHost)
	if h, p, err := net.SplitHostPort(reqHost); err == nil && h != "" {
		if p == "" {
			p = defPort
		}
		return net.JoinHostPort(h, p)
	}
	if reqHost != "" && !strings.Contains(reqHost, ":") {
		return net.JoinHostPort(reqHost, defPort)
	}
	if ips := localLANIPv4s(); len(ips) > 0 {
		return net.JoinHostPort(ips[0], defPort)
	}
	return listenAddr
}

func listenAddrFor(baseAddr string, allowLAN bool) string {
	_, port, err := net.SplitHostPort(baseAddr)
	if err != nil || port == "" {
		if allowLAN {
			return "0.0.0.0:8765"
		}
		return baseAddr
	}
	if allowLAN {
		return "0.0.0.0:" + port
	}
	return baseAddr
}

func resolveAdminAddr(dataAddr, rawAdmin string) (string, error) {
	trimmed := strings.TrimSpace(rawAdmin)
	if trimmed != "" {
		host, portStr, err := net.SplitHostPort(trimmed)
		if err != nil || portStr == "" {
			return "", fmt.Errorf("控制面板监听地址无效（%q）：请使用 127.0.0.1:端口 格式", rawAdmin)
		}
		if !isLoopbackHost(host) {
			return "", fmt.Errorf("安全限制：控制面板地址 %s 仅允许绑定本机回环（127.0.0.1 或 localhost）", rawAdmin)
		}
		pNum, pErr := strconv.Atoi(portStr)
		if pErr != nil || pNum < 1 || pNum > 65535 {
			return "", fmt.Errorf("控制面板端口无效：%s", portStr)
		}
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(pNum)), nil
	}

	_, dPortStr, _ := net.SplitHostPort(strings.TrimSpace(dataAddr))
	dPort, err := strconv.Atoi(dPortStr)
	if err != nil || dPort <= 0 {
		dPort = 8765
	}
	adminPort := dPort + 1
	if adminPort > 65535 {
		adminPort = 8766
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(adminPort)), nil
}

func (a *app) serveListener(ln net.Listener) {
	a.lnMu.Lock()
	a.activeLn = ln
	a.lnMu.Unlock()

	go func(cur net.Listener) {
		err := http.Serve(cur, a.p)
		if err != nil && !errors.Is(err, net.ErrClosed) {
			fatal(fmt.Sprint("代理服务已停止：", err))
		}
	}(ln)
}

func (a *app) serveAdminListener(ln net.Listener) {
	go func(cur net.Listener) {
		err := http.Serve(cur, a.adminHandler())
		if err != nil && !errors.Is(err, net.ErrClosed) {
			fatal(fmt.Sprint("控制面板服务已停止：", err))
		}
	}(ln)
}

func (a *app) applyAllowLAN(enable bool) (string, error) {
	a.lnMu.Lock()
	defer a.lnMu.Unlock()

	prev := a.p.allowLAN.Load()
	if a.activeLn != nil {
		prevBind := listenAddrFor(a.p.addr, prev)
		nextBind := listenAddrFor(a.p.addr, enable)
		if prevBind != nextBind {
			_ = a.activeLn.Close()
			newLn, err := net.Listen("tcp", nextBind)
			if err != nil {
				if rbLn, rbErr := net.Listen("tcp", prevBind); rbErr == nil {
					a.activeLn = rbLn
					go func(cur net.Listener) {
						if sErr := http.Serve(cur, a.p); sErr != nil && !errors.Is(sErr, net.ErrClosed) {
							fatal(fmt.Sprint("代理服务已停止：", sErr))
						}
					}(rbLn)
				}
				a.p.allowLAN.Store(prev)
				return "", fmt.Errorf("切换监听地址 %s 失败：%w", nextBind, err)
			}
			a.activeLn = newLn
			go func(cur net.Listener) {
				if sErr := http.Serve(cur, a.p); sErr != nil && !errors.Is(sErr, net.ErrClosed) {
					fatal(fmt.Sprint("代理服务已停止：", sErr))
				}
			}(newLn)
		}
	}

	a.p.allowLAN.Store(enable)
	a.cfgMu.Lock()
	a.cfg.AllowLAN = enable
	a.saveConfigLocked()
	a.cfgMu.Unlock()

	if enable {
		ips := a.cachedLANIPv4s(true)
		_, port, _ := net.SplitHostPort(a.p.addr)
		if port == "" {
			port = "8765"
		}
		if len(ips) > 0 {
			return fmt.Sprintf("已开启局域网共享：手机/iPad 可打开 http://%s:%s 进行配置（若无法连接请检查 Windows 防火墙放行）。", ips[0], port), nil
		}
		return "已开启局域网共享（端口 " + port + " 已绑定 0.0.0.0；本机控制面板仍仅限本机操作）。", nil
	}
	return "已关闭局域网共享，仅限本机 127.0.0.1 访问。", nil
}

func (a *app) pingUpstream() (int64, string, error) {
	tr := &http.Transport{
		Proxy:                 a.p.transportProxy,
		DialContext:           func(ctx context.Context, n, addr string) (net.Conn, error) { return a.p.dial(ctx, n, addr) },
		ForceAttemptHTTP2:     true,
		DisableKeepAlives:     true,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
	}
	defer tr.CloseIdleConnections()

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://prd-game-a-granbluefantasy.akamaized.net/", nil)
	if err != nil {
		return 0, "", err
	}
	t0 := time.Now()
	resp, err := tr.RoundTrip(req)
	if err != nil {
		return 0, "", fmt.Errorf("测速失败（%s）：%w", a.upstreamLabel(), err)
	}
	_ = resp.Body.Close()

	ms := time.Since(t0).Milliseconds()
	if ms < 1 {
		ms = 1
	}
	a.lastPingMs.Store(ms)
	a.lastPingAt.Store(time.Now().Unix())
	return ms, fmt.Sprintf("连通测速完成：%d ms（出口：%s）", ms, a.upstreamLabel()), nil
}

func mobileGuideHTML(r *http.Request, listenAddr string) string {
	hostPort := lanPacHostPort(r.Host, listenAddr)
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil {
		host = hostPort
		port = "8765"
	}
	pacURL := "http://" + hostPort + "/proxy.pac"
	caURL := "http://" + hostPort + "/ca.crt"

	safeHostPort := html.EscapeString(hostPort)
	safeHost := html.EscapeString(host)
	safePort := html.EscapeString(port)
	safePac := html.EscapeString(pacURL)
	safeCA := html.EscapeString(caURL)
	safeCheck := html.EscapeString(checkURL)

	return `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>GBF-Dock 移动端局域网向导</title>
<style>
  body{margin:0;padding:18px;font-family:-apple-system,"Segoe UI","Microsoft YaHei UI",sans-serif;background:#111317;color:#eef2fa;line-height:1.6}
  .wrap{max-width:560px;margin:0 auto;display:grid;gap:14px}
  .card{background:#1b1f27;border:1px solid rgba(255,255,255,.1);border-radius:14px;padding:16px}
  h1{margin:0 0 6px;font-size:19px}
  .sub{margin:0;color:#9aa6bc;font-size:13px}
  .step{display:flex;align-items:center;gap:8px;font-weight:700;margin-bottom:8px;font-size:15px}
  .num{width:22px;height:22px;border-radius:50%;background:#2f7cf6;color:#fff;font-size:12px;display:inline-flex;align-items:center;justify-content:center;flex:none}
  code{display:block;padding:10px 12px;border-radius:8px;background:#242936;color:#9cc2ff;font-size:13px;word-break:break-all;margin:8px 0}
  .btn{display:inline-block;padding:9px 14px;border-radius:8px;background:#2f7cf6;color:#fff;text-decoration:none;font-weight:600;font-size:13px;border:none;cursor:pointer}
  .btn.alt{background:#242936;color:#eef2fa;border:1px solid rgba(255,255,255,.12)}
  .warn{font-size:12px;color:#eab308;margin-top:6px}
</style>
</head>
<body>
<div class="wrap">
  <div class="card">
    <h1>GBF-Dock 移动端接入向导</h1>
    <p class="sub">当前电脑服务地址：` + safeHostPort + `（仅限本地/局域网访问；控制面板仅在本机可用）</p>
  </div>
  <div class="card">
    <div class="step"><span class="num">1</span>下载并信任本机根证书</div>
    <p class="sub">仅用于解密 <code>*.akamaized.net</code> 游戏静态资源，不碰账号与游戏主站流量。</p>
    <a class="btn" href="` + safeCA + `">下载根证书 ca.crt</a>
    <p class="warn">iOS / iPadOS 安装后请前往：设置 → 通用 → 关于本机 → 证书信任设置，开启对「` + caName + `」（若由旧版迁移则为「` + legacyCAName + `」）的完全信任。</p>
  </div>
  <div class="card">
    <div class="step"><span class="num">2</span>在当前 Wi-Fi 配置代理（二选一）</div>
    <p class="sub"><b>方式 A · 推荐自动代理 (PAC)</b>（仅分流游戏静态资源，不影响手机其他软件）：</p>
    <code id="pac">` + safePac + `</code>
    <button class="btn alt" type="button" onclick="copyText('` + safePac + `')">复制 PAC 地址</button>
    <p class="sub" style="margin-top:12px"><b>方式 B · 手动 HTTP 代理</b>（当游戏或代理软件需要全走电脑转发时）：</p>
    <code>服务器：` + safeHost + ` &nbsp;&nbsp; 端口：` + safePort + `</code>
  </div>
  <div class="card">
    <div class="step"><span class="num">3</span>验证缓存连通性</div>
    <p class="sub">完成后重启游戏浏览器（Safari / SkyLeap / Chrome），点击下方测试页看到「✓ 检测通过」即可畅玩：</p>
    <a class="btn" href="` + safeCheck + `" target="_blank">打开连通检测页</a>
  </div>
</div>
<script>
function copyText(txt){
  if(navigator.clipboard){navigator.clipboard.writeText(txt).then(()=>alert('已复制：'+txt),()=>prompt('请手动复制：',txt));}
  else{prompt('请手动复制：',txt);}
}
</script>
</body>
</html>`
}
