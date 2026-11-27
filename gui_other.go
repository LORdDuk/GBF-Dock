//go:build !windows

package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

var errWindowsOnly = errors.New("这个功能只在 Windows 版提供，macOS 请按 README 手动设置")

func installCert([]byte) error                   { return errWindowsOnly }
func removeCerts(func([]byte) bool) (int, error) { return 0, errWindowsOnly }
func countCerts(func([]byte) bool) int           { return 0 }
func pacState() (string, string)                 { return "", "" }
func isProxyServerSelf(string, string) bool      { return false }
func savedBackupServer(string, string) string    { return "" }
func savedBackupPAC(string, string, string) string { return "" }
func proxyState(string, string, string) (string, string, bool, bool, string) {
	return "", "", false, false, ""
}
func enableSystemProxy(string, string, string) error { return errWindowsOnly }
func disableSystemProxy(string, string, string, string) error {
	return nil
}
func resetSystemProxy(string, string, string, string) error { return errWindowsOnly }

func openURL(u string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	return exec.Command(name, u).Start()
}

func openFolder(dir string) error { return openURL(dir) }

// 非 Windows：控制面板在默认浏览器里打开，程序在终端里运行。
func launchWindow(url string) error { return openURL(url) }

func closePanelWindow(string) {}

func fatal(msg string) { log.Fatal(msg) }

// 非 Windows：没有托盘、原生文件夹选择框与注册表开机自启。
func startTray(*app) bool                                  { return false }
func trayNotify(string, string)                            {}
func removeTray()                                          {}
func pickFolderDialog(string) (string, bool, error)        { return "", false, errWindowsOnly }
func userCommonFolders() []string                          { return nil }
func getAutoStart() bool                                   { return false }
func setAutoStart(bool) error                              { return errWindowsOnly }

func detectInstalledBrowsers() []browserCandidate {
	var candidates []struct {
		name  string
		paths []string
	}
	if runtime.GOOS == "darwin" {
		candidates = []struct {
			name  string
			paths []string
		}{
			{"Google Chrome", []string{"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"}},
			{"Microsoft Edge", []string{"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"}},
			{"Brave", []string{"/Applications/Brave Browser.app/Contents/MacOS/Brave Browser"}},
			{"Vivaldi", []string{"/Applications/Vivaldi.app/Contents/MacOS/Vivaldi"}},
		}
	} else {
		candidates = []struct {
			name  string
			paths []string
		}{
			{"Google Chrome", []string{"/usr/bin/google-chrome", "/usr/bin/google-chrome-stable"}},
			{"Chromium", []string{"/usr/bin/chromium", "/usr/bin/chromium-browser"}},
			{"Microsoft Edge", []string{"/usr/bin/microsoft-edge"}},
			{"Brave", []string{"/usr/bin/brave-browser"}},
		}
	}
	var out []browserCandidate
	for _, c := range candidates {
		for _, p := range c.paths {
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				out = append(out, browserCandidate{Name: c.name, Path: p, Kind: "exe"})
				break
			}
		}
	}
	return out
}

func pickBrowserFileDialog(string) (string, bool, error) {
	return "", false, errors.New("当前系统暂不支持图形文件选择框，请在设置中输入浏览器可执行文件路径")
}

func resolveShortcutLnk(string) (string, string, string, error) {
	return "", "", "", errors.New("非 Windows 系统不支持 .lnk 快捷方式")
}

func launchBrowserProcess(exePath string, args []string, workDir string) error {
	cmd := exec.Command(exePath, args...)
	if wd := strings.TrimSpace(workDir); wd != "" {
		if st, err := os.Stat(wd); err == nil && st.IsDir() {
			cmd.Dir = wd
		}
	}
	if cmd.Dir == "" {
		cmd.Dir = filepath.Dir(exePath)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动浏览器失败：%w", err)
	}
	if cmd.Process != nil {
		_ = cmd.Process.Release()
	}
	return nil
}

func isBrowserInstanceRunning(string, string) bool           { return false }
func closeBrowserProcessesGracefully(string, string) error   { return nil }
func confirmRestartBrowserDialog(string) bool                { return false }
