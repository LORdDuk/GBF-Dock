package main

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	browserProfileModeOriginal = "original"
	browserProfileModeIsolated = "isolated"
)

// browserCandidate 表示本机自动探测到的可用 Chromium 浏览器（或预设快捷方式）。
type browserCandidate struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Kind string `json:"kind"` // "exe" | "lnk"
}

// gameBrowserInfo 描述当前已选定用于“独立加速启动”的浏览器或快捷方式状态。
type gameBrowserInfo struct {
	Configured       bool   `json:"configured"`
	Path             string `json:"path,omitempty"`
	Label            string `json:"label,omitempty"`
	Kind             string `json:"kind,omitempty"`        // "exe" | "lnk"
	TargetExe        string `json:"targetExe,omitempty"`   // 解析后的实际浏览器 .exe 路径
	RawArgs          string `json:"rawArgs,omitempty"`     // .lnk 原始启动参数
	WorkDir          string `json:"workDir,omitempty"`     // 启动工作目录
	HasCustomDir     bool   `json:"hasCustomDir"`          // .lnk 是否自带 --user-data-dir
	ProfileMode      string `json:"profileMode,omitempty"` // "original" | "isolated"
	ProfileDir       string `json:"profileDir,omitempty"`  // 独立配置模式下自动分配的目录
	ProfileDiskBytes int64  `json:"profileDiskBytes"`      // 已产生的独立浏览器配置数据占用字节
	ProfileDiskDirs  int    `json:"profileDiskDirs"`       // 已产生的独立浏览器配置目录数
	PACURL           string `json:"pacUrl,omitempty"`      // 本机分流 PAC 地址
	Error            string `json:"error,omitempty"`
}

func normalizeBrowserProfileMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), browserProfileModeIsolated) {
		return browserProfileModeIsolated
	}
	return browserProfileModeOriginal
}

// upstreamToPACReturn 将 GBF-Dock 的上游代理 URL 转为 FindProxyForURL 返回值。
func upstreamToPACReturn(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "DIRECT"
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "DIRECT"
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h", "socks":
		return "SOCKS5 " + u.Host + "; DIRECT"
	case "http", "https":
		return "PROXY " + u.Host + "; DIRECT"
	default:
		return "DIRECT"
	}
}

// splitWindowsCommandLine 按 Windows 命令行引号规则拆分快捷方式参数串。
func splitWindowsCommandLine(cmd string) []string {
	var args []string
	var cur strings.Builder
	inToken := false
	inQuotes := false
	backslashes := 0

	flush := func() {
		if inToken {
			args = append(args, cur.String())
			cur.Reset()
			inToken = false
		}
	}

	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if c == '\\' {
			backslashes++
			inToken = true
			continue
		}
		if c == '"' {
			inToken = true
			if backslashes > 0 {
				cur.WriteString(strings.Repeat(`\`, backslashes/2))
				if backslashes%2 == 1 {
					cur.WriteByte('"')
					backslashes = 0
					continue
				}
				backslashes = 0
			}
			inQuotes = !inQuotes
			continue
		}
		if backslashes > 0 {
			cur.WriteString(strings.Repeat(`\`, backslashes))
			backslashes = 0
		}
		if !inQuotes && (c == ' ' || c == '\t' || c == '\r' || c == '\n') {
			flush()
			continue
		}
		inToken = true
		cur.WriteByte(c)
	}
	if backslashes > 0 {
		cur.WriteString(strings.Repeat(`\`, backslashes))
	}
	flush()
	return args
}

// extractUserDataDirFromArgs 提取命令行参数中显式指定的 --user-data-dir 路径。
func extractUserDataDirFromArgs(rawArgs string) string {
	tokens := splitWindowsCommandLine(rawArgs)
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		low := strings.ToLower(tok)
		if low == "--user-data-dir" && i+1 < len(tokens) && !strings.HasPrefix(tokens[i+1], "-") {
			return strings.TrimSpace(tokens[i+1])
		}
		if strings.HasPrefix(low, "--user-data-dir=") {
			return strings.TrimSpace(tok[len("--user-data-dir="):])
		}
	}
	return ""
}

// isUnsupportedBrowserExe 识别不支持 --proxy-pac-url 参数的非 Chromium 内核浏览器。
func isUnsupportedBrowserExe(exePath string) (bool, string) {
	base := strings.ToLower(strings.TrimSpace(filepath.Base(exePath)))
	switch base {
	case "firefox.exe", "firefox", "librewolf.exe", "waterfox.exe", "zen.exe", "floorp.exe":
		return true, fmt.Sprintf("%s 属于 Firefox / Gecko 内核，不支持 Chromium 的 --proxy-pac-url 启动参数；请选择 Chrome、Edge、Brave、Vivaldi、百分浏览器等 Chromium 系浏览器", base)
	}
	return false, ""
}

func friendlyBrowserLabel(sourcePath, targetExe string) string {
	ext := strings.ToLower(filepath.Ext(sourcePath))
	if ext == ".lnk" {
		base := strings.TrimSuffix(filepath.Base(sourcePath), filepath.Ext(sourcePath))
		if base != "" {
			return base
		}
	}
	lowPath := strings.ToLower(targetExe)
	base := strings.ToLower(filepath.Base(targetExe))
	switch {
	case strings.Contains(lowPath, "centbrowser"):
		return "Cent Browser (百分浏览器)"
	case strings.Contains(lowPath, "360chrome"):
		return "360 极速浏览器"
	case base == "msedge.exe" || base == "microsoft-edge":
		return "Microsoft Edge"
	case base == "chrome.exe" || base == "google-chrome":
		if strings.Contains(lowPath, "chromium") {
			return "Chromium"
		}
		return "Google Chrome"
	case base == "brave.exe" || base == "brave-browser":
		return "Brave"
	case base == "vivaldi.exe" || base == "vivaldi":
		return "Vivaldi"
	case base == "opera.exe" || (base == "launcher.exe" && strings.Contains(lowPath, "opera")):
		return "Opera"
	default:
		name := strings.TrimSuffix(filepath.Base(sourcePath), filepath.Ext(sourcePath))
		if name != "" {
			return name
		}
		return targetExe
	}
}

func sanitizeProfileSlug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.':
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-_")
	if s == "" {
		return "browser"
	}
	if len(s) > 28 {
		s = s[:28]
	}
	return s
}

func browserProfilesRoot(baseDir string) string {
	if strings.TrimSpace(baseDir) == "" {
		return ""
	}
	return filepath.Join(baseDir, "browser-profile")
}

// browserProfileDir 为“独立空白配置”模式分配独立的持久化配置目录。
func browserProfileDir(baseDir, sourcePath, targetExe string) string {
	root := browserProfilesRoot(baseDir)
	if root == "" {
		return ""
	}
	ref := sourcePath
	if strings.TrimSpace(ref) == "" {
		ref = targetExe
	}
	base := strings.TrimSuffix(filepath.Base(ref), filepath.Ext(ref))
	slug := sanitizeProfileSlug(base)
	sum := sha1.Sum([]byte(strings.ToLower(filepath.Clean(ref))))
	return filepath.Join(root, slug+"-"+hex.EncodeToString(sum[:4]))
}

func dirTotalBytes(root string) int64 {
	var total int64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, iErr := d.Info(); iErr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

func scanBrowserProfilesUsage(baseDir string) (dirs int, totalBytes int64) {
	root := browserProfilesRoot(baseDir)
	if root == "" {
		return 0, 0
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, 0
	}
	for _, ent := range entries {
		p := filepath.Join(root, ent.Name())
		if ent.IsDir() {
			dirs++
			totalBytes += dirTotalBytes(p)
		} else if info, iErr := ent.Info(); iErr == nil {
			totalBytes += info.Size()
		}
	}
	return dirs, totalBytes
}

func clearBrowserProfiles(baseDir string) (removedDirs int, freedBytes int64, err error) {
	root := browserProfilesRoot(baseDir)
	if root == "" {
		return 0, 0, nil
	}
	entries, rErr := os.ReadDir(root)
	if rErr != nil {
		if errors.Is(rErr, os.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, rErr
	}
	var firstErr error
	for _, ent := range entries {
		p := filepath.Join(root, ent.Name())
		sz := int64(0)
		if ent.IsDir() {
			sz = dirTotalBytes(p)
		} else if info, iErr := ent.Info(); iErr == nil {
			sz = info.Size()
		}
		if rmErr := os.RemoveAll(p); rmErr != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("部分配置目录正在被打开的浏览器占用，请先关闭通过「独立空白配置」启动的游戏窗口后再清理（%w）", rmErr)
			}
			continue
		}
		if ent.IsDir() {
			removedDirs++
		}
		freedBytes += sz
	}
	if left, lErr := os.ReadDir(root); lErr == nil && len(left) == 0 {
		_ = os.Remove(root)
	}
	return removedDirs, freedBytes, firstErr
}

func isHTTPURL(s string) bool {
	low := strings.ToLower(strings.TrimSpace(s))
	return strings.HasPrefix(low, "https://") || strings.HasPrefix(low, "http://")
}

// buildBrowserLaunchArgs 构造传给 Chromium 系浏览器的单次启动参数列表（--proxy-pac-url 仅当次生效，关闭浏览器自动消失）：
// 1. 保留用户快捷方式中除旧代理开关外的原有参数（含 --user-data-dir、--profile-directory、--app=... 等）；
// 2. 若快捷方式未自带 --user-data-dir：
//    - profileMode=="original"（默认）：不注入 --user-data-dir，沿用浏览器原账号、登录态、Cookie 与扩展；
//    - profileMode=="isolated"：注入 GBF-Dock 独立配置目录，实现与主浏览器双开隔离；
// 3. 注入 --proxy-pac-url=<pacURL>，只在访问 GBF CDN 时走 GBF-Dock。
func buildBrowserLaunchArgs(targetExe, rawArgs, pacURL, defaultProfileDir, targetURL string, forceTargetURL bool, profileMode string) (args []string, hasCustomProfile bool, err error) {
	if bad, reason := isUnsupportedBrowserExe(targetExe); bad {
		return nil, false, errors.New(reason)
	}
	if strings.TrimSpace(pacURL) == "" {
		return nil, false, errors.New("PAC 地址不能为空")
	}

	mode := normalizeBrowserProfileMode(profileMode)
	tokens := splitWindowsCommandLine(rawArgs)
	out := make([]string, 0, len(tokens)+4)
	hasURL := false

	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		low := strings.ToLower(tok)

		switch {
		case low == "--proxy-pac-url" || low == "--proxy-server" || low == "--proxy-bypass-list":
			if i+1 < len(tokens) && !strings.HasPrefix(tokens[i+1], "-") {
				i++
			}
			continue
		case strings.HasPrefix(low, "--proxy-pac-url=") ||
			strings.HasPrefix(low, "--proxy-server=") ||
			strings.HasPrefix(low, "--proxy-bypass-list=") ||
			low == "--no-proxy-server" ||
			low == "--proxy-auto-detect":
			continue

		case low == "--user-data-dir":
			if i+1 < len(tokens) && !strings.HasPrefix(tokens[i+1], "-") {
				hasCustomProfile = true
				out = append(out, tok, tokens[i+1])
				i++
				continue
			}
		case strings.HasPrefix(low, "--user-data-dir="):
			val := strings.TrimSpace(tok[len("--user-data-dir="):])
			if val != "" {
				hasCustomProfile = true
			}
			out = append(out, tok)
			continue

		case strings.HasPrefix(low, "--app="):
			appVal := strings.TrimSpace(tok[len("--app="):])
			if forceTargetURL && isHTTPURL(appVal) {
				continue
			}
			if isHTTPURL(appVal) {
				hasURL = true
			}
			out = append(out, tok)
			continue

		case !strings.HasPrefix(tok, "-") && isHTTPURL(tok):
			if forceTargetURL {
				continue
			}
			hasURL = true
			out = append(out, tok)
			continue
		}

		out = append(out, tok)
	}

	if !hasCustomProfile && mode == browserProfileModeIsolated && strings.TrimSpace(defaultProfileDir) != "" {
		out = append(out, "--user-data-dir="+defaultProfileDir)
	}
	out = append(out, "--proxy-pac-url="+pacURL)
	if strings.TrimSpace(targetURL) != "" && (forceTargetURL || !hasURL) {
		out = append(out, targetURL)
	}
	return out, hasCustomProfile, nil
}

// inspectGameBrowser 验证并解析用户选择的浏览器程序（.exe）或快捷方式（.lnk）。
func inspectGameBrowser(rawPath, rawLabel, rawProfileMode, baseDir, pacURL string) gameBrowserInfo {
	mode := normalizeBrowserProfileMode(rawProfileMode)
	dirs, totalBytes := scanBrowserProfilesUsage(baseDir)

	p := strings.TrimSpace(rawPath)
	info := gameBrowserInfo{
		ProfileMode:      mode,
		ProfileDiskDirs:  dirs,
		ProfileDiskBytes: totalBytes,
		PACURL:           pacURL,
	}
	if p == "" {
		return info
	}
	p = filepath.Clean(p)
	info.Configured = true
	info.Path = p
	info.Label = strings.TrimSpace(rawLabel)

	st, err := os.Stat(p)
	if err != nil {
		info.Error = "所选浏览器或快捷方式文件不存在"
		return info
	}
	if st.IsDir() {
		info.Error = "请选择浏览器可执行文件（.exe）或快捷方式（.lnk），不要选择文件夹"
		return info
	}

	ext := strings.ToLower(filepath.Ext(p))
	switch ext {
	case ".lnk":
		info.Kind = "lnk"
		target, args, wd, lErr := resolveShortcutLnk(p)
		if lErr != nil {
			info.Error = lErr.Error()
			return info
		}
		target = strings.TrimSpace(target)
		if target == "" {
			info.Error = "快捷方式未指向有效的程序路径"
			return info
		}
		target = filepath.Clean(target)
		if tst, tErr := os.Stat(target); tErr != nil || tst.IsDir() {
			info.Error = fmt.Sprintf("快捷方式指向的目标程序不存在：%s", target)
			return info
		}
		if runtime.GOOS == "windows" && strings.ToLower(filepath.Ext(target)) != ".exe" {
			info.Error = fmt.Sprintf("快捷方式目标不是 .exe 程序：%s", filepath.Base(target))
			return info
		}
		info.TargetExe = target
		info.RawArgs = strings.TrimSpace(args)
		info.WorkDir = strings.TrimSpace(wd)
		if info.WorkDir == "" {
			info.WorkDir = filepath.Dir(target)
		}

	default:
		if runtime.GOOS == "windows" && ext != ".exe" {
			info.Error = "仅支持选择浏览器程序（.exe）或快捷方式（.lnk）"
			return info
		}
		info.Kind = "exe"
		info.TargetExe = p
		info.WorkDir = filepath.Dir(p)
	}

	if info.Label == "" {
		info.Label = friendlyBrowserLabel(p, info.TargetExe)
	}

	defProf := browserProfileDir(baseDir, p, info.TargetExe)
	_, hasCustom, bErr := buildBrowserLaunchArgs(info.TargetExe, info.RawArgs, pacURL, defProf, gameURL, false, mode)
	if bErr != nil {
		info.Error = bErr.Error()
		return info
	}
	info.HasCustomDir = hasCustom
	if !hasCustom && mode == browserProfileModeIsolated {
		info.ProfileDir = defProf
	}
	return info
}
