//go:build !windows

package main

import (
	"errors"
	"log"
	"os/exec"
	"runtime"
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
