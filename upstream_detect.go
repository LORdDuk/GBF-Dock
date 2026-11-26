package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type candPort struct {
	port int
	name string
}

type upstreamDetection struct {
	Proxy  string
	Source string
	Alive  bool
	TUN    string
}

// 单元测试可注入临时本地端口以覆盖扫描与误判过滤，生产环境为空。
var extraScanPorts []candPort

var candidateProxyPorts = []candPort{
	{7890, "Clash / FlClash"},
	{7897, "Clash Verge"},
	{10808, "v2rayN"},
	{10809, "v2rayN HTTP"},
	{2080, "Sing-box / NekoRay"},
	{12334, "Hiddify"},
	{1080, "Shadowsocks"},
	{7891, "Clash SOCKS"},
	{7892, "Clash HTTP"},
	{7893, "Clash Mixed"},
	{2081, "NekoRay HTTP"},
	{2801, "Netch"},
	{2802, "Netch HTTP"},
	{10800, "本地代理"},
	{1081, "本地代理"},
	{8099, "岛风GO"},
	{8123, "ACGP / 本地代理"},
	{8118, "Privoxy"},
	{7070, "本地 SOCKS"},
}

var pacProxyRe = regexp.MustCompile(`(?i)\b(PROXY|HTTPS|SOCKS5|SOCKS)\s+([a-z0-9.\[\]:-]+)`)

func isTCPReachable(hostPort string, timeout time.Duration) bool {
	if strings.TrimSpace(hostPort) == "" {
		return false
	}
	c, err := net.DialTimeout("tcp", hostPort, timeout)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func isUpstreamURLAlive(rawURL string, timeout time.Duration) bool {
	if strings.TrimSpace(rawURL) == "" {
		return false
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return false
	}
	return isTCPReachable(u.Host, timeout)
}

func newLoopbackProbeTarget() (string, func()) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", func() {}
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return ln.Addr().String(), func() { _ = ln.Close() }
}

func probeLocalProxyPort(hostPort, probeTarget string) (scheme string, ok bool) {
	if probeTarget != "" {
		if c, err := net.DialTimeout("tcp", hostPort, 120*time.Millisecond); err == nil {
			_ = c.SetDeadline(time.Now().Add(160 * time.Millisecond))
			_, wErr := fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", probeTarget, probeTarget)
			var line string
			var rErr error
			if wErr == nil {
				line, rErr = bufio.NewReaderSize(c, 256).ReadString('\n')
			}
			_ = c.Close()
			if wErr == nil && rErr == nil && (strings.HasPrefix(line, "HTTP/1.1 200") || strings.HasPrefix(line, "HTTP/1.0 200")) {
				return "http", true
			}
		}
	}

	c, err := net.DialTimeout("tcp", hostPort, 120*time.Millisecond)
	if err != nil {
		return "", false
	}
	_ = c.SetDeadline(time.Now().Add(150 * time.Millisecond))
	_, wErr := c.Write([]byte{0x05, 0x01, 0x00})
	var sel [2]byte
	var rErr error
	if wErr == nil {
		_, rErr = io.ReadFull(c, sel[:])
	}
	_ = c.Close()
	if wErr == nil && rErr == nil && sel[0] == 0x05 && sel[1] == 0x00 {
		return "socks5", true
	}
	return "", false
}

func scanLocalProxyPorts(selfAddr string) (proxyURL, source string) {
	cands := make([]candPort, 0, len(extraScanPorts)+len(candidateProxyPorts))
	cands = append(cands, extraScanPorts...)
	cands = append(cands, candidateProxyPorts...)
	if len(cands) == 0 {
		return "", ""
	}

	probeTarget, cleanup := newLoopbackProbeTarget()
	defer cleanup()

	type scanRes struct {
		proxy  string
		source string
	}
	results := make([]scanRes, len(cands))
	var wg sync.WaitGroup
	for i, c := range cands {
		hp := net.JoinHostPort("127.0.0.1", strconv.Itoa(c.port))
		if isSelfHostPort(hp, selfAddr) {
			continue
		}
		wg.Add(1)
		go func(idx int, cand candPort, hostPort string) {
			defer wg.Done()
			scheme, ok := probeLocalProxyPort(hostPort, probeTarget)
			if !ok {
				return
			}
			results[idx] = scanRes{
				proxy:  scheme + "://" + hostPort,
				source: fmt.Sprintf("端口扫描 (%s · %d)", cand.name, cand.port),
			}
		}(i, c, hp)
	}
	wg.Wait()

	for _, res := range results {
		if res.proxy != "" {
			return res.proxy, res.source
		}
	}
	return "", ""
}

func extractUpstreamFromPAC(pacURL, selfAddr, ownPACURL string) string {
	pacURL = strings.TrimSpace(pacURL)
	if pacURL == "" || pacURL == ownPACURL {
		return ""
	}
	u, err := url.Parse(pacURL)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || isSelfHostPort(u.Host, selfAddr) {
		return ""
	}

	client := &http.Client{
		Timeout: 400 * time.Millisecond,
		Transport: &http.Transport{
			Proxy:       nil,
			DialContext: (&net.Dialer{Timeout: 200 * time.Millisecond}).DialContext,
		},
	}
	resp, err := client.Get(pacURL)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return ""
	}

	matches := pacProxyRe.FindAllStringSubmatch(string(body), -1)
	for _, m := range matches {
		kind := strings.ToUpper(m[1])
		hp := strings.TrimRight(strings.TrimSpace(m[2]), ";,")
		if hp == "" || isSelfHostPort(hp, selfAddr) {
			continue
		}
		raw := "http://" + hp
		if kind == "SOCKS5" || kind == "SOCKS" {
			raw = "socks5://" + hp
		}
		norm, pu, err := normalizeUpstreamURL(raw)
		if err != nil || pu == nil || isSelfHostPort(pu.Host, selfAddr) {
			continue
		}
		if isTCPReachable(pu.Host, 150*time.Millisecond) {
			return norm
		}
	}
	return ""
}

func extractUpstreamFromEnv(selfAddr string) (string, string) {
	keys := []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"}
	for _, k := range keys {
		val := strings.TrimSpace(os.Getenv(k))
		if val == "" {
			continue
		}
		norm, u, err := normalizeUpstreamURL(val)
		if err != nil || u == nil || isSelfHostPort(u.Host, selfAddr) {
			continue
		}
		if isTCPReachable(u.Host, 150*time.Millisecond) {
			return norm, "环境变量 (" + k + ")"
		}
	}
	return "", ""
}

func hasNonLinkLocalUnicast(iface net.Interface) bool {
	addrs, err := iface.Addrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}

func detectTUNAdapter() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	excludes := []string{"wsl", "hyper-v", "vethernet (default", "vmware", "virtualbox", "bluetooth", "loopback", "isatap", "teredo"}
	keywords := []string{
		"clash", "mihomo", "flclash", "verge", "sing-box", "singbox",
		"nekoray", "nekobox", "hiddify", "v2ray", "xray", "wintun", "utun",
		"netch", "sstap", "网易uu", "leigod", "雷神", "qiyou", "奇游",
		"xunyou", "迅游", "biubiu", "golink", "玲珑", "lonlife",
		"ak加速器", "薄荷", "鲜牛", "奇妙", "uu加速",
	}

	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(iface.Name))
		if name == "" {
			continue
		}
		skip := false
		for _, ex := range excludes {
			if strings.Contains(name, ex) {
				skip = true
				break
			}
		}
		if skip || !hasNonLinkLocalUnicast(iface) {
			continue
		}

		matched := name == "uu" || name == "tun" || strings.HasPrefix(name, "tun") || strings.Contains(name, " tun")
		if !matched {
			for _, kw := range keywords {
				if strings.Contains(name, kw) {
					matched = true
					break
				}
			}
		}
		if matched {
			return iface.Name
		}
	}
	return ""
}

func (a *app) detectUpstream(sys sysState, prevAutoProxy string) upstreamDetection {
	tun := detectTUNAdapter()

	if sys.ManualProxy != "" && !sys.ProxyEnabled {
		if cand := extractUpstreamURL(sys.ManualProxy); cand != "" {
			if norm, u, err := normalizeUpstreamURL(cand); err == nil && u != nil && !isSelfHostPort(u.Host, a.p.addr) && isTCPReachable(u.Host, 150*time.Millisecond) {
				return upstreamDetection{Proxy: norm, Source: "系统代理", Alive: true, TUN: tun}
			}
		}
	}

	if sys.SavedUpstream != "" {
		if cand := extractUpstreamURL(sys.SavedUpstream); cand != "" {
			if norm, u, err := normalizeUpstreamURL(cand); err == nil && u != nil && !isSelfHostPort(u.Host, a.p.addr) && isTCPReachable(u.Host, 150*time.Millisecond) {
				return upstreamDetection{Proxy: norm, Source: "原系统代理", Alive: true, TUN: tun}
			}
		}
	}

	for _, pacURL := range []string{sys.PAC, savedBackupPAC(a.backupPath, a.p.addr, a.pacURL)} {
		if cand := extractUpstreamFromPAC(pacURL, a.p.addr, a.pacURL); cand != "" {
			return upstreamDetection{Proxy: cand, Source: "系统 PAC", Alive: true, TUN: tun}
		}
	}

	if cand, src := extractUpstreamFromEnv(a.p.addr); cand != "" {
		return upstreamDetection{Proxy: cand, Source: src, Alive: true, TUN: tun}
	}

	if cand, src := scanLocalProxyPorts(a.p.addr); cand != "" {
		return upstreamDetection{Proxy: cand, Source: src, Alive: true, TUN: tun}
	}

	if prev := strings.TrimSpace(prevAutoProxy); prev != "" && prev != defaultUpstream {
		if norm, u, err := normalizeUpstreamURL(prev); err == nil && u != nil && !isSelfHostPort(u.Host, a.p.addr) {
			if isLoopbackHost(u.Hostname()) {
				probeTarget, cleanup := newLoopbackProbeTarget()
				scheme, ok := probeLocalProxyPort(u.Host, probeTarget)
				cleanup()
				if ok {
					return upstreamDetection{Proxy: scheme + "://" + u.Host, Source: "上次可用节点", Alive: true, TUN: tun}
				}
			} else if isTCPReachable(u.Host, 150*time.Millisecond) {
				return upstreamDetection{Proxy: norm, Source: "上次可用节点", Alive: true, TUN: tun}
			}
		}
	}

	if tun != "" {
		return upstreamDetection{Proxy: "", Source: "TUN / 加速器通道 (" + tun + ")", Alive: false, TUN: tun}
	}
	return upstreamDetection{Proxy: "", Source: "直连（未检测到本地代理）", Alive: false, TUN: ""}
}
