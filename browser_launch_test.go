package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestUpstreamToPACReturn(t *testing.T) {
	cases := map[string]string{
		"":                         "DIRECT",
		"   ":                      "DIRECT",
		"http://127.0.0.1:7890":    "PROXY 127.0.0.1:7890; DIRECT",
		"https://127.0.0.1:8443":   "PROXY 127.0.0.1:8443; DIRECT",
		"127.0.0.1:7897":           "PROXY 127.0.0.1:7897; DIRECT",
		"socks5://127.0.0.1:10808": "SOCKS5 127.0.0.1:10808; DIRECT",
		"socks5h://127.0.0.1:2080": "SOCKS5 127.0.0.1:2080; DIRECT",
		"ftp://127.0.0.1:21":       "DIRECT",
	}
	for in, want := range cases {
		if got := upstreamToPACReturn(in); got != want {
			t.Errorf("upstreamToPACReturn(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSplitWindowsCommandLine(t *testing.T) {
	in := `--user-data-dir="D:\游戏 目录\GBF Profile" --app="https://game.granbluefantasy.jp/" --proxy-server=127.0.0.1:7890 --no-first-run`
	got := splitWindowsCommandLine(in)
	want := []string{
		`--user-data-dir=D:\游戏 目录\GBF Profile`,
		`--app=https://game.granbluefantasy.jp/`,
		`--proxy-server=127.0.0.1:7890`,
		`--no-first-run`,
	}
	if !slices.Equal(got, want) {
		t.Fatalf("splitWindowsCommandLine = %#v, want %#v", got, want)
	}
}

func TestBuildBrowserLaunchArgs(t *testing.T) {
	const pac = "http://127.0.0.1:8765/proxy.pac"
	const defProf = `C:\Users\test\AppData\Roaming\GBF-Dock\browser-profile\chrome-1234`

	// 1. Original mode (default): does NOT inject --user-data-dir, preserves user logins/extensions & .lnk --profile-directory
	args, hasCustom, err := buildBrowserLaunchArgs(`C:\Program Files\Google\Chrome\Application\chrome.exe`, `--profile-directory="Profile 2"`, pac, defProf, gameURL, false, browserProfileModeOriginal)
	if err != nil {
		t.Fatalf("original mode failed: %v", err)
	}
	if hasCustom {
		t.Fatal("original mode without --user-data-dir must not report hasCustomProfile=true")
	}
	want1 := []string{"--profile-directory=Profile 2", "--proxy-pac-url=" + pac, gameURL}
	if !slices.Equal(args, want1) {
		t.Fatalf("original mode args = %#v, want %#v", args, want1)
	}

	// 2. Isolated mode: injects dedicated --user-data-dir so it can run side-by-side with main browser
	argsIso, hasCustomIso, err := buildBrowserLaunchArgs(`C:\Program Files\Google\Chrome\Application\chrome.exe`, "", pac, defProf, gameURL, false, browserProfileModeIsolated)
	if err != nil {
		t.Fatalf("isolated mode failed: %v", err)
	}
	if hasCustomIso {
		t.Fatal("bare exe must not report hasCustomProfile=true")
	}
	wantIso := []string{"--user-data-dir=" + defProf, "--proxy-pac-url=" + pac, gameURL}
	if !slices.Equal(argsIso, wantIso) {
		t.Fatalf("isolated mode args = %#v, want %#v", argsIso, wantIso)
	}

	// 3. .lnk with explicit --user-data-dir preserves it even in isolated mode, strips old proxy flags
	rawLnk := `--user-data-dir="D:\GBF Profile" --proxy-server=127.0.0.1:7890 --proxy-pac-url http://old/pac --no-proxy-server --app="https://game.granbluefantasy.jp/" --disable-gpu`
	argsLnk, hasCustomLnk, err := buildBrowserLaunchArgs(`C:\Program Files\Microsoft\Edge\Application\msedge.exe`, rawLnk, pac, defProf, gameURL, false, browserProfileModeIsolated)
	if err != nil {
		t.Fatalf("lnk with custom profile failed: %v", err)
	}
	if !hasCustomLnk {
		t.Fatal("expected hasCustomProfile=true")
	}
	if gotDir := extractUserDataDirFromArgs(rawLnk); gotDir != `D:\GBF Profile` {
		t.Fatalf("extractUserDataDirFromArgs = %q, want D:\\GBF Profile", gotDir)
	}
	want2 := []string{
		`--user-data-dir=D:\GBF Profile`,
		`--app=https://game.granbluefantasy.jp/`,
		`--disable-gpu`,
		`--proxy-pac-url=` + pac,
	}
	if !slices.Equal(argsLnk, want2) {
		t.Fatalf("lnk custom profile args = %#v, want %#v", argsLnk, want2)
	}

	// 4. forceTargetURL overrides --app URL (used by check-browser)
	checkTarget := checkURL + "?t=999"
	argsChk, _, err := buildBrowserLaunchArgs(`C:\Program Files\Brave\brave.exe`, `--app="https://game.granbluefantasy.jp/" --start-maximized`, pac, defProf, checkTarget, true, browserProfileModeOriginal)
	if err != nil {
		t.Fatalf("forceTargetURL failed: %v", err)
	}
	want3 := []string{
		`--start-maximized`,
		`--proxy-pac-url=` + pac,
		checkTarget,
	}
	if !slices.Equal(argsChk, want3) {
		t.Fatalf("forceTargetURL args = %#v, want %#v", argsChk, want3)
	}

	// 5. Reject Gecko / Firefox
	if _, _, err := buildBrowserLaunchArgs(`C:\Program Files\Mozilla Firefox\firefox.exe`, "", pac, defProf, gameURL, false, browserProfileModeOriginal); err == nil {
		t.Fatal("expected firefox.exe to be rejected")
	}
}

func TestPACPrefersUpstreamProxy(t *testing.T) {
	dir := t.TempDir()
	p, err := newProxy(dir, filepath.Join(dir, "certs"), "127.0.0.1:8765", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(p, &logRing{})
	a.sysFn = func() sysState { return sysState{} }

	getPAC := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/proxy.pac", nil)
		r.Host = "127.0.0.1:8765"
		r.RemoteAddr = "127.0.0.1:54321"
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		return w
	}

	if err := p.setUpstream("http://127.0.0.1:7890"); err != nil {
		t.Fatal(err)
	}
	w := getPAC()
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("pac http status=%d cc=%q", w.Code, w.Header().Get("Cache-Control"))
	}
	body := w.Body.String()
	if !strings.Contains(body, `return "PROXY 127.0.0.1:8765; DIRECT";`) ||
		!strings.Contains(body, `return "PROXY 127.0.0.1:7890; DIRECT";`) {
		t.Fatalf("unexpected HTTP upstream PAC body:\n%s", body)
	}

	if err := p.setUpstream("socks5://127.0.0.1:10808"); err != nil {
		t.Fatal(err)
	}
	body = getPAC().Body.String()
	if !strings.Contains(body, `return "SOCKS5 127.0.0.1:10808; DIRECT";`) {
		t.Fatalf("unexpected SOCKS5 upstream PAC body:\n%s", body)
	}

	if _, err := a.applyUpstreamConfig("direct", ""); err != nil {
		t.Fatal(err)
	}
	body = getPAC().Body.String()
	if !strings.Contains(body, `return "DIRECT";`) {
		t.Fatalf("unexpected DIRECT PAC body:\n%s", body)
	}
}

func TestBrowserSetProfileModeCleanupAndHealth(t *testing.T) {
	dir := t.TempDir()
	certDir := filepath.Join(dir, "cfg", "certs")
	p, err := newProxy(dir, certDir, "127.0.0.1:8765", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(p, &logRing{})
	a.sysFn = func() sysState { return sysState{CertTrusted: true, ProxyEnabled: false} }

	fakeDir := filepath.Join(dir, "Chrome", "Application")
	if err := os.MkdirAll(fakeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeChrome := filepath.Join(fakeDir, "chrome.exe")
	if err := os.WriteFile(fakeChrome, []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}

	doPost := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Host = "127.0.0.1:8765"
		r.Header.Set(csrfHeaderName, "1")
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		return w
	}

	// Create dummy isolated profile data on disk to test usage scan + cleanup
	dummyProf := filepath.Join(a.configBaseDir(), "browser-profile", "chrome-test", "Default")
	if err := os.MkdirAll(dummyProf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dummyProf, "Cache"), make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	// 1. Select dummy chrome.exe (defaults to "original" profile mode)
	payload, _ := json.Marshal(map[string]string{"path": fakeChrome, "label": "Test Chrome"})
	w := doPost("/api/browser-set", string(payload))
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("browser-set failed: %d %s", w.Code, w.Body.String())
	}

	// Status should reflect configured browser, original profile mode, and dummy profile disk usage
	readStatus := func() gameBrowserInfo {
		sr := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		sr.Host = "127.0.0.1:8765"
		sw := httptest.NewRecorder()
		p.ServeHTTP(sw, sr)
		var st struct {
			GameBrowser gameBrowserInfo `json:"gameBrowser"`
		}
		if err := json.Unmarshal(sw.Body.Bytes(), &st); err != nil {
			t.Fatalf("decode status: %v", err)
		}
		return st.GameBrowser
	}

	gb := readStatus()
	if !gb.Configured || gb.TargetExe != filepath.Clean(fakeChrome) || gb.ProfileMode != browserProfileModeOriginal {
		t.Fatalf("unexpected gameBrowser in status: %+v", gb)
	}
	if gb.ProfileDiskDirs != 1 || gb.ProfileDiskBytes != 4096 {
		t.Fatalf("expected ProfileDiskDirs=1 ProfileDiskBytes=4096, got %+v", gb)
	}

	// 2. Switch profileMode via keepPath=true and verify config.json persistence
	wMode := doPost("/api/browser-set", `{"keepPath":true,"profileMode":"isolated"}`)
	if wMode.Code != http.StatusOK || strings.Contains(wMode.Body.String(), `"error"`) {
		t.Fatalf("browser-set keepPath failed: %d %s", wMode.Code, wMode.Body.String())
	}
	cfgBytes, err := os.ReadFile(a.configPath)
	if err != nil {
		t.Fatalf("config.json not written: %v", err)
	}
	var cfg appConfig
	if err := json.Unmarshal(cfgBytes, &cfg); err != nil || cfg.GameBrowserPath != filepath.Clean(fakeChrome) || cfg.GameBrowserProfileMode != browserProfileModeIsolated {
		t.Fatalf("persisted config mismatch: %+v err=%v", cfg, err)
	}

	// 3. Cleanup isolated profile directories
	wClean := doPost("/api/browser-profile-clear", `{}`)
	if wClean.Code != http.StatusOK || strings.Contains(wClean.Body.String(), `"error"`) {
		t.Fatalf("browser-profile-clear failed: %d %s", wClean.Code, wClean.Body.String())
	}
	gbAfterClean := readStatus()
	if gbAfterClean.ProfileDiskDirs != 0 || gbAfterClean.ProfileDiskBytes != 0 {
		t.Fatalf("expected 0 disk usage after clear, got %+v", gbAfterClean)
	}

	// 4. Health check proxy item should be "ok" even when system proxy is off
	rep := a.runHealthCheck()
	var proxyItem healthItem
	for _, it := range rep.Items {
		if it.Key == "proxy" {
			proxyItem = it
		}
	}
	if proxyItem.Status != "ok" || !strings.Contains(proxyItem.Detail, "独立浏览器模式就绪") {
		t.Fatalf("expected health proxyItem ok for isolated browser, got %+v", proxyItem)
	}

	// 5. Clear browser selection
	wClr := doPost("/api/browser-set", `{"path":""}`)
	if wClr.Code != http.StatusOK || !strings.Contains(wClr.Body.String(), "系统默认浏览器") {
		t.Fatalf("browser-set clear failed: %d %s", wClr.Code, wClr.Body.String())
	}
	if a.hasValidGameBrowser() {
		t.Fatal("expected game browser cleared")
	}
}

func TestAccessModeMutualExclusionAndUI(t *testing.T) {
	dir := t.TempDir()
	p, err := newProxy(dir, filepath.Join(dir, "cfg", "certs"), "127.0.0.1:8765", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(p, &logRing{})
	a.sysFn = func() sysState { return sysState{CertTrusted: true, ProxyEnabled: false} }

	doReq := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Host = "127.0.0.1:8765"
		r.Header.Set(csrfHeaderName, "1")
		w := httptest.NewRecorder()
		p.ServeHTTP(w, r)
		return w
	}

	// Default accessMode is "isolated"
	var st struct {
		AccessMode string `json:"accessMode"`
	}
	if err := json.Unmarshal(doReq(http.MethodGet, "/api/status", "").Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.AccessMode != "isolated" {
		t.Fatalf("default accessMode = %q, want isolated", st.AccessMode)
	}

	// Switch to "system" mode & verify persistence
	wSys := doReq(http.MethodPost, "/api/set-access-mode", `{"mode":"system"}`)
	if wSys.Code != http.StatusOK || !strings.Contains(wSys.Body.String(), `"accessMode":"system"`) {
		t.Fatalf("set-access-mode system failed: %d %s", wSys.Code, wSys.Body.String())
	}
	if err := json.Unmarshal(doReq(http.MethodGet, "/api/status", "").Body.Bytes(), &st); err != nil || st.AccessMode != "system" {
		t.Fatalf("expected accessMode=system, got %+v err=%v", st, err)
	}

	// Switch back to "isolated"
	wIso := doReq(http.MethodPost, "/api/set-access-mode", `{"mode":"isolated"}`)
	if wIso.Code != http.StatusOK || !strings.Contains(wIso.Body.String(), `"accessMode":"isolated"`) {
		t.Fatalf("set-access-mode isolated failed: %d %s", wIso.Code, wIso.Body.String())
	}
	cfgBytes, err := os.ReadFile(a.configPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg appConfig
	if err := json.Unmarshal(cfgBytes, &cfg); err != nil || cfg.AccessMode != "isolated" {
		t.Fatalf("expected persisted accessMode=isolated, got %+v err=%v", cfg, err)
	}

	// Verify redesigned UI serves 5 tabs & 3-way theme mode
	uiResp := doReq(http.MethodGet, "/", "")
	body := uiResp.Body.String()
	for _, want := range []string{"gbf-theme-mode", `data-tab="browser"`, `data-tab="cache"`, `data-tab="network"`, `data-access="isolated"`, `data-access="system"`, `data-feed="all"`} {
		if !strings.Contains(body, want) {
			t.Errorf("UI HTML missing expected marker %q", want)
		}
	}
}

func TestActivityRingInStatus(t *testing.T) {
	dir := t.TempDir()
	p, err := newProxy(dir, filepath.Join(dir, "cfg", "certs"), "127.0.0.1:8765", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	a := newApp(p, &logRing{})
	a.sysFn = func() sysState { return sysState{CertTrusted: true} }

	p.recordEvent("https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/1729002592/img/sp/quest/scene_1.png?v=1", nil, "miss", 12345)
	p.recordEvent("https://prd-game-a1-granbluefantasy.akamaized.net/assets_en/1729002592/sound/voice/v_01.mp3", nil, "ram", 4096)

	r := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	r.Host = "127.0.0.1:8765"
	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("/api/status code = %d", w.Code)
	}
	var st struct {
		Events []assetEvent `json:"events"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Events) != 2 {
		t.Fatalf("events len = %d, want 2", len(st.Events))
	}
	if st.Events[0].Kind != "ram" || st.Events[0].Cat != "audio" || st.Events[0].Path != "/assets_en/sound/voice/v_01.mp3" || st.Events[0].Bytes != 4096 {
		t.Fatalf("unexpected newest event: %+v", st.Events[0])
	}
	if st.Events[1].Kind != "miss" || st.Events[1].Cat != "image" || st.Events[1].Path != "/assets_en/img/sp/quest/scene_1.png" || st.Events[1].Bytes != 12345 {
		t.Fatalf("unexpected second event: %+v", st.Events[1])
	}
}
