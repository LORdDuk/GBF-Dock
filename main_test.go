package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const assetURL = "https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/sp/a.png?v=1"

// 假 CDN：记录每次请求，支持 gzip、ETag/304；内容随 version 和查询串变化
type fakeCDN struct {
	mu      sync.Mutex
	reqs    []string // 每次请求的 If-None-Match|Range|Accept-Encoding
	version string
	delay   time.Duration
}

func (c *fakeCDN) body(q string) []byte {
	return append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("granblue-"+c.version+"-"+q+";"), 4000)...)
}

func (c *fakeCDN) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	time.Sleep(c.delay)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reqs = append(c.reqs, r.Header.Get("If-None-Match")+"|"+r.Header.Get("Range")+"|"+r.Header.Get("Accept-Encoding"))
	etag := `"` + c.version + r.URL.RawQuery + `"`
	ct := "image/png"
	if strings.HasSuffix(r.URL.Path, ".js") {
		ct = "application/javascript"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Etag", etag)
	w.Header().Set("Set-Cookie", "akamai=1")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := c.body(r.URL.RawQuery)
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		gz.Write(body)
		gz.Close()
		return
	}
	w.Write(body)
}

func (c *fakeCDN) count() int { c.mu.Lock(); defer c.mu.Unlock(); return len(c.reqs) }
func (c *fakeCDN) req(i int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reqs[i]
}

// rig 启动代理，把所有出站连接导向假 CDN（*.akamaized.net）或假 API 服务器（其他域名）
func rig(t *testing.T, dir string, ttl time.Duration, cdn *fakeCDN) (*http.Client, *app, *httptest.Server) {
	p, err := newProxy(dir, filepath.Join(dir, "certs"), "127.0.0.1:0", ttl)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(p, &logRing{})
	pool := x509.NewCertPool()
	pool.AddCert(p.ca)

	up := httptest.NewUnstartedServer(cdn)
	// 假 CDN 按请求的域名出示证书，这样各个 CDN 域名都能测
	up.TLS = &tls.Config{GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) { return p.leaf(h.ServerName) }}
	up.StartTLS()
	t.Cleanup(up.Close)
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "api ok") }))
	t.Cleanup(api.Close)

	p.tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	p.dial = func(ctx context.Context, n, addr string) (net.Conn, error) {
		target := up.Listener.Addr().String()
		if !strings.Contains(addr, "akamaized.net") {
			target = api.Listener.Addr().String()
		}
		return (&net.Dialer{}).DialContext(ctx, n, target)
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	proxyURL, _ := url.Parse(front.URL)
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: pool}, ForceAttemptHTTP2: true,
	}}
	return client, a, api
}

func fetchURL(c *http.Client, u string, hdr ...string) (*http.Response, []byte, error) {
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return resp, b, err
}

func get(t *testing.T, c *http.Client, u string, hdr ...string) (*http.Response, []byte) {
	t.Helper()
	resp, b, err := fetchURL(c, u, hdr...)
	if err != nil {
		t.Fatal(err)
	}
	return resp, b
}

func TestCacheFlow(t *testing.T) {
	dir := t.TempDir()
	cdn := &fakeCDN{version: "v1"}
	client, _, api := rig(t, dir, time.Hour, cdn)
	want := cdn.body("v=1")

	// 1. 首次：从 CDN 下载（gzip 传输），浏览器拿到完整原文，走 HTTP/2
	resp, b := get(t, client, assetURL)
	if resp.StatusCode != 200 || !bytes.Equal(b, want) {
		t.Fatalf("miss: status %d, body ok=%v", resp.StatusCode, bytes.Equal(b, want))
	}
	if resp.ProtoMajor != 2 {
		t.Errorf("want HTTP/2 to browser, got %s", resp.Proto)
	}
	if cdn.count() != 1 || !strings.HasSuffix(cdn.req(0), "|gzip") {
		t.Fatalf("upstream reqs = %q", cdn.reqs)
	}

	// 2. 再次：本地命中，不碰 CDN；Set-Cookie 不缓存，ETag 保留
	resp, b = get(t, client, assetURL)
	if !bytes.Equal(b, want) || cdn.count() != 1 {
		t.Fatalf("hit: body ok=%v upstream=%d", bytes.Equal(b, want), cdn.count())
	}
	if resp.Header.Get("Set-Cookie") != "" || resp.Header.Get("Etag") != `"v1v=1"` || resp.Header.Get("Content-Type") != "image/png" {
		t.Errorf("hit headers: %v", resp.Header)
	}

	// 3. 断点续传（音频常用）：本地切片返回 206
	resp, b = get(t, client, assetURL, "Range", "bytes=0-9")
	if resp.StatusCode != 206 || string(b) != string(want[:10]) || cdn.count() != 1 {
		t.Fatalf("range: %d %q upstream=%d", resp.StatusCode, b, cdn.count())
	}

	// 4. 浏览器条件请求：本地直接 304
	resp, _ = get(t, client, assetURL, "If-None-Match", `"v1v=1"`)
	if resp.StatusCode != 304 || cdn.count() != 1 {
		t.Fatalf("conditional: %d upstream=%d", resp.StatusCode, cdn.count())
	}

	// 5. 游戏 API 域名：原样隧道，浏览器看到的是真实服务器证书（代理没解密）
	apiPool := x509.NewCertPool()
	apiPool.AddCert(api.Certificate())
	apiClient := &http.Client{Transport: &http.Transport{
		Proxy: client.Transport.(*http.Transport).Proxy, TLSClientConfig: &tls.Config{RootCAs: apiPool, ServerName: "example.com"},
	}}
	_, ab := get(t, apiClient, "https://game.granbluefantasy.jp/rest/test")
	if string(ab) != "api ok" {
		t.Fatalf("api body %q", ab)
	}

	// 6. 缓存过期（ttl=0 的新进程，复用同一目录和证书）：先秒回旧缓存，后台 304 确认
	client0, _, _ := rig(t, dir, 0, cdn)
	if _, b = get(t, client0, assetURL); !bytes.Equal(b, want) {
		t.Fatal("stale hit body mismatch")
	}
	waitFor(t, func() bool { return cdn.count() == 2 })
	if !strings.HasPrefix(cdn.req(1), `"v1v=1"|`) {
		t.Fatalf("revalidation should be conditional, got %q", cdn.req(1))
	}

	// 7. 官方更新了文件（URL 不变）：本次仍回旧版，后台拉到新版，下一次就是新版
	cdn.mu.Lock()
	cdn.version = "v2"
	cdn.mu.Unlock()
	if _, b = get(t, client0, assetURL); !bytes.Equal(b, want) {
		t.Fatal("expected old body while refreshing")
	}
	waitFor(t, func() bool { return cdn.count() == 3 })
	waitFor(t, func() bool { _, b = get(t, client0, assetURL); return bytes.Equal(b, cdn.body("v=1")) })
}

func TestMergeAndVersions(t *testing.T) {
	dir := t.TempDir()
	cdn := &fakeCDN{version: "v1", delay: 150 * time.Millisecond}
	client, a, _ := rig(t, dir, time.Hour, cdn)

	// 10 个并发请求同一个还没缓存的文件：只向 CDN 下载一次
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, b, err := fetchURL(client, assetURL)
			if err != nil || !bytes.Equal(b, cdn.body("v=1")) {
				t.Errorf("concurrent fetch: err=%v body ok=%v", err, bytes.Equal(b, cdn.body("v=1")))
			}
		}()
	}
	wg.Wait()
	if cdn.count() != 1 || a.p.merged.Load() == 0 {
		t.Fatalf("upstream=%d merged=%d, want 1 download shared by the rest", cdn.count(), a.p.merged.Load())
	}

	// 资源换了版本号（查询串）：重新下载，同一路径只留最新一份
	if _, b := get(t, client, strings.Replace(assetURL, "v=1", "v=2", 1)); !bytes.Equal(b, cdn.body("v=2")) {
		t.Fatal("v=2 body mismatch")
	}
	if cdn.count() != 2 || countFiles(t, dir) != 1 {
		t.Fatalf("upstream=%d files=%d, want 2 and 1", cdn.count(), countFiles(t, dir))
	}
	// 再请求旧版本号：不会把新版当旧版返回
	if _, b := get(t, client, assetURL); !bytes.Equal(b, cdn.body("v=1")) || cdn.count() != 3 {
		t.Fatalf("old version must not be served from the new entry (upstream=%d)", cdn.count())
	}

	// 路径里的时间戳版本号也一样
	js := "https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/1729002592/js/a.js"
	get(t, client, js)
	get(t, client, strings.Replace(js, "1729002592", "1729999999", 1))
	if n := countFiles(t, dir); n != 2 {
		t.Fatalf("want 2 cached files (image + js), got %d", n)
	}
}

func TestLocalAPI(t *testing.T) {
	dir := t.TempDir()
	p, err := newProxy(dir, filepath.Join(dir, "certs"), "127.0.0.1:8765", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	newApp(p, &logRing{})
	do := func(method, path, host string, header bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Host = host
		if header {
			r.Header.Set(csrfHeaderName, "1")
		}
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		return w
	}
	const self = "127.0.0.1:8765"

	if w := do("GET", "/", self, false); w.Code != 200 || !strings.Contains(w.Body.String(), "GBF-Dock") {
		t.Fatalf("ui: %d", w.Code)
	}
	if w := do("GET", "/proxy.pac", self, false); !strings.Contains(w.Body.String(), "PROXY 127.0.0.1:8765; DIRECT") {
		t.Fatalf("pac: %q", w.Body.String())
	}
	var st map[string]any
	w := do("GET", "/api/status", self, false)
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil || st["hits"] == nil || st["system"] == nil || st["version"] != appVersion {
		t.Fatalf("status: %d %s", w.Code, w.Body.String())
	}

	// 别的网站发起的写操作：没有自定义头、或 Host 不是本机（DNS 重绑定）一律拒绝
	if w := do("POST", "/api/cache-clear", self, false); w.Code != http.StatusForbidden {
		t.Fatalf("missing header must be rejected, got %d", w.Code)
	}
	if w := do("POST", "/api/cache-clear", "evil.example:8765", true); w.Code != http.StatusForbidden {
		t.Fatalf("foreign host must be rejected, got %d", w.Code)
	}
	if w := do("GET", "/api/status", "evil.example:8765", false); w.Code != http.StatusForbidden {
		t.Fatalf("foreign host must not read status, got %d", w.Code)
	}
	if w := do("POST", "/api/cache-clear", self, true); w.Code != 200 || !strings.Contains(w.Body.String(), "缓存已清空") {
		t.Fatalf("cache-clear: %d %s", w.Code, w.Body.String())
	}
	doBody := func(method, path, host, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Host = host
		r.Header.Set("X-GBF-Dock", "1")
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		return w
	}

	if w := doBody("POST", "/api/set-upstream", self, `{"mode":"custom","proxy":"127.0.0.1:7890"}`); w.Code != 200 || !strings.Contains(w.Body.String(), "127.0.0.1:7890") {
		t.Fatalf("set-upstream custom: %d %s", w.Code, w.Body.String())
	}
	if w := doBody("POST", "/api/set-upstream", self, `{"mode":"custom","proxy":"127.0.0.1:8765"}`); w.Code != 200 || !strings.Contains(w.Body.String(), "error") {
		t.Fatalf("set-upstream self loop must error: %d %s", w.Code, w.Body.String())
	}
	if w := doBody("POST", "/api/set-upstream", self, `{"mode":"direct"}`); w.Code != 200 || !strings.Contains(w.Body.String(), "直连") {
		t.Fatalf("set-upstream direct: %d %s", w.Code, w.Body.String())
	}
	if w := do("POST", "/api/nope", self, true); w.Code != 404 {
		t.Fatalf("unknown action: %d", w.Code)
	}
}

// 旧版本把证书放在程序目录；新版本应搬到固定的用户目录并沿用同一张，不用重装
func TestCertsDirMigration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("AppData", filepath.Join(home, "AppData"))

	oldDir := t.TempDir()
	old, err := newProxy(oldDir, filepath.Join(oldDir, "certs"), "127.0.0.1:0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	dir := certsDir(oldDir)
	if !strings.HasPrefix(dir, home) {
		t.Fatalf("certs should move to the user config dir, got %s", dir)
	}
	moved, err := newProxy(oldDir, dir, "127.0.0.1:0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(moved.ca.Raw, old.ca.Raw) {
		t.Fatal("migrated CA must be the same certificate the user already trusted")
	}
	if runtime.GOOS != "windows" {
		if st, err := os.Stat(filepath.Join(dir, "ca.key")); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("ca.key must stay private: %v %v", st.Mode(), err)
		}
	} else if st, err := os.Stat(filepath.Join(dir, "ca.key")); err != nil || st.Size() == 0 {
		t.Fatalf("ca.key missing: %v", err)
	}
	// 换个解压位置（没有旧 certs）：仍用同一张证书
	if again := certsDir(t.TempDir()); again != dir {
		t.Fatalf("a new folder should reuse %s, got %s", dir, again)
	}
}

// “清理旧证书”只能删以前版本的历史根证书：不能删当前这张，也不能删别人的证书
func TestIsOldCA(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	cur, err := newProxy(d1, filepath.Join(d1, "certs"), "127.0.0.1:0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	prev, err := newProxy(d2, filepath.Join(d2, "certs"), "127.0.0.1:0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(cur, &logRing{})
	other := httptest.NewTLSServer(http.NotFoundHandler())
	defer other.Close()
	switch {
	case a.isOldCA(cur.ca.Raw):
		t.Fatal("must never remove the CA currently in use")
	case !a.isOldCA(prev.ca.Raw):
		t.Fatal("a CA from an older install must be offered for cleanup")
	case a.isOldCA(other.Certificate().Raw):
		t.Fatal("must not touch unrelated certificates")
	}
}

func TestAssetHost(t *testing.T) {
	for host, want := range map[string]bool{
		"prd-game-a-granbluefantasy.akamaized.net":        true,
		"prd-game-a5-granbluefantasy.akamaized.net":       true,
		"granbluefantasy.akamaized.net":                   true,
		"prd-game-a.granbluefantasy.akamaized.net":        true,
		"gbf.akamaized.net":                               true,
		"cdn1.gbf.akamaized.net":                          true,
		"prd-game-a-gbf.akamaized.net":                    true,
		"prd-game-a5-gbf.akamaized.net":                   true,
		"game.granbluefantasy.jp":                         false, // 游戏 API：绝不解密
		"gbf.game.mbga.jp":                                false, // 平台登录/接口：绝不解密
		"prd-game-a-granbluefantasy-steam.akamaized.net":  true,  // Steam 版 GBF CDN
		"prd-game-a5-granbluefantasy-steam.akamaized.net": true,  // Steam 版 GBF 分片
		"other-granbluefantasy-steam.akamaized.net":       false,
		"other-gbf.akamaized.net":                         false,
		"notgbf.akamaized.net":                            false,
		"evil-granbluefantasy.akamaized.net":              false,
		"prd-game-a-granbluefantasy.akamaized.net.evil.x": false,
	} {
		if got := assetHost.MatchString(host); got != want {
			t.Errorf("assetHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// “检测浏览器”的页面由本程序直接回答（兼容新版与旧版检测路径），不访问 CDN；新增的 CDN 域名也能正常缓存
func TestBrowserCheckAndNewHosts(t *testing.T) {
	cdn := &fakeCDN{version: "v1"}
	client, a, _ := rig(t, t.TempDir(), time.Hour, cdn)

	resp, b := get(t, client, "https://prd-game-a-granbluefantasy.akamaized.net"+checkPath+"?t=123")
	if resp.StatusCode != 200 || !strings.Contains(string(b), "检测通过") || !strings.Contains(string(b), "GBF-Dock") || cdn.count() != 0 || a.p.checkedAt.Load() == 0 {
		t.Fatalf("check page: status=%d upstream=%d checkedAt=%d", resp.StatusCode, cdn.count(), a.p.checkedAt.Load())
	}
	respOld, bOld := get(t, client, "https://prd-game-a-granbluefantasy.akamaized.net"+legacyCheckPath+"?t=124")
	if respOld.StatusCode != 200 || !strings.Contains(string(bOld), "检测通过") || cdn.count() != 0 {
		t.Fatalf("legacy check page failed: status=%d upstream=%d", respOld.StatusCode, cdn.count())
	}

	u := "https://cdn1.gbf.akamaized.net/assets_en/img/x.png?v=1"
	get(t, client, u)
	if _, b := get(t, client, u); !bytes.Equal(b, cdn.body("v=1")) || cdn.count() != 1 || a.p.hits.Load() != 1 {
		t.Fatalf("gbf.akamaized.net should be cached: upstream=%d hits=%d", cdn.count(), a.p.hits.Load())
	}
}

func TestPACFallback(t *testing.T) {
	for in, want := range map[string]string{
		"":               "DIRECT",
		"127.0.0.1:7890": "PROXY 127.0.0.1:7890; DIRECT",
		"http=127.0.0.1:10809;https=127.0.0.1:10808": "PROXY 127.0.0.1:10808; DIRECT",
		"http=127.0.0.1:10809":                       "PROXY 127.0.0.1:10809; DIRECT",
		"socks=127.0.0.1:1080":                       "SOCKS5 127.0.0.1:1080; DIRECT",
		"ftp=1.2.3.4:21":                             "DIRECT",
	} {
		if got := pacFallback(in); got != want {
			t.Errorf("pacFallback(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRefusesNonAssetCert(t *testing.T) {
	d := t.TempDir()
	p, err := newProxy(d, filepath.Join(d, "certs"), "127.0.0.1:0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.tlsConfig.GetCertificate(&tls.ClientHelloInfo{ServerName: "accounts.google.com"}); err == nil {
		t.Fatal("must refuse to mint certs for non-GBF hosts")
	}
	// 名称约束：就算有人拿私钥签了别的域名，验证也会失败
	pool := x509.NewCertPool()
	pool.AddCert(p.ca)
	leaf, _ := p.leaf("evil.example.com")
	c, _ := x509.ParseCertificate(leaf.Certificate[0])
	if _, err := c.Verify(x509.VerifyOptions{Roots: pool, DNSName: "evil.example.com"}); err == nil {
		t.Fatal("name constraints not enforced")
	}
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	filepath.WalkDir(filepath.Join(dir, "cache"), func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !isInternalCacheFile(d.Name()) {
			n++
		}
		return nil
	})
	return n
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}

// TestUpstreamProxyChaining 验证：
// 1. 未缓存静态资源会通过上游代理下载并落盘，随后直接本地命中；
// 2. 游戏 API 与桌面端 Gemini 等非静态资源绝不解密，原样通过上游代理隧道转发；
// 3. 防死循环检查会拒绝把自身监听地址设为上游代理。
func TestUpstreamProxyChaining(t *testing.T) {
	dir := t.TempDir()
	p, err := newProxy(dir, filepath.Join(dir, "certs"), "127.0.0.1:18765", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	newApp(p, &logRing{})

	pool := x509.NewCertPool()
	pool.AddCert(p.ca)

	cdn := &fakeCDN{version: "v1"}
	upCDN := httptest.NewUnstartedServer(cdn)
	upCDN.TLS = &tls.Config{GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) { return p.leaf(h.ServerName) }}
	upCDN.StartTLS()
	t.Cleanup(upCDN.Close)

	apiSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "upstream-"+r.Host)
	}))
	t.Cleanup(apiSrv.Close)

	var upMu sync.Mutex
	var connectHosts []string
	upProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "expect CONNECT", http.StatusMethodNotAllowed)
			return
		}
		upMu.Lock()
		connectHosts = append(connectHosts, r.Host)
		upMu.Unlock()

		dest := apiSrv.Listener.Addr().String()
		if strings.Contains(r.Host, "akamaized.net") {
			dest = upCDN.Listener.Addr().String()
		}
		backend, err := net.Dial("tcp", dest)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		conn, brw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			backend.Close()
			return
		}
		conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		relayTunnel(bufConn{conn, brw.Reader}, backend)
	}))
	t.Cleanup(upProxy.Close)

	// 拒绝自回环
	if err := p.setUpstream("127.0.0.1:18765"); err == nil {
		t.Fatal("must reject self-loop upstream proxy")
	}
	if err := p.setUpstream(upProxy.URL); err != nil {
		t.Fatalf("setUpstream(%s): %v", upProxy.URL, err)
	}

	p.tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	frontURL, _ := url.Parse(front.URL)

	cdnClient := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(frontURL), TLSClientConfig: &tls.Config{RootCAs: pool}, ForceAttemptHTTP2: true,
	}}
	// 首次静态资源应经由上游代理拉取并缓存
	resp, b := get(t, cdnClient, assetURL)
	if resp.StatusCode != 200 || !bytes.Equal(b, cdn.body("v=1")) {
		t.Fatalf("asset miss via upstream failed: %d", resp.StatusCode)
	}
	// 再次请求命中本地，不能再触发上游代理连接
	upMu.Lock()
	afterMiss := len(connectHosts)
	upMu.Unlock()
	if afterMiss != 1 {
		t.Fatalf("expected 1 upstream CONNECT on CDN miss, got %v", connectHosts)
	}
	if _, b = get(t, cdnClient, assetURL); !bytes.Equal(b, cdn.body("v=1")) {
		t.Fatal("second request mismatch")
	}
	upMu.Lock()
	afterHit := len(connectHosts)
	upMu.Unlock()
	if afterHit != 1 || cdn.count() != 1 {
		t.Fatalf("hit should not hit upstream proxy: connects=%d cdn=%d", afterHit, cdn.count())
	}

	// 非静态资源（游戏 API 与桌面端 Gemini）原样经上游代理透传且未解密 TLS
	apiPool := x509.NewCertPool()
	apiPool.AddCert(apiSrv.Certificate())
	tunnelClient := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(frontURL),
		TLSClientConfig: &tls.Config{RootCAs: apiPool, ServerName: "example.com"},
	}}
	for _, u := range []string{"https://game.granbluefantasy.jp/rest/test", "https://gemini.google.com/app"} {
		_, body := get(t, tunnelClient, u)
		if !strings.HasPrefix(string(body), "upstream-") {
			t.Fatalf("unexpected tunnel body for %s: %q", u, body)
		}
	}

	upMu.Lock()
	gotConnects := append([]string(nil), connectHosts...)
	upMu.Unlock()
	if !strings.Contains(strings.Join(gotConnects, ","), "gemini.google.com:443") ||
		!strings.Contains(strings.Join(gotConnects, ","), "game.granbluefantasy.jp:443") {
		t.Fatalf("expected non-asset hosts tunneled via upstream proxy, got %v", gotConnects)
	}
}

func TestLiveCacheUsageCounters(t *testing.T) {
	dir := t.TempDir()
	cdn := &fakeCDN{version: "v1"}
	client, a, _ := rig(t, dir, time.Hour, cdn)

	get(t, client, assetURL)
	if files := a.p.cachedFiles.Load(); files != 1 {
		t.Fatalf("cachedFiles = %d, want 1", files)
	}
	sizeV1 := a.p.cachedBytes.Load()
	if sizeV1 <= 0 {
		t.Fatalf("cachedBytes = %d, want > 0", sizeV1)
	}

	// 同路径新版本覆盖：文件数仍为 1，体积同步更新且与磁盘扫描一致
	get(t, client, strings.Replace(assetURL, "v=1", "v=2000", 1))
	if files := a.p.cachedFiles.Load(); files != 1 {
		t.Fatalf("after overwrite cachedFiles = %d, want 1", files)
	}
	liveBytes := a.p.cachedBytes.Load()
	diskFiles, diskBytes := a.p.scanCacheUsage()
	if diskFiles != 1 || liveBytes != diskBytes {
		t.Fatalf("live=(1,%d) != disk=(%d,%d)", liveBytes, diskFiles, diskBytes)
	}

	if err := a.p.clearCache(); err != nil {
		t.Fatal(err)
	}
	if a.p.cachedFiles.Load() != 0 || a.p.cachedBytes.Load() != 0 {
		t.Fatalf("after clear: files=%d bytes=%d", a.p.cachedFiles.Load(), a.p.cachedBytes.Load())
	}
}

func startFakeSOCKS5Proxy(t *testing.T, cdnAddr, apiAddr string) (addr string, port int, recorded *[]string, mu *sync.Mutex) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	var hostsMu sync.Mutex
	var hosts []string

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				var head [2]byte
				if _, err := io.ReadFull(conn, head[:]); err != nil || head[0] != 0x05 {
					return
				}
				methods := make([]byte, int(head[1]))
				if _, err := io.ReadFull(conn, methods); err != nil {
					return
				}
				if _, err := conn.Write([]byte{0x05, 0x00}); err != nil {
					return
				}

				var reqHead [4]byte
				if _, err := io.ReadFull(conn, reqHead[:]); err != nil || reqHead[0] != 0x05 || reqHead[1] != 0x01 {
					return
				}
				var host string
				switch reqHead[3] {
				case 0x01:
					var ip4 [4]byte
					if _, err := io.ReadFull(conn, ip4[:]); err != nil {
						return
					}
					host = net.IP(ip4[:]).String()
				case 0x03:
					var dl [1]byte
					if _, err := io.ReadFull(conn, dl[:]); err != nil {
						return
					}
					dom := make([]byte, int(dl[0]))
					if _, err := io.ReadFull(conn, dom); err != nil {
						return
					}
					host = string(dom)
				case 0x04:
					var ip6 [16]byte
					if _, err := io.ReadFull(conn, ip6[:]); err != nil {
						return
					}
					host = net.IP(ip6[:]).String()
				default:
					return
				}
				var pb [2]byte
				if _, err := io.ReadFull(conn, pb[:]); err != nil {
					return
				}
				targetPort := int(binary.BigEndian.Uint16(pb[:]))

				hostsMu.Lock()
				hosts = append(hosts, net.JoinHostPort(host, strconv.Itoa(targetPort)))
				hostsMu.Unlock()

				dest := apiAddr
				if strings.Contains(host, "akamaized.net") {
					dest = cdnAddr
				}
				backend, err := net.DialTimeout("tcp", dest, 3*time.Second)
				if err != nil {
					_, _ = conn.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
					return
				}
				if _, err := conn.Write([]byte{0x05, 0x00, 0x00, 0x01, 0, 0, 0, 0, 0, 0}); err != nil {
					_ = backend.Close()
					return
				}
				relayTunnel(conn, backend)
			}(c)
		}
	}()

	tcpAddr := ln.Addr().(*net.TCPAddr)
	return tcpAddr.String(), tcpAddr.Port, &hosts, &hostsMu
}

func TestUpstreamAutoDetectAndSOCKS5(t *testing.T) {
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("ALL_PROXY", "")
	t.Setenv("all_proxy", "")

	oldExtra := extraScanPorts
	oldCands := candidateProxyPorts
	t.Cleanup(func() {
		extraScanPorts = oldExtra
		candidateProxyPorts = oldCands
	})
	candidateProxyPorts = nil

	dir := t.TempDir()
	p, err := newProxy(dir, filepath.Join(dir, "certs"), "127.0.0.1:18766", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(p, &logRing{})
	a.sysFn = func() sysState { return sysState{} }

	pool := x509.NewCertPool()
	pool.AddCert(p.ca)

	cdn := &fakeCDN{version: "v1"}
	upCDN := httptest.NewUnstartedServer(cdn)
	upCDN.TLS = &tls.Config{GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) { return p.leaf(h.ServerName) }}
	upCDN.StartTLS()
	t.Cleanup(upCDN.Close)

	apiSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "socks5-tunnel-"+r.Host)
	}))
	t.Cleanup(apiSrv.Close)

	// 1. 普通 HTTP 服务器必须被握手探测排除，且无活跃代理时不能写死 127.0.0.1:7890
	ordinaryHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not a proxy", http.StatusMethodNotAllowed)
	}))
	t.Cleanup(ordinaryHTTP.Close)
	httpU, _ := url.Parse(ordinaryHTTP.URL)
	httpPort, _ := strconv.Atoi(httpU.Port())
	extraScanPorts = []candPort{{port: httpPort, name: "普通HTTP误报测试"}}

	const self = "127.0.0.1:18766"
	detectReq := httptest.NewRequest(http.MethodPost, "/api/detect-upstream", nil)
	detectReq.Host = self
	detectReq.Header.Set("X-GBF-Dock", "1")
	dw := httptest.NewRecorder()
	p.ServeHTTP(dw, detectReq)
	if dw.Code != 200 {
		t.Fatalf("detect-upstream failed: %d %s", dw.Code, dw.Body.String())
	}
	if up := p.getUpstream(); up != "" {
		t.Fatalf("ordinary HTTP server or dead 7890 must not be picked as upstream, got %q", up)
	}

	// 2. 启动 SOCKS5 代理并加入扫描列表：应自动选中 socks5:// 代理，并打通 CDN 缓存 + API 加密隧道
	_, socksPort, socksHosts, socksMu := startFakeSOCKS5Proxy(t, upCDN.Listener.Addr().String(), apiSrv.Listener.Addr().String())
	extraScanPorts = []candPort{
		{port: httpPort, name: "普通HTTP误报测试"},
		{port: socksPort, name: "测试SOCKS5"},
	}

	detectReq2 := httptest.NewRequest(http.MethodPost, "/api/detect-upstream", nil)
	detectReq2.Host = self
	detectReq2.Header.Set("X-GBF-Dock", "1")
	dw2 := httptest.NewRecorder()
	p.ServeHTTP(dw2, detectReq2)
	if dw2.Code != 200 {
		t.Fatalf("detect-upstream(socks5): %d %s", dw2.Code, dw2.Body.String())
	}

	stReq := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	stReq.Host = self
	sw := httptest.NewRecorder()
	p.ServeHTTP(sw, stReq)
	var st map[string]any
	if err := json.Unmarshal(sw.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	wantSOCKS := "socks5://127.0.0.1:" + strconv.Itoa(socksPort)
	if st["upstreamProxy"] != wantSOCKS {
		t.Fatalf("upstreamProxy = %v, want %q", st["upstreamProxy"], wantSOCKS)
	}
	if st["upstreamAlive"] != true {
		t.Fatalf("upstreamAlive = %v, want true", st["upstreamAlive"])
	}
	if src, _ := st["upstreamSource"].(string); !strings.Contains(src, "端口扫描") {
		t.Fatalf("upstreamSource = %q, want 端口扫描", src)
	}

	p.tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	frontURL, _ := url.Parse(front.URL)

	cdnClient := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(frontURL), TLSClientConfig: &tls.Config{RootCAs: pool}, ForceAttemptHTTP2: true,
	}}
	// 未命中 CDN 静态资源走 SOCKS5 上游下载，再次请求命中本地缓存不再经过 SOCKS5
	resp, b := get(t, cdnClient, assetURL)
	if resp.StatusCode != 200 || !bytes.Equal(b, cdn.body("v=1")) {
		t.Fatalf("asset via socks5 failed: %d", resp.StatusCode)
	}
	socksMu.Lock()
	afterMiss := len(*socksHosts)
	socksMu.Unlock()
	if afterMiss != 1 {
		t.Fatalf("want 1 SOCKS5 request on asset miss, got %d (%v)", afterMiss, *socksHosts)
	}
	if _, b = get(t, cdnClient, assetURL); !bytes.Equal(b, cdn.body("v=1")) {
		t.Fatal("cached asset mismatch after socks5")
	}
	socksMu.Lock()
	afterHit := len(*socksHosts)
	socksMu.Unlock()
	if afterHit != 1 || cdn.count() != 1 {
		t.Fatalf("hit should not contact SOCKS5: socks=%d cdn=%d", afterHit, cdn.count())
	}

	// 非静态资源（游戏 API）通过 SOCKS5 原样 TLS 隧道透传，不经过 MITM
	apiPool := x509.NewCertPool()
	apiPool.AddCert(apiSrv.Certificate())
	tunnelClient := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(frontURL),
		TLSClientConfig: &tls.Config{RootCAs: apiPool, ServerName: "example.com"},
	}}
	_, apiBody := get(t, tunnelClient, "https://game.granbluefantasy.jp/rest/test")
	if string(apiBody) != "socks5-tunnel-game.granbluefantasy.jp" {
		t.Fatalf("unexpected SOCKS5 API tunnel body: %q", apiBody)
	}

	// 3. 第三方 PAC 解析：SOCKS5 指令与普通 HTTP 代理端口均可提取
	pacSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `function FindProxyForURL(u, h) { return "SOCKS5 127.0.0.1:`+strconv.Itoa(socksPort)+`; DIRECT"; }`)
	}))
	t.Cleanup(pacSrv.Close)
	if got := extractUpstreamFromPAC(pacSrv.URL, self, a.pacURL); got != wantSOCKS {
		t.Fatalf("extractUpstreamFromPAC = %q, want %q", got, wantSOCKS)
	}
}

func TestStreamThroughAndTruncation(t *testing.T) {
	dir := t.TempDir()
	p, err := newProxy(dir, filepath.Join(dir, "certs"), "127.0.0.1:0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	newApp(p, &logRing{})

	pool := x509.NewCertPool()
	pool.AddCert(p.ca)

	releaseSecond := make(chan struct{})
	var relOnce sync.Once
	t.Cleanup(func() { relOnce.Do(func() { close(releaseSecond) }) })

	chunk1 := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("A"), 200)...)
	chunk2 := bytes.Repeat([]byte("B"), 200)

	var srvCalls atomic.Int64
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srvCalls.Add(1)
		if strings.HasSuffix(r.URL.Path, "trunc.png") {
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Content-Length", "4096")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(chunk1)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			panic(http.ErrAbortHandler)
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Etag", `"stream-v1"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(chunk1)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		select {
		case <-releaseSecond:
		case <-time.After(5 * time.Second):
		}
		_, _ = w.Write(chunk2)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}))
	up.EnableHTTP2 = true
	up.TLS = &tls.Config{GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) { return p.leaf(h.ServerName) }}
	up.StartTLS()
	t.Cleanup(up.Close)

	p.tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	p.dial = func(ctx context.Context, n, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, n, up.Listener.Addr().String())
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	frontURL, _ := url.Parse(front.URL)

	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(frontURL), TLSClientConfig: &tls.Config{RootCAs: pool}, ForceAttemptHTTP2: true,
	}}

	// 1. 首块数据必须在服务端发完第二块之前即推送给客户端
	req, _ := http.NewRequest(http.MethodGet, "https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/sp/stream.png", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	firstRead := make([]byte, len(chunk1))
	if _, err := io.ReadFull(resp.Body, firstRead); err != nil {
		t.Fatalf("failed reading first streamed chunk before release: %v", err)
	}
	if !bytes.Equal(firstRead, chunk1) {
		t.Fatalf("first streamed chunk mismatch")
	}
	relOnce.Do(func() { close(releaseSecond) })
	rest, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rest, chunk2) {
		t.Fatalf("second chunk mismatch: got %d bytes", len(rest))
	}
	if n := countFiles(t, dir); n != 1 {
		t.Fatalf("completed stream must be cached (files=%d)", n)
	}

	// 再次请求走本地命中 + 304 内存索引短路
	_, hitBody := get(t, client, "https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/sp/stream.png")
	if !bytes.Equal(hitBody, append(append([]byte(nil), chunk1...), chunk2...)) || srvCalls.Load() != 1 {
		t.Fatalf("second request should hit disk cache cleanly (calls=%d)", srvCalls.Load())
	}
	r304, _ := get(t, client, "https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/sp/stream.png", "If-None-Match", `"stream-v1"`)
	if r304.StatusCode != http.StatusNotModified || srvCalls.Load() != 1 {
		t.Fatalf("304 metadata shortcut failed: status=%d calls=%d", r304.StatusCode, srvCalls.Load())
	}

	// 2. 半截截断响应绝不能写入缓存
	_, _, _ = fetchURL(client, "https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/sp/trunc.png")
	if n := countFiles(t, dir); n != 1 {
		t.Fatalf("truncated stream must not be cached, files=%d", n)
	}
}

func TestMagicBytesAndCacheInspect(t *testing.T) {
	dir := t.TempDir()
	p, err := newProxy(dir, filepath.Join(dir, "certs"), "127.0.0.1:8767", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	newApp(p, &logRing{})

	pool := x509.NewCertPool()
	pool.AddCert(p.ca)

	validPNG := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("PNGDATA"), 32)...)
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/assets_en/img/sp/empty.png":
			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(http.StatusOK)
		case "/assets_en/img/sp/hijack.png":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, "<html><body>502 Bad Gateway</body></html>")
		case "/assets_en/img/sp/badhdr.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = io.WriteString(w, "NOT_A_REAL_PNG_FILE_1234567890")
		default:
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(validPNG)
		}
	}))
	up.TLS = &tls.Config{GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) { return p.leaf(h.ServerName) }}
	up.StartTLS()
	t.Cleanup(up.Close)

	p.tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	p.dial = func(ctx context.Context, n, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, n, up.Listener.Addr().String())
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	frontURL, _ := url.Parse(front.URL)

	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(frontURL), TLSClientConfig: &tls.Config{RootCAs: pool},
	}}

	for _, bad := range []string{"empty.png", "hijack.png", "badhdr.png"} {
		_, _ = get(t, client, "https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/sp/"+bad)
	}
	if n := countFiles(t, dir); n != 0 {
		t.Fatalf("corrupt/hijacked responses must not be cached, got files=%d", n)
	}

	_, got := get(t, client, "https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/sp/good.png")
	if !bytes.Equal(got, validPNG) || countFiles(t, dir) != 1 {
		t.Fatalf("valid PNG should be cached cleanly (files=%d)", countFiles(t, dir))
	}

	// 手工向 cache/ 注入一个损坏的历史缓存文件，再通过 /api/cache-inspect 体检清理
	corruptTarget := "https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/sp/corrupt.png"
	corruptFile := p.cachePath(corruptTarget)
	_ = os.MkdirAll(filepath.Dir(corruptFile), 0o755)
	metaJSON, _ := json.Marshal(entryMeta{URL: corruptTarget, Header: http.Header{"Content-Type": {"image/png"}}})
	if err := os.WriteFile(corruptFile, append(metaJSON, []byte("\n<!doctype html>broken")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if countFiles(t, dir) != 2 {
		t.Fatalf("expected 2 cache files before inspect")
	}

	r := httptest.NewRequest(http.MethodPost, "/api/cache-inspect", nil)
	r.Host = "127.0.0.1:8767"
	r.Header.Set("X-GBF-Dock", "1")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "清理损坏/异常文件 1 个") {
		t.Fatalf("cache-inspect unexpected: %d %s", w.Code, w.Body.String())
	}
	if n := countFiles(t, dir); n != 1 {
		t.Fatalf("inspect should remove only the corrupt file and keep valid file, got files=%d", n)
	}
}

func TestSteamCDNAndColdCachePrune(t *testing.T) {
	if !assetHost.MatchString("prd-game-a1-granbluefantasy-steam.akamaized.net") {
		t.Fatal("assetHost must match Steam GBF CDN")
	}

	dir := t.TempDir()
	cdn := &fakeCDN{version: "v1"}
	client, _, _ := rig(t, dir, time.Hour, cdn)
	p := client.Transport.(*http.Transport)
	_ = p

	// 通过代理下载 Steam 版资源与常规资源
	steamURL := "https://prd-game-a1-granbluefantasy-steam.akamaized.net/assets_en/img/sp/steam.png"
	oldURL := "https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/sp/old.png"
	get(t, client, steamURL)
	get(t, client, oldURL)
	if countFiles(t, dir) != 2 {
		t.Fatalf("want 2 cached files, got %d", countFiles(t, dir))
	}

	// 测试本地创建的 proxy 用于校验 PAC 与冷门清理接口
	dir2 := t.TempDir()
	px, err := newProxy(dir2, filepath.Join(dir2, "certs"), "127.0.0.1:8768", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	newApp(px, &logRing{})

	pacReq := httptest.NewRequest(http.MethodGet, "/proxy.pac", nil)
	pacReq.Host = "127.0.0.1:8768"
	pw := httptest.NewRecorder()
	px.ServeHTTP(pw, pacReq)
	if !strings.Contains(pw.Body.String(), "granbluefantasy-steam.akamaized.net") {
		t.Fatalf("PAC must include Steam GBF CDN: %s", pw.Body.String())
	}

	freshPath := px.cachePath(steamURL)
	coldPath := px.cachePath(oldURL)
	for _, item := range []struct {
		target, fp string
		age        time.Duration
	}{
		{steamURL, freshPath, 24 * time.Hour},
		{oldURL, coldPath, 90 * 24 * time.Hour},
	} {
		_ = os.MkdirAll(filepath.Dir(item.fp), 0o755)
		mj, _ := json.Marshal(entryMeta{URL: item.target, Header: http.Header{"Content-Type": {"image/png"}}})
		payload := append(mj, '\n')
		payload = append(payload, cdn.body("1")...)
		if err := os.WriteFile(item.fp, payload, 0o644); err != nil {
			t.Fatal(err)
		}
		ts := time.Now().Add(-item.age)
		_ = os.Chtimes(item.fp, ts, ts)
	}

	prReq := httptest.NewRequest(http.MethodPost, "/api/cache-prune", strings.NewReader(`{"days":60}`))
	prReq.Host = "127.0.0.1:8768"
	prReq.Header.Set("X-GBF-Dock", "1")
	prW := httptest.NewRecorder()
	px.ServeHTTP(prW, prReq)
	if prW.Code != 200 || !strings.Contains(prW.Body.String(), "冷门缓存 1 个") {
		t.Fatalf("cache-prune unexpected: %d %s", prW.Code, prW.Body.String())
	}
	if fileExists(coldPath) || !fileExists(freshPath) {
		t.Fatalf("cold file should be removed (%v) and fresh file preserved (%v)", fileExists(coldPath), fileExists(freshPath))
	}
}

func TestLANIsolationAndMobileGuide(t *testing.T) {
	dir := t.TempDir()
	p, err := newProxy(dir, filepath.Join(dir, "certs"), "127.0.0.1:8769", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(p, &logRing{})

	if _, err := a.applyAllowLAN(true); err != nil {
		t.Fatalf("applyAllowLAN(true): %v", err)
	}

	reqAs := func(remoteAddr, host, method, path string, hdr bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = remoteAddr
		r.Host = host
		if hdr {
			r.Header.Set("X-GBF-Dock", "1")
		}
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		return w
	}

	// 1. 局域网手机 IP：可见移动端引导页、Host 自适应 PAC、根证书，但严禁访问任何 /api/* 管理接口
	if w := reqAs("192.168.1.50:54321", "192.168.1.10:8769", "GET", "/", false); w.Code != 200 || !strings.Contains(w.Body.String(), "GBF-Dock 移动端接入向导") || !strings.Contains(w.Body.String(), "192.168.1.10:8769/proxy.pac") {
		t.Fatalf("LAN mobile guide failed: %d %s", w.Code, w.Body.String())
	}
	if w := reqAs("192.168.1.50:54321", "192.168.1.10:8769", "GET", "/proxy.pac", false); w.Code != 200 || !strings.Contains(w.Body.String(), "PROXY 192.168.1.10:8769; DIRECT") {
		t.Fatalf("LAN PAC failed: %d %s", w.Code, w.Body.String())
	}
	if w := reqAs("192.168.1.50:54321", "192.168.1.10:8769", "GET", "/ca.crt", false); w.Code != 200 || w.Body.Len() == 0 {
		t.Fatalf("LAN ca.crt failed: %d", w.Code)
	}
	if w := reqAs("192.168.1.50:54321", "127.0.0.1:8769", "GET", "/api/status", false); w.Code != http.StatusForbidden {
		t.Fatalf("LAN must not access /api/status even with spoofed Host: %d", w.Code)
	}
	if w := reqAs("192.168.1.50:54321", "127.0.0.1:8769", "POST", "/api/cache-clear", true); w.Code != http.StatusForbidden {
		t.Fatalf("LAN must not access write API: %d", w.Code)
	}

	// 2. 公网 IP：即便 allowLAN 开启也全部 403 拒绝
	for _, path := range []string{"/", "/proxy.pac", "/ca.crt", "/api/status"} {
		if w := reqAs("8.8.8.8:54321", "127.0.0.1:8769", "GET", path, false); w.Code != http.StatusForbidden {
			t.Fatalf("public IP %s must be forbidden, got %d", path, w.Code)
		}
	}

	// 3. 本机回环：正常访问控制面板与 API，allowLAN 状态为 true
	if w := reqAs("127.0.0.1:54321", "127.0.0.1:8769", "GET", "/", false); w.Code != 200 || !strings.Contains(w.Body.String(), panelTitle) {
		t.Fatalf("localhost UI failed: %d", w.Code)
	}
	if w := reqAs("127.0.0.1:54321", "127.0.0.1:8769", "GET", "/api/status", false); w.Code != 200 || !strings.Contains(w.Body.String(), `"allowLAN":true`) {
		t.Fatalf("localhost /api/status missing allowLAN=true: %d %s", w.Code, w.Body.String())
	}

	// 4. 关闭局域网共享后，局域网 IP 立即返回 403
	if _, err := a.applyAllowLAN(false); err != nil {
		t.Fatalf("applyAllowLAN(false): %v", err)
	}
	if w := reqAs("192.168.1.50:54321", "192.168.1.10:8769", "GET", "/", false); w.Code != http.StatusForbidden {
		t.Fatalf("LAN must be blocked when allowLAN=false, got %d", w.Code)
	}
}

func TestDualPortIsolationAndHealthCheck(t *testing.T) {
	dir := t.TempDir()
	p, err := newProxy(dir, filepath.Join(dir, "certs"), "127.0.0.1:18771", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(p, &logRing{})
	a.setAdminAddr("127.0.0.1:18772")
	a.sysFn = func() sysState { return sysState{CertTrusted: true, ProxyEnabled: true} }

	pool := x509.NewCertPool()
	pool.AddCert(p.ca)
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	up.TLS = &tls.Config{GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) { return p.leaf(h.ServerName) }}
	up.StartTLS()
	t.Cleanup(up.Close)

	origDial := p.dial
	p.dial = func(ctx context.Context, n, addr string) (net.Conn, error) {
		if strings.Contains(addr, "akamaized.net") {
			return (&net.Dialer{}).DialContext(ctx, n, up.Listener.Addr().String())
		}
		return origDial(ctx, n, addr)
	}
	// pingUpstream 使用独立 Transport，注入 Fake RootCA 需要默认证书可验证或走系统；改用 direct dial 并跳过仅在 test 中的 CDN TLS 校验是不必要的，但 pingUpstream 会走 p.dial -> up：
	// 注意 pingUpstream 未设 RootCAs，此处主要验证 6 维度条目结构完整性，不强制要求 cdn 状态为 ok。

	// 1. 数据端口严格屏蔽 /api/*，且本机访问 / 自动 302 跳转管理端口
	rAPI := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rAPI.Host = "127.0.0.1:18771"
	wAPI := httptest.NewRecorder()
	p.ServeHTTP(wAPI, rAPI)
	if wAPI.Code != http.StatusForbidden {
		t.Fatalf("data port must forbid /api/status, got %d", wAPI.Code)
	}

	rRoot := httptest.NewRequest(http.MethodGet, "/", nil)
	rRoot.Host = "127.0.0.1:18771"
	wRoot := httptest.NewRecorder()
	p.ServeHTTP(wRoot, rRoot)
	if wRoot.Code != http.StatusFound || wRoot.Header().Get("Location") != "http://127.0.0.1:18772/" {
		t.Fatalf("data port loopback / must redirect to admin port, got %d (%s)", wRoot.Code, wRoot.Header().Get("Location"))
	}

	// 2. 管理端口正常提供 UI、/api/status（含 dualPort + tlsSessionCache）及 6 维系统体检
	admin := a.adminHandler()
	rUI := httptest.NewRequest(http.MethodGet, "/", nil)
	rUI.Host = "127.0.0.1:18772"
	wUI := httptest.NewRecorder()
	admin.ServeHTTP(wUI, rUI)
	if wUI.Code != 200 || !strings.Contains(wUI.Body.String(), panelTitle) {
		t.Fatalf("admin UI failed: %d", wUI.Code)
	}

	rSt := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	rSt.Host = "127.0.0.1:18772"
	wSt := httptest.NewRecorder()
	admin.ServeHTTP(wSt, rSt)
	var st map[string]any
	if err := json.Unmarshal(wSt.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st["dualPort"] != true || st["tlsSessionCache"] != true || st["adminAddr"] != "127.0.0.1:18772" {
		t.Fatalf("unexpected dual-port status fields: %v", st)
	}

	rHC := httptest.NewRequest(http.MethodPost, "/api/health-check", nil)
	rHC.Host = "127.0.0.1:18772"
	rHC.Header.Set("X-GBF-Dock", "1")
	wHC := httptest.NewRecorder()
	admin.ServeHTTP(wHC, rHC)
	var rep healthReport
	if wHC.Code != 200 || json.Unmarshal(wHC.Body.Bytes(), &rep) != nil || len(rep.Items) != 6 {
		t.Fatalf("health-check failed: %d %s", wHC.Code, wHC.Body.String())
	}
	_ = pool
}

func TestCacheBreakdownAndCategoryClear(t *testing.T) {
	dir := t.TempDir()
	p, err := newProxy(dir, filepath.Join(dir, "certs"), "127.0.0.1:18773", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	newApp(p, &logRing{})

	entries := []struct {
		url  string
		ct   string
		body []byte
	}{
		{"https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/a.png", "image/png", append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("1"), 64)...)},
		{"https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/sound/bgm.mp3", "audio/mpeg", bytes.Repeat([]byte("MP3"), 64)},
		{"https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/js/app.js", "application/javascript", []byte("console.log('ok');")},
		{"https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/data/cfg.json", "application/json", []byte(`{"ok":true}`)},
	}
	for _, e := range entries {
		fp := p.cachePath(e.url)
		_ = os.MkdirAll(filepath.Dir(fp), 0o755)
		mj, _ := json.Marshal(entryMeta{URL: e.url, Header: http.Header{"Content-Type": {e.ct}}})
		if err := os.WriteFile(fp, append(append(mj, '\n'), e.body...), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	rBD := httptest.NewRequest(http.MethodGet, "/api/cache-breakdown", nil)
	rBD.Host = "127.0.0.1:18773"
	wBD := httptest.NewRecorder()
	p.ServeHTTP(wBD, rBD)
	var bd cacheBreakdownReport
	if wBD.Code != 200 || json.Unmarshal(wBD.Body.Bytes(), &bd) != nil {
		t.Fatalf("cache-breakdown failed: %d %s", wBD.Code, wBD.Body.String())
	}
	if bd.TotalFiles != 4 || len(bd.Categories) != 5 {
		t.Fatalf("unexpected breakdown totals: files=%d cats=%d", bd.TotalFiles, len(bd.Categories))
	}

	rClr := httptest.NewRequest(http.MethodPost, "/api/cache-clear-category", strings.NewReader(`{"category":"audio"}`))
	rClr.Host = "127.0.0.1:18773"
	rClr.Header.Set("X-GBF-Dock", "1")
	wClr := httptest.NewRecorder()
	p.ServeHTTP(wClr, rClr)
	if wClr.Code != 200 || !strings.Contains(wClr.Body.String(), "音频与语音") {
		t.Fatalf("clear audio category failed: %d %s", wClr.Code, wClr.Body.String())
	}

	after := p.cacheBreakdown()
	if after.TotalFiles != 3 {
		t.Fatalf("after clearing audio want 3 files left, got %d", after.TotalFiles)
	}
}

func TestEOFRetryAndCrossShardDedup(t *testing.T) {
	dir := t.TempDir()
	p, err := newProxy(dir, filepath.Join(dir, "certs"), "127.0.0.1:0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(p, &logRing{})
	pool := x509.NewCertPool()
	pool.AddCert(p.ca)

	validPNG := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("RETRYDATA"), 64)...)
	var upCalls atomic.Int64
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := upCalls.Add(1)
		if c == 1 {
			// 第 1 次立即断开 TCP 连接模拟上游瞬时 EOF
			if conn, _, hErr := http.NewResponseController(w).Hijack(); hErr == nil {
				_ = conn.Close()
				return
			}
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Etag", `"hero-etag-1"`)
		_, _ = w.Write(validPNG)
	}))
	up.TLS = &tls.Config{GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) { return p.leaf(h.ServerName) }}
	up.StartTLS()
	t.Cleanup(up.Close)

	p.tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	p.dial = func(ctx context.Context, n, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, n, up.Listener.Addr().String())
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	frontURL, _ := url.Parse(front.URL)

	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(frontURL), TLSClientConfig: &tls.Config{RootCAs: pool}, ForceAttemptHTTP2: true,
	}}

	// 1. 首次请求遭遇 EOF，代理静默重试一次直接成功返回 200 并写盘
	uA1 := "https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/sp/hero.png?v=1"
	resp, body := get(t, client, uA1)
	if resp.StatusCode != 200 || !bytes.Equal(body, validPNG) || a.p.errors.Load() != 0 || upCalls.Load() != 2 {
		t.Fatalf("EOF retry failed: status=%d len=%d errs=%d calls=%d", resp.StatusCode, len(body), a.p.errors.Load(), upCalls.Load())
	}

	// 2. 不同 CDN 分片 a4 请求同一资源：跨分片去重直接本地命中（0 次上游请求）
	uA4 := "https://prd-game-a4-granbluefantasy.akamaized.net/assets_en/img/sp/hero.png?v=1"
	resp2, body2 := get(t, client, uA4)
	if resp2.StatusCode != 200 || !bytes.Equal(body2, validPNG) || upCalls.Load() != 2 || a.p.hits.Load() != 1 {
		t.Fatalf("cross-shard hit failed: status=%d calls=%d hits=%d", resp2.StatusCode, upCalls.Load(), a.p.hits.Load())
	}

	// 3. 条件请求触发零磁盘 304 并记录 hits304
	r304, _ := get(t, client, uA4, "If-None-Match", `"hero-etag-1"`)
	if r304.StatusCode != 304 || upCalls.Load() != 2 || a.p.hits304.Load() != 1 {
		t.Fatalf("304 counter failed: status=%d calls=%d hits304=%d", r304.StatusCode, upCalls.Load(), a.p.hits304.Load())
	}
}

func TestCacheImportNativeAndRawCDNLooseQuery(t *testing.T) {
	dir := t.TempDir()
	cdn := &fakeCDN{version: "v1"}
	client, a, _ := rig(t, dir, time.Hour, cdn)
	p := a.p

	// 1. 禁止自我导入
	if _, err := p.previewImportCache(p.getCacheRoot(), "newer"); err == nil {
		t.Fatal("self-import must be rejected")
	}

	// 2. 准备原始 CDN 目录（含 1 个有效 PNG + 1 个 HTML 坏 PNG）
	rawSrc := t.TempDir()
	goodPath := filepath.Join(rawSrc, "prd-game-a5-granbluefantasy.akamaized.net", "assets_en", "img", "sp", "raw.png")
	badPath := filepath.Join(rawSrc, "prd-game-a5-granbluefantasy.akamaized.net", "assets_en", "img", "sp", "bad.png")
	_ = os.MkdirAll(filepath.Dir(goodPath), 0o755)
	rawPNG := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("RAWCDN"), 40)...)
	_ = os.WriteFile(goodPath, rawPNG, 0o644)
	_ = os.WriteFile(badPath, []byte("<html>502 Bad Gateway</html>"), 0o644)

	prev, err := p.previewImportCache(rawSrc, "newer")
	if err != nil {
		t.Fatalf("previewImportCache(raw): %v", err)
	}
	if prev.ValidFiles != 1 || prev.InvalidFiles != 1 || prev.NewFiles != 1 || !strings.Contains(prev.Format, "原始 CDN") {
		t.Fatalf("unexpected raw preview: %+v", prev)
	}

	// 3. 通过异步 API 执行并入并轮询 /api/cache-import-progress
	bodyBytes, _ := json.Marshal(map[string]any{"path": rawSrc, "strategy": "newer", "async": true})
	rMerge := httptest.NewRequest(http.MethodPost, "/api/cache-import-merge", bytes.NewReader(bodyBytes))
	rMerge.Host = "127.0.0.1:0"
	rMerge.Header.Set("X-GBF-Dock", "1")
	wMerge := httptest.NewRecorder()
	p.ServeHTTP(wMerge, rMerge)
	if wMerge.Code != 200 || !strings.Contains(wMerge.Body.String(), `"started":true`) {
		t.Fatalf("async merge start failed: %d %s", wMerge.Code, wMerge.Body.String())
	}

	waitFor(t, func() bool {
		prog := a.getImportProgress()
		return !prog.Running && prog.Done
	})
	prog := a.getImportProgress()
	if prog.Added != 1 || prog.Invalid != 1 {
		t.Fatalf("unexpected async import result: %+v", prog)
	}

	// 4. 请求带任意 ?v=999 且跨分片 a3 的 raw.png：应兼容查询串并直接命中（0 次上游请求）
	_, gotRaw := get(t, client, "https://prd-game-a3-granbluefantasy.akamaized.net/assets_en/img/sp/raw.png?v=999")
	if !bytes.Equal(gotRaw, rawPNG) || cdn.count() != 0 {
		t.Fatalf("imported raw CDN with LooseQuery should hit locally: len=%d cdnCalls=%d", len(gotRaw), cdn.count())
	}

	// 5. 原生格式导入（LooseQuery=false）：版本不同仍须向 CDN 验证
	nativeSrc := t.TempDir()
	nativeURL := "https://prd-game-a2-granbluefantasy.akamaized.net/assets_en/img/sp/native.png?v=10"
	sumPath := p.cachePath(nativeURL)
	relBucket, _ := filepath.Rel(p.getCacheRoot(), sumPath)
	nativeFile := filepath.Join(nativeSrc, relBucket)
	_ = os.MkdirAll(filepath.Dir(nativeFile), 0o755)
	mj, _ := json.Marshal(entryMeta{URL: nativeURL, Header: http.Header{"Content-Type": {"image/png"}}})
	_ = os.WriteFile(nativeFile, append(append(mj, '\n'), rawPNG...), 0o644)

	mRes, mErr := p.mergeCacheFrom(context.Background(), nativeSrc, "newer", nil)
	if mErr != nil || mRes.Added != 1 {
		t.Fatalf("native merge failed: res=%+v err=%v", mRes, mErr)
	}
	// v=10 直接命中
	if _, gotN10 := get(t, client, nativeURL); !bytes.Equal(gotN10, rawPNG) || cdn.count() != 0 {
		t.Fatalf("native v=10 must hit locally, cdnCalls=%d", cdn.count())
	}
	// v=11 不可错用 v=10 缓存
	if _, gotN11 := get(t, client, "https://prd-game-a2-granbluefantasy.akamaized.net/assets_en/img/sp/native.png?v=11"); !bytes.Equal(gotN11, cdn.body("v=11")) || cdn.count() != 1 {
		t.Fatalf("native v=11 must fetch upstream instead of matching v=10, cdnCalls=%d", cdn.count())
	}
}

func TestCustomCacheDirSwitch(t *testing.T) {
	dir := t.TempDir()
	cdn := &fakeCDN{version: "v1"}
	client, a, _ := rig(t, dir, time.Hour, cdn)
	p := a.p

	customDir := filepath.Join(t.TempDir(), "my-gbf-dock")
	reqBody, _ := json.Marshal(map[string]string{"path": customDir})
	rSet := httptest.NewRequest(http.MethodPost, "/api/set-cache-dir", bytes.NewReader(reqBody))
	rSet.Host = "127.0.0.1:0"
	rSet.Header.Set("X-GBF-Dock", "1")
	wSet := httptest.NewRecorder()
	p.ServeHTTP(wSet, rSet)
	if wSet.Code != 200 || !strings.Contains(wSet.Body.String(), "已切换缓存存储目录") {
		t.Fatalf("set-cache-dir failed: %d %s", wSet.Code, wSet.Body.String())
	}
	if !sameCleanPath(p.getCacheRoot(), customDir) {
		t.Fatalf("cacheRoot = %s, want %s", p.getCacheRoot(), customDir)
	}

	// 新拉取的资源必须落入自定义目录而非默认目录
	get(t, client, assetURL)
	if n := countFiles(t, dir); n != 0 {
		t.Fatalf("default cache dir should have 0 files, got %d", n)
	}
	customFiles := 0
	_ = filepath.WalkDir(customDir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !isInternalCacheFile(d.Name()) {
			customFiles++
		}
		return nil
	})
	if customFiles != 1 {
		t.Fatalf("custom cache dir want 1 file, got %d", customFiles)
	}

	// 恢复默认目录
	rReset := httptest.NewRequest(http.MethodPost, "/api/set-cache-dir", strings.NewReader(`{"path":""}`))
	rReset.Host = "127.0.0.1:0"
	rReset.Header.Set("X-GBF-Dock", "1")
	wReset := httptest.NewRecorder()
	p.ServeHTTP(wReset, rReset)
	if wReset.Code != 200 || !sameCleanPath(p.getCacheRoot(), p.defaultCacheRoot()) {
		t.Fatalf("reset cache dir failed: %d %s (cur=%s)", wReset.Code, wReset.Body.String(), p.getCacheRoot())
	}
}

func TestImageMagicCrossTolerance(t *testing.T) {
	dir := t.TempDir()
	p, err := newProxy(dir, filepath.Join(dir, "certs"), "127.0.0.1:0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	newApp(p, &logRing{})
	pool := x509.NewCertPool()
	pool.AddCert(p.ca)

	jpegBody := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte("J"), 64)...)
	webpBody := append([]byte("RIFF\x24\x00\x00\x00WEBP"), bytes.Repeat([]byte("W"), 64)...)
	gifBody := append([]byte("GIF89a"), bytes.Repeat([]byte("G"), 64)...)
	pngBody := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte("P"), 64)...)

	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/assets_en/img/jpeg_as_png.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(jpegBody)
		case "/assets_en/img/webp_as_png.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(webpBody)
		case "/assets_en/img/gif_as_jpg.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(gifBody)
		case "/assets_en/img/png_as_jpg.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(pngBody)
		case "/assets_en/img/fake_html.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = io.WriteString(w, "<html><body>502 Bad Gateway</body></html>")
		case "/assets_en/img/garbage.png":
			w.Header().Set("Content-Type", "image/png")
			_, _ = io.WriteString(w, "NOT_AN_IMAGE_AT_ALL_123456")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	up.TLS = &tls.Config{GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) { return p.leaf(h.ServerName) }}
	up.StartTLS()
	t.Cleanup(up.Close)

	p.tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	p.dial = func(ctx context.Context, n, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, n, up.Listener.Addr().String())
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	frontURL, _ := url.Parse(front.URL)
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(frontURL), TLSClientConfig: &tls.Config{RootCAs: pool},
	}}

	validCases := []struct {
		url  string
		want []byte
	}{
		{"https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/jpeg_as_png.png", jpegBody},
		{"https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/webp_as_png.png", webpBody},
		{"https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/gif_as_jpg.jpg", gifBody},
		{"https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/png_as_jpg.jpg", pngBody},
	}
	for _, tc := range validCases {
		resp, b := get(t, client, tc.url)
		if resp.StatusCode != 200 || !bytes.Equal(b, tc.want) {
			t.Fatalf("%s mismatch: status=%d len=%d", tc.url, resp.StatusCode, len(b))
		}
	}
	for _, badURL := range []string{
		"https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/fake_html.png",
		"https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/garbage.png",
	} {
		_, _ = get(t, client, badURL)
	}
	if n := countFiles(t, dir); n != len(validCases) {
		t.Fatalf("want %d cached image files (cross-format allowed, html/garbage rejected), got %d", len(validCases), n)
	}
}

func TestRAMSLRUCacheAndEviction(t *testing.T) {
	// 1. 验证 SLRU 分段淘汰（Probation 25% + Protected 75%）与单文件超限跳过
	cache := newSLRUCache(100, 50)
	cache.Put("k-too-big", "u", http.Header{}, bytes.Repeat([]byte("X"), 51), time.Now(), false)
	if items, _, _ := cache.Stats(); items != 0 {
		t.Fatalf("oversized item must not enter RAM cache, got items=%d", items)
	}

	b30 := bytes.Repeat([]byte("A"), 30)
	b30_2 := bytes.Repeat([]byte("B"), 30)
	b30_3 := bytes.Repeat([]byte("C"), 30)
	b30_4 := bytes.Repeat([]byte("D"), 30)

	cache.Put("hot", "u1", http.Header{}, b30, time.Now(), false)
	// 二次访问晋升到 Protected（protLimit=75, probLimit=25）
	if _, ok := cache.Get("hot"); !ok {
		t.Fatal("hot key must be in probation")
	}
	cache.Put("cold1", "u2", http.Header{}, b30_2, time.Now(), false)
	cache.Put("cold2", "u3", http.Header{}, b30_3, time.Now(), false)
	cache.Put("cold3", "u4", http.Header{}, b30_4, time.Now(), false)

	if _, ok := cache.Get("hot"); !ok {
		t.Fatal("protected hot key should resist probation churn")
	}

	// 2. 端到端验证 RAM 命中 200 / 206 / 304、文件更新即时刷新、清空即时失效
	dir := t.TempDir()
	cdn := &fakeCDN{version: "v1"}
	client, a, _ := rig(t, dir, time.Hour, cdn)
	p := a.p

	// 替换成一个可控大小的测试目标（~4KB < 512KB，准入 RAM）
	u := "https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/sp/ram.png?v=1"
	want := cdn.body("v=1")
	if _, b := get(t, client, u); !bytes.Equal(b, want) {
		t.Fatal("initial miss body mismatch")
	}
	if items, used, _ := p.ram.Stats(); items != 1 || used != int64(len(want)) {
		t.Fatalf("expected item stored in RAM after fetch: items=%d used=%d want=%d", items, used, len(want))
	}

	// 哪怕删掉磁盘文件，也必须从 RAM 直接命中返回 200 / 206 / 304
	if err := os.Remove(p.cachePath(u)); err != nil {
		t.Fatal(err)
	}
	r200, b200 := get(t, client, u)
	if r200.StatusCode != 200 || !bytes.Equal(b200, want) || p.ramHits.Load() != 1 || cdn.count() != 1 {
		t.Fatalf("RAM 200 hit failed: code=%d ramHits=%d cdn=%d", r200.StatusCode, p.ramHits.Load(), cdn.count())
	}
	r206, b206 := get(t, client, u, "Range", "bytes=0-15")
	if r206.StatusCode != 206 || !bytes.Equal(b206, want[:16]) || p.ramHits.Load() != 2 {
		t.Fatalf("RAM 206 range hit failed: code=%d len=%d ramHits=%d", r206.StatusCode, len(b206), p.ramHits.Load())
	}
	r304, _ := get(t, client, u, "If-None-Match", `"v1v=1"`)
	if r304.StatusCode != 304 || p.ramHits.Load() != 3 || p.hits304.Load() != 1 {
		t.Fatalf("RAM 304 hit failed: code=%d ramHits=%d hits304=%d", r304.StatusCode, p.ramHits.Load(), p.hits304.Load())
	}

	// 换新版 URL 覆盖或清空后，RAM 不得返回过期旧版
	u2 := "https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/sp/ram.png?v=2"
	if _, b2 := get(t, client, u2); !bytes.Equal(b2, cdn.body("v=2")) {
		t.Fatal("v=2 overwrite mismatch")
	}
	if _, bCheck := get(t, client, u2); !bytes.Equal(bCheck, cdn.body("v=2")) {
		t.Fatal("RAM must serve updated v=2 body")
	}
	if err := p.clearCache(); err != nil {
		t.Fatal(err)
	}
	if items, _, _ := p.ram.Stats(); items != 0 {
		t.Fatalf("clearCache must empty RAM cache, got items=%d", items)
	}
}

func TestPersistentMetaIndexLifecycle(t *testing.T) {
	dir := t.TempDir()
	cdn := &fakeCDN{version: "v1"}
	client, a, _ := rig(t, dir, time.Hour, cdn)
	p := a.p

	u1 := "https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/sp/idx1.png?v=1"
	u2 := "https://prd-game-a2-granbluefantasy.akamaized.net/assets_en/js/idx2.js?v=1"
	get(t, client, u1)
	get(t, client, u2)
	wantFiles := p.cachedFiles.Load()
	wantBytes := p.cachedBytes.Load()
	if wantFiles != 2 || wantBytes <= 0 {
		t.Fatalf("unexpected cached stats: files=%d bytes=%d", wantFiles, wantBytes)
	}

	p.flushMetaIndex()
	indexPath := filepath.Join(p.getCacheRoot(), metaIndexFileName)
	if !fileExists(indexPath) {
		t.Fatalf("expected %s to be written on flush", indexPath)
	}

	// 验证所有目录统计/扫描工具均忽略 .meta_index.v1
	if cnt := countFiles(t, dir); cnt != 2 {
		t.Fatalf("countFiles must ignore .meta_index.v1, got %d", cnt)
	}
	if bd := p.cacheBreakdown(); bd.TotalFiles != 2 {
		t.Fatalf("cacheBreakdown must ignore .meta_index.v1, got %d", bd.TotalFiles)
	}
	if checked, _, _ := p.scanCacheHealth(true); checked != 2 {
		t.Fatalf("scanCacheHealth must ignore .meta_index.v1, got %d", checked)
	}

	// 新建 proxy 实例模拟重启：应直接从 .meta_index.v1 秒级加载恢复，且零磁盘打开完成 304 命中
	p2, err := newProxy(dir, filepath.Join(dir, "certs"), "127.0.0.1:0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !p2.metaIdxReady.Load() || p2.cachedFiles.Load() != wantFiles || p2.cachedBytes.Load() != wantBytes {
		t.Fatalf("restarted proxy failed loading meta index: ready=%v files=%d/%d bytes=%d/%d",
			p2.metaIdxReady.Load(), p2.cachedFiles.Load(), wantFiles, p2.cachedBytes.Load(), wantBytes)
	}
	if _, ok := p2.metaIdx.Load(p2.cachePath(u1)); !ok {
		t.Fatal("restarted proxy missing metaIdx entry for u1")
	}

	// 破坏 .meta_index.v1 的最后几个字节（损坏 CRC32）：重启应安全回退扫描而不崩溃或报错
	rawIdx, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	rawIdx[len(rawIdx)-1] ^= 0xFF
	if err := os.WriteFile(indexPath, rawIdx, 0o644); err != nil {
		t.Fatal(err)
	}

	p3, err := newProxy(dir, filepath.Join(dir, "certs"), "127.0.0.1:0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return p3.metaIdxReady.Load() && p3.cachedFiles.Load() == 2 })
}

func TestDeadShardAutoFallback(t *testing.T) {
	dir := t.TempDir()
	p, err := newProxy(dir, filepath.Join(dir, "certs"), "127.0.0.1:0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(p, &logRing{})
	pool := x509.NewCertPool()
	pool.AddCert(p.ca)

	mp3Data := append([]byte("ID3\x03\x00\x00\x00\x00\x00\x00"), bytes.Repeat([]byte("WIN_SE_AUDIO_"), 256)...)
	steamMP3 := append([]byte("ID3\x03\x00\x00\x00\x00\x00\x00"), bytes.Repeat([]byte("STEAM_AUDIO_"), 128)...)

	var deadHits atomic.Int64
	var liveHits atomic.Int64
	var steamLiveHits atomic.Int64

	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Host {
		case "prd-game-a5-granbluefantasy.akamaized.net", "prd-game-a5-granbluefantasy-steam.akamaized.net":
			deadHits.Add(1)
			if conn, _, hErr := http.NewResponseController(w).Hijack(); hErr == nil {
				_ = conn.Close()
				return
			}
			http.Error(w, "dead", http.StatusBadGateway)
		case "prd-game-a-granbluefantasy.akamaized.net":
			liveHits.Add(1)
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write(mp3Data)
		case "prd-game-a-granbluefantasy-steam.akamaized.net":
			steamLiveHits.Add(1)
			w.Header().Set("Content-Type", "audio/mpeg")
			_, _ = w.Write(steamMP3)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	up.TLS = &tls.Config{GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) { return p.leaf(h.ServerName) }}
	up.StartTLS()
	t.Cleanup(up.Close)

	p.tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	p.dial = func(ctx context.Context, n, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, n, up.Listener.Addr().String())
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	frontURL, _ := url.Parse(front.URL)

	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(frontURL), TLSClientConfig: &tls.Config{RootCAs: pool}, ForceAttemptHTTP2: true,
	}}

	// 1. 复现 Range 请求已下线 a5 分片 win_se_1.mp3：应自动回退主域名 a 并写入缓存，返回 206 且 errors == 0
	uA5 := "https://prd-game-a5-granbluefantasy.akamaized.net/assets/sound/se/win_se_1.mp3"
	r1, b1 := get(t, client, uA5, "Range", "bytes=0-9")
	if r1.StatusCode != 206 || !bytes.Equal(b1, mp3Data[:10]) || a.p.errors.Load() != 0 {
		t.Fatalf("a5 Range fallback failed: status=%d body=%q errs=%d", r1.StatusCode, b1, a.p.errors.Load())
	}
	if deadHits.Load() != 1 || liveHits.Load() != 1 {
		t.Fatalf("expected 1 dead attempt then 1 live fallback, got dead=%d live=%d", deadHits.Load(), liveHits.Load())
	}

	// 再次请求（无论是 a5 还是主域名 a）均直接命中本地缓存
	if _, bHit := get(t, client, "https://prd-game-a-granbluefantasy.akamaized.net/assets/sound/se/win_se_1.mp3"); !bytes.Equal(bHit, mp3Data) || liveHits.Load() != 1 {
		t.Fatalf("canonical hit after fallback failed: live=%d", liveHits.Load())
	}

	// 2. 坏分片记忆：后续再请求 a5 新文件，应跳过 a5 直接向主域名 prd-game-a 拉取（deadHits 不再增加）
	uA5Two := "https://prd-game-a5-granbluefantasy.akamaized.net/assets/sound/se/bgm2.mp3"
	r2, b2 := get(t, client, uA5Two)
	if r2.StatusCode != 200 || !bytes.Equal(b2, mp3Data) || deadHits.Load() != 1 || liveHits.Load() != 2 {
		t.Fatalf("dead-shard memory skip failed: status=%d dead=%d live=%d", r2.StatusCode, deadHits.Load(), liveHits.Load())
	}

	// 3. Steam 版坏分片仅回退至 Steam 主域名（不串普通版域名）
	uSteamA5 := "https://prd-game-a5-granbluefantasy-steam.akamaized.net/assets/sound/se/s.mp3"
	r3, b3 := get(t, client, uSteamA5)
	if r3.StatusCode != 200 || !bytes.Equal(b3, steamMP3) || steamLiveHits.Load() != 1 || liveHits.Load() != 2 {
		t.Fatalf("steam dead-shard fallback isolation failed: status=%d steamLive=%d live=%d", r3.StatusCode, steamLiveHits.Load(), liveHits.Load())
	}
}

func TestLANProxySSRFBlocked(t *testing.T) {
	dir := t.TempDir()
	p, err := newProxy(dir, filepath.Join(dir, "certs"), "127.0.0.1:18781", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(p, &logRing{})

	adminLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer adminLn.Close()
	adminAddr := adminLn.Addr().String()
	a.setAdminAddr(adminAddr)
	a.serveAdminListener(adminLn)

	p.allowLAN.Store(true)

	// 本机直接访问管理端口应返回 200
	respLocal, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + adminAddr + "/api/status")
	if err != nil || respLocal.StatusCode != 200 {
		t.Fatalf("direct local admin access failed: err=%v resp=%v", err, respLocal)
	}
	_ = respLocal.Body.Close()

	_, adminPort, _ := net.SplitHostPort(adminAddr)

	// 1. 局域网客户端通过数据面发起绝对 URI HTTP 代理请求访问回环/管理端口必须全部 403
	httpTargets := []string{
		"http://" + adminAddr + "/api/status",
		"http://localhost:" + adminPort + "/api/status",
		"http://sub.localhost:" + adminPort + "/api/status",
		"http://127.1:" + adminPort + "/api/status",
		"http://2130706433:" + adminPort + "/api/status",
		"http://[::1]:" + adminPort + "/api/status",
	}
	for _, targetURL := range httpTargets {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			path := "/api/status"
			if method == http.MethodPost {
				path = "/api/cache-clear"
			}
			u := strings.Replace(targetURL, "/api/status", path, 1)
			r := httptest.NewRequest(method, u, nil)
			r.RemoteAddr = "192.168.1.50:54321"
			if method == http.MethodPost {
				r.Header.Set("X-GBF-Dock", "1")
			}
			w := httptest.NewRecorder()
			p.ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("LAN HTTP proxy SSRF %s %s must return 403, got %d", method, u, w.Code)
			}
		}
	}

	// 2. 局域网客户端通过数据面发起 CONNECT 隧道访问回环/管理端口必须全部 403
	connectTargets := []string{
		adminAddr,
		"localhost:" + adminPort,
		"foo.localhost:" + adminPort,
		"[::1]:" + adminPort,
		"127.1:" + adminPort,
	}
	for _, ct := range connectTargets {
		r := httptest.NewRequest(http.MethodConnect, "http://"+ct, nil)
		r.Host = ct
		r.RemoteAddr = "192.168.1.50:54321"
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("LAN CONNECT SSRF %s must return 403, got %d", ct, w.Code)
		}
	}
}

func TestTTLRevalidationNotBlockedByTouchWarm(t *testing.T) {
	dir := t.TempDir()
	cdn := &fakeCDN{version: "v1"}
	client, a, _ := rig(t, dir, 150*time.Millisecond, cdn)
	p := a.p

	u := "https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/sp/ttl_banner.png"
	_, body := get(t, client, u)
	if len(body) == 0 || cdn.count() != 1 {
		t.Fatalf("initial fetch unexpected: len=%d calls=%d", len(body), cdn.count())
	}

	fp := p.cachePath(u)
	stBefore, err := os.Stat(fp)
	if err != nil {
		t.Fatal(err)
	}
	origMod := stBefore.ModTime()

	// 模拟 13 小时前访问：touchWarm 只能更新 lastAccess，绝不能刷新 modTime 或磁盘 mtime
	oldAccess := time.Now().Add(-13 * time.Hour)
	v, ok := p.metaIdx.Load(fp)
	if !ok {
		t.Fatal("missing metaIdx entry")
	}
	cmOld := v.(*cachedMeta)
	p.metaIdx.Store(fp, &cachedMeta{
		url:        cmOld.url,
		header:     cmOld.header,
		headerLen:  cmOld.headerLen,
		bodySize:   cmOld.bodySize,
		modTime:    origMod,
		lastAccess: oldAccess,
	})
	p.touchWarm(fp, nil)

	stAfter, err := os.Stat(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !stAfter.ModTime().Equal(origMod) {
		t.Fatalf("touchWarm mutated disk mtime: before=%v after=%v", origMod, stAfter.ModTime())
	}
	v2, _ := p.metaIdx.Load(fp)
	cmAfter := v2.(*cachedMeta)
	if !cmAfter.modTime.Equal(origMod) {
		t.Fatalf("touchWarm mutated metaIdx modTime: before=%v after=%v", origMod, cmAfter.modTime)
	}
	if !cmAfter.lastAccess.After(oldAccess) {
		t.Fatalf("touchWarm did not advance lastAccess: old=%v new=%v", oldAccess, cmAfter.lastAccess)
	}

	// 等待超过 TTL（150ms），再次命中必须触发后台 revalidate 且带上 If-None-Match
	time.Sleep(220 * time.Millisecond)
	_, _ = get(t, client, u)
	waitFor(t, func() bool { return cdn.count() >= 2 && p.revalidated.Load() >= 1 })
	if !strings.Contains(cdn.req(1), `"v1"`) {
		t.Fatalf("expected conditional revalidate with ETag, got %q", cdn.req(1))
	}

	// 验证 pruneColdCache 以 max(mtime, lastAccess) 判断冷热：
	// 磁盘 mtime 虽然很旧，但只要 lastAccess 刚刷新过，就不应被当成冷门误删；两者都旧才删除。
	recentAccessFile := p.cachePath("https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/sp/warm.png")
	coldFile := p.cachePath("https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/sp/cold.png")
	for _, item := range []struct {
		fp     string
		url    string
		mtime  time.Time
		access time.Time
	}{
		{recentAccessFile, "https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/sp/warm.png", time.Now().Add(-2 * time.Hour), time.Now()},
		{coldFile, "https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/sp/cold.png", time.Now().Add(-2 * time.Hour), time.Now().Add(-2 * time.Hour)},
	} {
		_ = os.MkdirAll(filepath.Dir(item.fp), 0o755)
		mj, _ := json.Marshal(entryMeta{URL: item.url, Header: http.Header{"Content-Type": {"image/png"}}})
		line := append(mj, '\n')
		content := append(line, body[:64]...)
		if err := os.WriteFile(item.fp, content, 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(item.fp, item.mtime, item.mtime)
		p.metaIdx.Store(item.fp, &cachedMeta{
			url:        item.url,
			header:     http.Header{"Content-Type": {"image/png"}},
			headerLen:  int64(len(line)),
			bodySize:   64,
			modTime:    item.mtime,
			lastAccess: item.access,
		})
	}
	removed, _, pErr := p.pruneColdCache(30 * time.Minute)
	if pErr != nil {
		t.Fatal(pErr)
	}
	if removed != 1 || !fileExists(recentAccessFile) || fileExists(coldFile) {
		t.Fatalf("pruneColdCache unexpected: removed=%d warmExists=%v coldExists=%v", removed, fileExists(recentAccessFile), fileExists(coldFile))
	}
}

func TestMetaIndexV2PersistenceAndV1Upgrade(t *testing.T) {
	dir := t.TempDir()
	p, err := newProxy(dir, filepath.Join(dir, "certs"), "127.0.0.1:0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	u := "https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/sp/idx_v2.png?v=1"
	fp := p.cachePath(u)
	modT := time.Unix(1700000000, 123456000)
	accT := time.Unix(1700086400, 654321000)
	p.metaIdx.Store(fp, &cachedMeta{
		url:        u,
		header:     http.Header{"Content-Type": {"image/png"}},
		headerLen:  64,
		bodySize:   1024,
		modTime:    modT,
		lastAccess: accT,
	})
	if err := p.saveMetaIndex(); err != nil {
		t.Fatal(err)
	}

	p2, err := newProxy(dir, filepath.Join(dir, "certs"), "127.0.0.1:0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := p2.metaIdx.Load(fp)
	if !ok {
		t.Fatal("v2 index missing entry after reload")
	}
	cm := v.(*cachedMeta)
	if !cm.modTime.Equal(modT) || !cm.lastAccess.Equal(accT) {
		t.Fatalf("v2 timestamps mismatch: got mod=%v acc=%v, want mod=%v acc=%v", cm.modTime, cm.lastAccess, modT, accT)
	}

	// 兼容读取 v1 格式索引（25 字节固定头）
	var v1Buf bytes.Buffer
	v1Buf.WriteString(metaIndexMagic)
	var b4 [4]byte
	binary.LittleEndian.PutUint32(b4[:], 1)
	v1Buf.Write(b4[:])
	binary.LittleEndian.PutUint32(b4[:], 1)
	v1Buf.Write(b4[:])
	rel, _ := filepath.Rel(p.getCacheRoot(), fp)
	var tmp []byte
	tmp, _ = appendLenBytes(tmp, filepath.ToSlash(rel))
	tmp, _ = appendLenBytes(tmp, u)
	var fixed25 [25]byte
	binary.LittleEndian.PutUint64(fixed25[0:8], 64)
	binary.LittleEndian.PutUint64(fixed25[8:16], 1024)
	binary.LittleEndian.PutUint64(fixed25[16:24], uint64(modT.UnixNano()))
	tmp = append(tmp, fixed25[:]...)
	var kc [2]byte
	binary.LittleEndian.PutUint16(kc[:], 0)
	tmp = append(tmp, kc[:]...)
	v1Buf.Write(tmp)
	binary.LittleEndian.PutUint32(b4[:], crc32.ChecksumIEEE(v1Buf.Bytes()))
	v1Buf.Write(b4[:])
	if err := os.WriteFile(filepath.Join(p.getCacheRoot(), metaIndexFileName), v1Buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	p3, err := newProxy(dir, filepath.Join(dir, "certs"), "127.0.0.1:0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	v3, ok := p3.metaIdx.Load(fp)
	if !ok {
		t.Fatal("v1 index compatibility load failed")
	}
	cm3 := v3.(*cachedMeta)
	if !cm3.modTime.Equal(modT) || !cm3.lastAccess.Equal(modT) {
		t.Fatalf("v1 upgrade timestamps mismatch: mod=%v acc=%v", cm3.modTime, cm3.lastAccess)
	}

	// 2000 条 saveMetaIndex 纯字符串路径性能回归（应远低于 500ms）
	for i := 0; i < 2000; i++ {
		su := fmt.Sprintf("https://prd-game-a-granbluefantasy.akamaized.net/assets_en/img/bench_%d.png", i)
		p3.metaIdx.Store(p3.cachePath(su), &cachedMeta{
			url:        su,
			header:     http.Header{"Content-Type": {"image/png"}},
			headerLen:  64,
			bodySize:   256,
			modTime:    modT,
			lastAccess: accT,
		})
	}
	t0 := time.Now()
	if err := p3.saveMetaIndex(); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(t0); elapsed > 500*time.Millisecond {
		t.Fatalf("saveMetaIndex(2000) took too long: %v", elapsed)
	}
}

func TestOpenRangeZeroStreamThroughAndPassthrough(t *testing.T) {
	dir := t.TempDir()
	p, err := newProxy(dir, filepath.Join(dir, "certs"), "127.0.0.1:0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(p, &logRing{})
	pool := x509.NewCertPool()
	pool.AddCert(p.ca)

	part1 := append([]byte("ID3\x03\x00\x00\x00\x00\x00\x00"), bytes.Repeat([]byte("FIRST_CHUNK_"), 16)...)
	part2 := bytes.Repeat([]byte("SECOND_CHUNK_"), 128)
	fullBody := append(append([]byte(nil), part1...), part2...)
	var calls atomic.Int64

	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/assets/sound/bgm/stream.mp3":
			calls.Add(1)
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Header().Set("Content-Length", strconv.Itoa(len(fullBody)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(part1)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			time.Sleep(250 * time.Millisecond)
			_, _ = w.Write(part2)
		case "/assets/sound/bgm/chunked.mp3":
			calls.Add(1)
			w.Header().Set("Content-Type", "audio/mpeg")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(part1)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			_, _ = w.Write(part2)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	up.TLS = &tls.Config{GetCertificate: func(h *tls.ClientHelloInfo) (*tls.Certificate, error) { return p.leaf(h.ServerName) }}
	up.StartTLS()
	t.Cleanup(up.Close)

	p.tr.TLSClientConfig = &tls.Config{RootCAs: pool}
	p.dial = func(ctx context.Context, n, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, n, up.Listener.Addr().String())
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	frontURL, _ := url.Parse(front.URL)
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(frontURL), TLSClientConfig: &tls.Config{RootCAs: pool},
	}}

	// 1. 带 Range: bytes=0- 且已知长度时，首字节必须在 250ms 睡醒前到达，且返回 206 + Content-Range
	tStart := time.Now()
	req, _ := http.NewRequest(http.MethodGet, "https://prd-game-a-granbluefantasy.akamaized.net/assets/sound/bgm/stream.mp3", nil)
	req.Header.Set("Range", "bytes=0-")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	firstByte := make([]byte, 1)
	if _, rErr := io.ReadFull(resp.Body, firstByte); rErr != nil {
		t.Fatal(rErr)
	}
	ttfb := time.Since(tStart)
	rest, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	gotBody := append(firstByte, rest...)
	wantRange := fmt.Sprintf("bytes 0-%d/%d", len(fullBody)-1, len(fullBody))
	if resp.StatusCode != http.StatusPartialContent || resp.Header.Get("Content-Range") != wantRange {
		t.Fatalf("unexpected streaming 206 headers: code=%d range=%q want=%q", resp.StatusCode, resp.Header.Get("Content-Range"), wantRange)
	}
	if ttfb > 180*time.Millisecond {
		t.Fatalf("Range: bytes=0- did not stream first byte early enough: ttfb=%v", ttfb)
	}
	if !bytes.Equal(gotBody, fullBody) {
		t.Fatalf("streamed 206 body mismatch: len=%d want=%d", len(gotBody), len(fullBody))
	}

	// 再次请求应直接本地命中，不再打上游
	_, hitBody := get(t, client, "https://prd-game-a-granbluefantasy.akamaized.net/assets/sound/bgm/stream.mp3", "Range", "bytes=0-")
	if !bytes.Equal(hitBody, fullBody) || calls.Load() != 1 {
		t.Fatalf("expected cache hit after Range 0- stream, calls=%d", calls.Load())
	}

	// 2. 上游 chunked 未知长度时，Range: bytes=0- 安全回退落盘后吐 206
	rCh, bCh := get(t, client, "https://prd-game-a-granbluefantasy.akamaized.net/assets/sound/bgm/chunked.mp3", "Range", "bytes=0-")
	if rCh.StatusCode != http.StatusPartialContent || !bytes.Equal(bCh, fullBody) || calls.Load() != 2 {
		t.Fatalf("chunked Range 0- fallback failed: code=%d len=%d calls=%d", rCh.StatusCode, len(bCh), calls.Load())
	}

	// 3. 404 非缓存透传计数 + /api/status 暴露 passthrough 字段
	r404, _ := get(t, client, "https://prd-game-a-granbluefantasy.akamaized.net/assets/missing.png")
	if r404.StatusCode != http.StatusNotFound || p.passthrough.Load() != 1 {
		t.Fatalf("expected 404 passthrough=1, got code=%d passthrough=%d", r404.StatusCode, p.passthrough.Load())
	}
	stReq := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	stReq.Host = "127.0.0.1:0"
	stW := httptest.NewRecorder()
	a.adminHandler().ServeHTTP(stW, stReq)
	var stMap map[string]any
	if json.Unmarshal(stW.Body.Bytes(), &stMap) != nil || stMap["passthrough"] != float64(1) {
		t.Fatalf("status JSON missing passthrough=1: %s", stW.Body.String())
	}
}

func TestStoreDirectRAMPopulation(t *testing.T) {
	dir := t.TempDir()
	cdn := &fakeCDN{version: "v1"}
	client, a, _ := rig(t, dir, time.Hour, cdn)
	p := a.p

	// 带非零 Range 走 p.fetch() -> p.store()：应在不回读磁盘的情况下直接写入 RAM SLRU
	u := "https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/img/sp/direct_ram.png?v=1"
	want := cdn.body("v=1")
	r1, b1 := get(t, client, u, "Range", "bytes=10-29")
	if r1.StatusCode != http.StatusPartialContent || !bytes.Equal(b1, want[10:30]) {
		t.Fatalf("initial range fetch failed: code=%d len=%d", r1.StatusCode, len(b1))
	}
	if items, used, _ := p.ram.Stats(); items != 1 || used != int64(len(want)) {
		t.Fatalf("store() should populate RAM directly: items=%d used=%d want=%d", items, used, len(want))
	}

	// 删掉磁盘文件，后续全量 GET 必须由 RAM 直接命中返回
	if err := os.Remove(p.cachePath(u)); err != nil {
		t.Fatal(err)
	}
	r2, b2 := get(t, client, u)
	if r2.StatusCode != 200 || !bytes.Equal(b2, want) || p.ramHits.Load() != 1 || cdn.count() != 1 {
		t.Fatalf("expected RAM hit after store(): code=%d ramHits=%d cdn=%d", r2.StatusCode, p.ramHits.Load(), cdn.count())
	}
}

func TestLegacyGBFCacheMigration(t *testing.T) {
	home := t.TempDir()
	appData := filepath.Join(home, "AppData")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", appData)
	t.Setenv("AppData", appData)

	// 预先在旧版 %APPDATA%\GBFCache 目录生成证书与配置
	legacyRoot := filepath.Join(appData, legacyConfigDirName)
	legacyCertDir := filepath.Join(legacyRoot, "certs")
	legacyDataDir := t.TempDir()
	oldProxy, err := newProxy(legacyDataDir, legacyCertDir, "127.0.0.1:0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	oldConfigJSON := []byte(`{"upstreamMode":"direct","allowLAN":true}`)
	oldWindowJSON := []byte(`{"x":100,"y":120,"width":980,"height":640}`)
	if err := os.WriteFile(filepath.Join(legacyRoot, "config.json"), oldConfigJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyRoot, "window.json"), oldWindowJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	// 新版 certsDir 应自动迁移证书与配置至 %APPDATA%\GBF-Dock，且保留旧目录不动
	newDataDir := t.TempDir()
	gotCertDir := certsDir(newDataDir)
	wantCertDir := filepath.Join(appData, appConfigDirName, "certs")
	if gotCertDir != wantCertDir {
		t.Fatalf("certsDir = %q, want %q", gotCertDir, wantCertDir)
	}

	migratedProxy, err := newProxy(newDataDir, gotCertDir, "127.0.0.1:0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(migratedProxy.ca.Raw, oldProxy.ca.Raw) {
		t.Fatal("migrated CA must match legacy GBFCache CA")
	}
	newCfgDir := filepath.Dir(gotCertDir)
	if gotCfg, err := os.ReadFile(filepath.Join(newCfgDir, "config.json")); err != nil || !bytes.Equal(gotCfg, oldConfigJSON) {
		t.Fatalf("migrated config.json mismatch: err=%v data=%q", err, gotCfg)
	}
	if gotWin, err := os.ReadFile(filepath.Join(newCfgDir, "window.json")); err != nil || !bytes.Equal(gotWin, oldWindowJSON) {
		t.Fatalf("migrated window.json mismatch: err=%v data=%q", err, gotWin)
	}
	if !fileExists(filepath.Join(legacyCertDir, "ca.crt")) || !fileExists(filepath.Join(legacyRoot, "config.json")) {
		t.Fatal("legacy GBFCache config/certs must stay untouched after migration")
	}

	// isOldCA 能同时识别旧版 gbf-cache local CA 与另一份 GBF-Dock Local CA
	otherDir := t.TempDir()
	otherCertPath := filepath.Join(otherDir, "legacy_ca.crt")
	otherKeyPath := filepath.Join(otherDir, "legacy_ca.key")
	if err := genCA(otherCertPath, otherKeyPath); err != nil {
		t.Fatal(err)
	}
	otherProxy, err := newProxy(otherDir, otherDir, "127.0.0.1:0", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(migratedProxy, &logRing{})
	if !a.isOldCA(otherProxy.ca.Raw) {
		t.Fatal("isOldCA should recognize other GBF-Dock CA")
	}
}
