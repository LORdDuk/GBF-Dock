//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// 全部直接调用 Windows API：不启动 certutil、rundll32、reg 等命令行工具，也不去翻浏览器的安装目录。

var (
	crypt32               = syscall.NewLazyDLL("crypt32.dll")
	procCertDuplicate     = crypt32.NewProc("CertDuplicateCertificateContext")
	procCertDelete        = crypt32.NewProc("CertDeleteCertificateFromStore")
	ole32                 = syscall.NewLazyDLL("ole32.dll")
	procCoInitializeEx    = ole32.NewProc("CoInitializeEx")
	procCoUninitialize    = ole32.NewProc("CoUninitialize")
	procCoCreateInstance  = ole32.NewProc("CoCreateInstance")
	procCoTaskMemFree     = ole32.NewProc("CoTaskMemFree")
	advapi32              = syscall.NewLazyDLL("advapi32.dll")
	procRegSetValueExW    = advapi32.NewProc("RegSetValueExW")
	procRegDeleteValueW   = advapi32.NewProc("RegDeleteValueW")
	procShellExecuteW     = shell32.NewProc("ShellExecuteW")
	procEnumWindows       = user32.NewProc("EnumWindows")
	procIsWindowVisible   = user32.NewProc("IsWindowVisible")
	procGetWindowTextLenW = user32.NewProc("GetWindowTextLengthW")
	procGetWindowTextW    = user32.NewProc("GetWindowTextW")
	procIsIconic          = user32.NewProc("IsIconic")
	procIsZoomed          = user32.NewProc("IsZoomed")
	procIsWindow          = user32.NewProc("IsWindow")
	procShowWindow        = user32.NewProc("ShowWindow")
	procBringWindowToTop         = user32.NewProc("BringWindowToTop")
	procGetDpiForSystem          = user32.NewProc("GetDpiForSystem")
	procGetGUIThreadInfo         = user32.NewProc("GetGUIThreadInfo")
)

const (
	certStoreAddUseExisting = 2 // CERT_STORE_ADD_USE_EXISTING：已装过就不重复添加
	swMaximize              = 3
	swRestore               = 9
	swpNoZOrder             = 0x0004
	wmClose                 = 0x0010
	guiInMoveSize           = 0x00000002
	defaultProxyBypass      = "localhost;127.*;10.*;172.16.*;192.168.*;<local>"
	panelMinWidthDIP        = 480
	panelMinHeightDIP       = 560
)

// closePanelWindow closes any visible GBF-Dock control panel windows after persisting non-minimized bounds.
func closePanelWindow(titlePrefix string) {
	closeNativePanel()
	if titlePrefix == "" {
		return
	}
	closed := make(map[uintptr]bool)
	for i := 0; i < 8; i++ {
		hwnd := findPanelWindow(titlePrefix)
		if hwnd == 0 {
			break
		}
		if !closed[hwnd] {
			closed[hwnd] = true
			if iconic, _, _ := procIsIconic.Call(hwnd); iconic == 0 {
				if zoomed, _, _ := procIsZoomed.Call(hwnd); zoomed != 0 {
					if wb, ok := loadWindowBounds(); ok && !wb.Maximized {
						wb.Maximized = true
						saveWindowBounds(wb)
					}
				} else {
					var rc winRect
					if r, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rc))); r != 0 {
						w, h := rc.Right-rc.Left, rc.Bottom-rc.Top
						if w >= 400 && h >= 320 {
							saveWindowBounds(windowBounds{X: rc.Left, Y: rc.Top, Width: w, Height: h, Maximized: false})
						}
					}
				}
			}
			procPostMessageW.Call(hwnd, wmClose, 0, 0)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// openRootStore 打开“当前用户 → 受信任的根证书颁发机构”。不需要管理员权限。
func openRootStore() (syscall.Handle, error) {
	name, _ := syscall.UTF16PtrFromString("ROOT")
	return syscall.CertOpenSystemStore(0, name)
}

// installCert 把根证书装进当前用户的受信任根证书库；Windows 会弹出安全警告让用户确认。
func installCert(der []byte) error {
	store, err := openRootStore()
	if err != nil {
		return err
	}
	defer syscall.CertCloseStore(store, 0)
	ctx, err := syscall.CertCreateCertificateContext(syscall.X509_ASN_ENCODING|syscall.PKCS_7_ASN_ENCODING, &der[0], uint32(len(der)))
	if err != nil {
		return err
	}
	defer syscall.CertFreeCertificateContext(ctx)
	if err := syscall.CertAddCertificateContextToStore(store, ctx, certStoreAddUseExisting, nil); err != nil {
		return fmt.Errorf("证书没有安装（%v），弹出确认窗口时请选择“是”", err)
	}
	return nil
}

// eachRootCert 遍历当前用户根证书库，fn 返回 false 时停止。
func eachRootCert(fn func(ctx *syscall.CertContext, der []byte) bool) error {
	store, err := openRootStore()
	if err != nil {
		return err
	}
	defer syscall.CertCloseStore(store, 0)
	var ctx *syscall.CertContext
	for {
		// 传入上一个 ctx，Windows 会自动释放它；遍历结束时返回 nil
		if ctx, err = syscall.CertEnumCertificatesInStore(store, ctx); ctx == nil {
			return nil
		}
		if !fn(ctx, append([]byte(nil), unsafe.Slice(ctx.EncodedCert, ctx.Length)...)) {
			syscall.CertFreeCertificateContext(ctx) // 提前结束时要自己释放当前这一个
			return nil
		}
	}
}

func countCerts(match func(der []byte) bool) int {
	n := 0
	eachRootCert(func(_ *syscall.CertContext, der []byte) bool {
		if match(der) {
			n++
		}
		return true
	})
	return n
}

// removeCerts 删除匹配的根证书；Windows 会对每一张弹窗确认。
func removeCerts(match func(der []byte) bool) (int, error) {
	n := 0
	var failed error
	err := eachRootCert(func(ctx *syscall.CertContext, der []byte) bool {
		if !match(der) {
			return true
		}
		// 删除会释放传入的上下文，所以删副本，原件留着继续遍历
		dup, _, _ := procCertDuplicate.Call(uintptr(unsafe.Pointer(ctx)))
		if r, _, e := procCertDelete.Call(dup); r == 0 {
			failed = fmt.Errorf("证书没有删除（%v），弹出确认窗口时请选择“是”", e)
			return false
		}
		n++
		return true
	})
	if err != nil {
		return n, err
	}
	return n, failed
}

// shellOpen 用 Windows 的“打开”动作启动程序、网址或文件夹，和在资源管理器里双击一样。
func shellOpen(file, args string) error {
	errc := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		// 微软建议调用 ShellExecute 前先初始化 COM（APARTMENTTHREADED | DISABLE_OLE1DDE）
		if hr, _, _ := procCoInitializeEx.Call(0, 0x2|0x4); int32(hr) >= 0 {
			defer procCoUninitialize.Call()
		}
		verb, _ := syscall.UTF16PtrFromString("open")
		f, _ := syscall.UTF16PtrFromString(file)
		var a *uint16
		if args != "" {
			a, _ = syscall.UTF16PtrFromString(args)
		}
		r, _, _ := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(f)),
			uintptr(unsafe.Pointer(a)), 0, 1) // SW_SHOWNORMAL
		if r <= 32 {
			errc <- fmt.Errorf("无法打开 %s（错误码 %d）", file, r)
			return
		}
		errc <- nil
	}()
	return <-errc
}

func openURL(u string) error      { return shellOpen(u, "") }
func openFolder(dir string) error { return shellOpen(dir, "") }

var (
	enumFind struct {
		mu     sync.Mutex
		prefix string
		found  uintptr
	}
	enumCallbackOnce sync.Once
	enumCallbackPtr  uintptr

	launchMu      sync.Mutex
	lastLaunch    time.Time
	trackerMu     sync.Mutex
	trackerActive bool
)

type windowBounds struct {
	X         int32 `json:"x"`
	Y         int32 `json:"y"`
	Width     int32 `json:"width"`
	Height    int32 `json:"height"`
	Maximized bool  `json:"maximized"`
}

func windowBoundsPath() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		exe, _ := os.Executable()
		return filepath.Join(filepath.Dir(exe), "window.json")
	}
	return filepath.Join(base, appConfigDirName, "window.json")
}

func loadWindowBounds() (windowBounds, bool) {
	b, err := os.ReadFile(windowBoundsPath())
	if err != nil {
		return windowBounds{}, false
	}
	var wb windowBounds
	if json.Unmarshal(b, &wb) != nil || wb.Width < 320 || wb.Height < 240 {
		return windowBounds{}, false
	}
	return wb, true
}

func saveWindowBounds(wb windowBounds) {
	if wb.Width < 400 || wb.Height < 320 {
		return
	}
	p := windowBoundsPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	if data, err := json.MarshalIndent(wb, "", "  "); err == nil {
		_ = os.WriteFile(p, data, 0o600)
	}
}

func systemDPIScale() float64 {
	if procGetDpiForSystem.Find() == nil {
		if d, _, _ := procGetDpiForSystem.Call(); d >= 96 {
			return float64(d) / 96.0
		}
	}
	return 1.0
}

func windowDPIScale(hwnd uintptr) float64 {
	if hwnd != 0 && procGetDpiForWindow.Find() == nil {
		if d, _, _ := procGetDpiForWindow.Call(hwnd); d >= 96 {
			return float64(d) / 96.0
		}
	}
	return systemDPIScale()
}

type guiThreadInfo struct {
	CbSize        uint32
	Flags         uint32
	HwndActive    uintptr
	HwndFocus     uintptr
	HwndCapture   uintptr
	HwndMenuOwner uintptr
	HwndMoveSize  uintptr
	HwndCaret     uintptr
	RcCaret       winRect
}

func isWindowMovingOrResizing(hwnd uintptr) bool {
	if s, _, _ := procGetAsyncKeyState.Call(vkLButton); (s & 0x8000) != 0 {
		return true
	}
	if s, _, _ := procGetAsyncKeyState.Call(vkRButton); (s & 0x8000) != 0 {
		return true
	}
	if hwnd != 0 && procGetWindowThreadProcessId.Find() == nil && procGetGUIThreadInfo.Find() == nil {
		tid, _, _ := procGetWindowThreadProcessId.Call(hwnd, 0)
		if tid != 0 {
			var gti guiThreadInfo
			gti.CbSize = uint32(unsafe.Sizeof(gti))
			if r, _, _ := procGetGUIThreadInfo.Call(tid, uintptr(unsafe.Pointer(&gti))); r != 0 {
				if (gti.Flags&guiInMoveSize) != 0 || gti.HwndMoveSize != 0 {
					return true
				}
			}
		}
	}
	return false
}

func clampBoundsToWorkArea(wb windowBounds, hasSaved bool) windowBounds {
	scale := systemDPIScale()
	calcMin := func(workW, workH int32) (int32, int32) {
		minW := int32(float64(panelMinWidthDIP) * scale)
		minH := int32(float64(panelMinHeightDIP) * scale)
		if workW > 0 && minW > workW {
			minW = workW
		}
		if workH > 0 && minH > workH {
			minH = workH
		}
		return minW, minH
	}

	if !hasSaved {
		var pt winPoint
		procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
		work := workAreaForPoint(pt)
		workW := work.Right - work.Left
		workH := work.Bottom - work.Top
		w := int32(1160 * scale)
		h := int32(760 * scale)
		if workW > 0 && w > workW*9/10 {
			w = workW * 9 / 10
		}
		if workH > 0 && h > workH*9/10 {
			h = workH * 9 / 10
		}
		minW, minH := calcMin(workW, workH)
		if w < minW {
			w = minW
		}
		if h < minH {
			h = minH
		}
		return windowBounds{
			X:      work.Left + (workW-w)/2,
			Y:      work.Top + (workH-h)/2,
			Width:  w,
			Height: h,
		}
	}

	center := winPoint{X: wb.X + wb.Width/2, Y: wb.Y + wb.Height/2}
	work := workAreaForPoint(center)
	workW := work.Right - work.Left
	workH := work.Bottom - work.Top
	minW, minH := calcMin(workW, workH)
	if wb.Width < minW {
		wb.Width = minW
	}
	if wb.Height < minH {
		wb.Height = minH
	}
	if workW > 0 && wb.Width > workW {
		wb.Width = workW
	}
	if workH > 0 && wb.Height > workH {
		wb.Height = workH
	}
	if wb.X < work.Left {
		wb.X = work.Left
	}
	if wb.Y < work.Top {
		wb.Y = work.Top
	}
	if wb.X+wb.Width > work.Right {
		wb.X = work.Right - wb.Width
	}
	if wb.Y+wb.Height > work.Bottom {
		wb.Y = work.Bottom - wb.Height
	}
	return wb
}

func findPanelWindow(titlePrefix string) uintptr {
	enumCallbackOnce.Do(func() {
		enumCallbackPtr = syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
			if vis, _, _ := procIsWindowVisible.Call(hwnd); vis == 0 {
				return 1
			}
			n, _, _ := procGetWindowTextLenW.Call(hwnd)
			if n == 0 || n > 512 {
				return 1
			}
			buf := make([]uint16, n+1)
			procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), n+1)
			title := syscall.UTF16ToString(buf)
			if strings.Contains(title, enumFind.prefix) {
				enumFind.found = hwnd
				return 0
			}
			return 1
		})
	})

	enumFind.mu.Lock()
	enumFind.prefix = titlePrefix
	enumFind.found = 0
	procEnumWindows.Call(enumCallbackPtr, 0)
	hwnd := enumFind.found
	enumFind.mu.Unlock()
	return hwnd
}

func focusExistingWindow(titlePrefix string) bool {
	if focusNativePanel() {
		return true
	}
	hwnd := findPanelWindow(titlePrefix)
	if hwnd == 0 {
		return false
	}
	if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
		procShowWindow.Call(hwnd, swRestore)
	}
	procBringWindowToTop.Call(hwnd)
	procSetForegroundWindow.Call(hwnd)
	return true
}

func applyWindowBounds(hwnd uintptr, wb windowBounds) {
	if wb.Maximized {
		procSetWindowPos.Call(hwnd, 0, uintptr(wb.X), uintptr(wb.Y), uintptr(wb.Width), uintptr(wb.Height), swpNoZOrder|swpNoActivate)
		procShowWindow.Call(hwnd, swMaximize)
		return
	}
	procSetWindowPos.Call(hwnd, 0, uintptr(wb.X), uintptr(wb.Y), uintptr(wb.Width), uintptr(wb.Height), swpNoZOrder|swpNoActivate)
}

func absDiff32(a, b int32) int32 {
	if a > b {
		return a - b
	}
	return b - a
}

func startWindowTracker(initial windowBounds, applyInitial bool) {
	trackerMu.Lock()
	if trackerActive {
		trackerMu.Unlock()
		return
	}
	trackerActive = true
	trackerMu.Unlock()

	go func() {
		defer func() {
			trackerMu.Lock()
			trackerActive = false
			trackerMu.Unlock()
		}()

		var hwnd uintptr
		for i := 0; i < 140; i++ { // 最多等待约 4.2 秒让浏览器拉起并设置标题
			if hwnd = findPanelWindow(panelTitle); hwnd != 0 {
				break
			}
			time.Sleep(30 * time.Millisecond)
		}
		if hwnd == 0 {
			return
		}

		cur := initial
		if applyInitial {
			applyWindowBounds(hwnd, initial)
			time.Sleep(140 * time.Millisecond)
			if !initial.Maximized {
				var rc winRect
				if r, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rc))); r != 0 {
					if absDiff32(rc.Left, initial.X) > 10 || absDiff32(rc.Top, initial.Y) > 10 ||
						absDiff32(rc.Right-rc.Left, initial.Width) > 10 || absDiff32(rc.Bottom-rc.Top, initial.Height) > 10 {
						applyWindowBounds(hwnd, initial)
					}
				}
			}
		}

		lastSaved := cur
		var lastSample winRect
		hasSample := false
		stableTicks := 0

		for {
			time.Sleep(120 * time.Millisecond)
			if alive, _, _ := procIsWindow.Call(hwnd); alive == 0 {
				saveWindowBounds(cur)
				return
			}
			if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
				stableTicks = 0
				hasSample = false
				continue
			}
			zoomed, _, _ := procIsZoomed.Call(hwnd)
			if zoomed != 0 {
				stableTicks = 0
				hasSample = false
				if !cur.Maximized {
					cur.Maximized = true
					if cur != lastSaved {
						saveWindowBounds(cur)
						lastSaved = cur
					}
				}
				continue
			}

			// 用户仍在按下鼠标拖拽或系统处于 MoveSize 调整环时绝不介入，避免窗口反复弹跳卡顿
			if isWindowMovingOrResizing(hwnd) {
				stableTicks = 0
				continue
			}

			var rc winRect
			if r, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rc))); r == 0 {
				continue
			}
			if !hasSample || rc != lastSample {
				lastSample = rc
				hasSample = true
				stableTicks = 0
				continue
			}
			stableTicks++
			if stableTicks != 2 {
				continue
			}

			w := rc.Right - rc.Left
			h := rc.Bottom - rc.Top
			if w <= 0 || h <= 0 {
				continue
			}
			scale := windowDPIScale(hwnd)
			minW := int32(float64(panelMinWidthDIP) * scale)
			minH := int32(float64(panelMinHeightDIP) * scale)
			work := workAreaForPoint(winPoint{X: rc.Left + w/2, Y: rc.Top + h/2})
			workW := work.Right - work.Left
			workH := work.Bottom - work.Top
			if workW > 0 && minW > workW {
				minW = workW
			}
			if workH > 0 && minH > workH {
				minH = workH
			}
			if w < minW || h < minH {
				newW, newH := w, h
				newX, newY := rc.Left, rc.Top
				if newW < minW {
					newW = minW
					if cur.Width > 0 && absDiff32(rc.Right, cur.X+cur.Width) <= 8 && rc.Left > cur.X {
						newX = rc.Right - newW
					}
				}
				if newH < minH {
					newH = minH
					if cur.Height > 0 && absDiff32(rc.Bottom, cur.Y+cur.Height) <= 8 && rc.Top > cur.Y {
						newY = rc.Bottom - newH
					}
				}
				if workW > 0 && newW > workW {
					newW = workW
				}
				if workH > 0 && newH > workH {
					newH = workH
				}
				if workW > 0 {
					if newX < work.Left {
						newX = work.Left
					}
					if newX+newW > work.Right {
						newX = work.Right - newW
					}
				}
				if workH > 0 {
					if newY < work.Top {
						newY = work.Top
					}
					if newY+newH > work.Bottom {
						newY = work.Bottom - newH
					}
				}
				procSetWindowPos.Call(hwnd, 0, uintptr(newX), uintptr(newY), uintptr(newW), uintptr(newH), swpNoZOrder|swpNoActivate)
				lastSample = winRect{Left: newX, Top: newY, Right: newX + newW, Bottom: newY + newH}
				cur = windowBounds{X: newX, Y: newY, Width: newW, Height: newH, Maximized: false}
				if cur != lastSaved {
					saveWindowBounds(cur)
					lastSaved = cur
				}
				continue
			}

			next := windowBounds{X: rc.Left, Y: rc.Top, Width: w, Height: h, Maximized: false}
			if next != cur || cur != lastSaved {
				cur = next
				saveWindowBounds(cur)
				lastSaved = cur
			}
		}
	}()
}

// launchWindow 严格保证同一时间只打开一个控制面板窗口：优先使用原生 Win32 + WebView2 宿主窗口（硬性锁死最小尺寸无回弹），不可用时自动回退至浏览器应用模式。
func launchWindow(url string) error {
	launchMu.Lock()
	defer launchMu.Unlock()

	saved, hasSaved := loadWindowBounds()
	wb := clampBoundsToWorkArea(saved, hasSaved)

	if focusExistingWindow(panelTitle) {
		if hwnd := findPanelWindow(panelTitle); hwnd != 0 && !focusNativePanel() {
			startWindowTracker(wb, false)
		}
		return nil
	}
	if time.Since(lastLaunch) < 1500*time.Millisecond {
		return nil
	}
	lastLaunch = time.Now()

	if err := launchNativePanel(url, wb); err == nil {
		return nil
	} else {
		log.Printf("原生 WebView2 控制面板不可用（%v），自动回退浏览器应用模式", err)
	}

	scale := systemDPIScale()
	dipX := int32(float64(wb.X) / scale)
	dipY := int32(float64(wb.Y) / scale)
	dipW := int32(float64(wb.Width) / scale)
	dipH := int32(float64(wb.Height) / scale)
	panelDir := filepath.Join(filepath.Dir(windowBoundsPath()), "panel-profile")
	_ = os.MkdirAll(panelDir, 0o755)
	args := fmt.Sprintf(`--user-data-dir="%s" --no-first-run --no-default-browser-check --app=%s --window-position=%d,%d --window-size=%d,%d`,
		panelDir, url, dipX, dipY, dipW, dipH)

	if shellOpen("msedge", args) == nil {
		startWindowTracker(wb, true)
		return nil
	}
	if shellOpen("chrome", args) == nil {
		startWindowTracker(wb, true)
		return nil
	}
	return openURL(url)
}

// 系统代理设置通过 WinINet 的 per-connection 接口读写。
var (
	wininet         = syscall.NewLazyDLL("wininet.dll")
	procSetOption   = wininet.NewProc("InternetSetOptionW")
	procQueryOption = wininet.NewProc("InternetQueryOptionW")
	kernel32        = syscall.NewLazyDLL("kernel32.dll")
	procLstrlenW    = kernel32.NewProc("lstrlenW")
	procMoveMemory  = kernel32.NewProc("RtlMoveMemory")
	procGlobalFree  = kernel32.NewProc("GlobalFree")
)

const (
	optPerConnection   = 75 // INTERNET_OPTION_PER_CONNECTION_OPTION
	optSettingsChanged = 95 // INTERNET_OPTION_PROXY_SETTINGS_CHANGED
	optRefresh         = 37 // INTERNET_OPTION_REFRESH

	connFlags         = 1 // INTERNET_PER_CONN_FLAGS
	connProxyServer   = 2 // INTERNET_PER_CONN_PROXY_SERVER
	connProxyBypass   = 3 // INTERNET_PER_CONN_PROXY_BYPASS
	connAutoConfigURL = 4 // INTERNET_PER_CONN_AUTOCONFIG_URL

	proxyDirect  = 0x1 // PROXY_TYPE_DIRECT
	proxyManual  = 0x2 // PROXY_TYPE_PROXY
	proxyAutoURL = 0x4 // PROXY_TYPE_AUTO_PROXY_URL
)

// INTERNET_PER_CONN_OPTIONW（64 位布局：DWORD + 8 字节对齐的 union）
type perConnOption struct {
	option uint32
	_      uint32
	value  uintptr // DWORD 或 LPWSTR
}

// INTERNET_PER_CONN_OPTION_LISTW
type perConnOptionList struct {
	size       uint32
	connection *uint16 // nil = 默认的局域网连接
	count      uint32
	optionErr  uint32
	options    *perConnOption
}

type proxyBackup struct {
	Flags       uint32 `json:"flags"`
	ProxyServer string `json:"proxyServer"`
	ProxyBypass string `json:"proxyBypass"`
	AutoConfig  string `json:"autoConfig"`
}

func queryFullProxy() (flags uint32, server, bypass, pac string, err error) {
	opts := [4]perConnOption{
		{option: connFlags},
		{option: connProxyServer},
		{option: connProxyBypass},
		{option: connAutoConfigURL},
	}
	list := perConnOptionList{count: uint32(len(opts)), options: &opts[0]}
	list.size = uint32(unsafe.Sizeof(list))
	size := list.size
	if r, _, e := procQueryOption.Call(0, optPerConnection, uintptr(unsafe.Pointer(&list)), uintptr(unsafe.Pointer(&size))); r == 0 {
		return 0, "", "", "", fmt.Errorf("读取系统代理设置失败：%v", e)
	}
	return uint32(opts[0].value), takeString(opts[1].value), takeString(opts[2].value), takeString(opts[3].value), nil
}

// takeString 复制 WinINet 分配的字符串并释放它。
func takeString(p uintptr) string {
	if p == 0 {
		return ""
	}
	defer procGlobalFree.Call(p)
	n, _, _ := procLstrlenW.Call(p)
	buf := make([]uint16, n+1)
	procMoveMemory.Call(uintptr(unsafe.Pointer(&buf[0])), p, n*2)
	return syscall.UTF16ToString(buf)
}

func setFullProxy(flags uint32, server, bypass, pac string) error {
	srvPtr, err := syscall.UTF16PtrFromString(server)
	if err != nil {
		return err
	}
	bypPtr, err := syscall.UTF16PtrFromString(bypass)
	if err != nil {
		return err
	}
	pacPtr, err := syscall.UTF16PtrFromString(pac)
	if err != nil {
		return err
	}
	opts := [4]perConnOption{
		{option: connFlags, value: uintptr(flags)},
		{option: connProxyServer, value: uintptr(unsafe.Pointer(srvPtr))},
		{option: connProxyBypass, value: uintptr(unsafe.Pointer(bypPtr))},
		{option: connAutoConfigURL, value: uintptr(unsafe.Pointer(pacPtr))},
	}
	list := perConnOptionList{count: uint32(len(opts)), options: &opts[0]}
	list.size = uint32(unsafe.Sizeof(list))
	r, _, e := procSetOption.Call(0, optPerConnection, uintptr(unsafe.Pointer(&list)), uintptr(list.size))
	runtime.KeepAlive(srvPtr)
	runtime.KeepAlive(bypPtr)
	runtime.KeepAlive(pacPtr)
	if r == 0 {
		return fmt.Errorf("修改系统代理设置失败：%v", e)
	}
	procSetOption.Call(0, optSettingsChanged, 0, 0) // 通知浏览器和桌面软件立即生效
	procSetOption.Call(0, optRefresh, 0, 0)
	return nil
}

func isProxyServerSelf(server, listenAddr string) bool {
	s := strings.TrimSpace(server)
	if s == "" || listenAddr == "" {
		return false
	}
	if !strings.Contains(s, "=") {
		return isSelfHostPort(s, listenAddr)
	}
	for _, part := range strings.Split(s, ";") {
		if _, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok && v != "" {
			if isSelfHostPort(v, listenAddr) {
				return true
			}
		}
	}
	return false
}

func pacState() (pac, manualProxy string) {
	flags, server, _, urlStr, err := queryFullProxy()
	if err != nil {
		log.Print(err)
		return "", ""
	}
	if flags&proxyAutoURL != 0 {
		pac = urlStr
	}
	if flags&proxyManual != 0 {
		manualProxy = server
	}
	return pac, manualProxy
}

func loadProxyBackup(backupPath, listenAddr string) (proxyBackup, bool) {
	b, err := os.ReadFile(backupPath)
	if err != nil {
		return proxyBackup{}, false
	}
	var bk proxyBackup
	if json.Unmarshal(b, &bk) != nil {
		return proxyBackup{}, false
	}
	if isProxyServerSelf(bk.ProxyServer, listenAddr) {
		return proxyBackup{}, false
	}
	return bk, true
}

func savedBackupServer(backupPath, listenAddr string) string {
	bk, ok := loadProxyBackup(backupPath, listenAddr)
	if !ok || bk.Flags&proxyManual == 0 {
		return ""
	}
	return bk.ProxyServer
}

func savedBackupPAC(backupPath, listenAddr, ownPACURL string) string {
	bk, ok := loadProxyBackup(backupPath, listenAddr)
	if !ok || bk.Flags&proxyAutoURL == 0 || bk.AutoConfig == "" || bk.AutoConfig == ownPACURL {
		return ""
	}
	return bk.AutoConfig
}

func proxyState(listenAddr, pacURL, backupPath string) (pac, manualProxy string, enabled, legacyPAC bool, savedUpstream string) {
	flags, server, _, urlStr, err := queryFullProxy()
	if err != nil {
		log.Print(err)
		return "", "", false, false, ""
	}
	if flags&proxyAutoURL != 0 {
		pac = urlStr
		legacyPAC = urlStr == pacURL
	}
	if flags&proxyManual != 0 {
		manualProxy = server
		enabled = isProxyServerSelf(server, listenAddr)
	}
	savedUpstream = savedBackupServer(backupPath, listenAddr)
	return pac, manualProxy, enabled, legacyPAC, savedUpstream
}

// enableSystemProxy 备份原 Windows 代理并启用手动代理串联（彻底不使用 PAC，确保桌面端 Gemini 正常联网）。
func enableSystemProxy(listenAddr, pacURL, backupPath string) error {
	flags, server, bypass, pac, err := queryFullProxy()
	if err != nil {
		return err
	}
	if !isProxyServerSelf(server, listenAddr) {
		bk := proxyBackup{
			Flags:       flags,
			ProxyServer: server,
			ProxyBypass: bypass,
			AutoConfig:  pac,
		}
		if bk.AutoConfig == pacURL {
			bk.AutoConfig = ""
			bk.Flags &^= proxyAutoURL
		}
		if err := os.MkdirAll(filepath.Dir(backupPath), 0o700); err == nil {
			if data, mErr := json.MarshalIndent(bk, "", "  "); mErr == nil {
				_ = os.WriteFile(backupPath, data, 0o600)
			}
		}
	}
	newBypass := strings.TrimSpace(bypass)
	if newBypass == "" {
		newBypass = defaultProxyBypass
	}
	return setFullProxy(proxyDirect|proxyManual, listenAddr, newBypass, "")
}

// disableSystemProxy 仅在当前仍为本程序接管（或残留旧版 PAC）时恢复原系统代理，不覆盖第三方代理软件之后做的修改。
func disableSystemProxy(listenAddr, pacURL, backupPath, fallbackUpstream string) error {
	flags, server, _, pac, err := queryFullProxy()
	if err != nil {
		return err
	}
	ourManual := (flags&proxyManual != 0) && isProxyServerSelf(server, listenAddr)
	ourPAC := (flags&proxyAutoURL != 0) && pac == pacURL
	if !ourManual && !ourPAC {
		_ = os.Remove(backupPath)
		return nil
	}
	return restoreProxyFromBackup(listenAddr, pacURL, backupPath, fallbackUpstream)
}

func resetSystemProxy(listenAddr, pacURL, backupPath, fallbackUpstream string) error {
	return restoreProxyFromBackup(listenAddr, pacURL, backupPath, fallbackUpstream)
}

func restoreProxyFromBackup(listenAddr, pacURL, backupPath, fallbackUpstream string) error {
	defer os.Remove(backupPath)
	if bk, ok := loadProxyBackup(backupPath, listenAddr); ok {
		if bk.AutoConfig == pacURL {
			bk.AutoConfig = ""
			bk.Flags &^= proxyAutoURL
		}
		if bk.Flags&proxyManual != 0 && bk.ProxyServer != "" {
			if ex := extractUpstreamURL(bk.ProxyServer); ex != "" {
				if u, err := url.Parse(ex); err == nil && u.Host != "" && isLoopbackHost(u.Hostname()) && !isTCPReachable(u.Host, 150*time.Millisecond) {
					bk.Flags = (bk.Flags &^ proxyManual) | proxyDirect
					bk.ProxyServer = ""
				}
			}
		}
		if bk.Flags == 0 {
			bk.Flags = proxyDirect
		}
		return setFullProxy(bk.Flags, bk.ProxyServer, bk.ProxyBypass, bk.AutoConfig)
	}
	if hostPort := reachableUpstreamHostPort(fallbackUpstream); hostPort != "" {
		return setFullProxy(proxyDirect|proxyManual, hostPort, defaultProxyBypass, "")
	}
	return setFullProxy(proxyDirect, "", "", "")
}

func reachableUpstreamHostPort(upstream string) string {
	if upstream == "" {
		return ""
	}
	u, err := url.Parse(upstream)
	if err != nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	c, err := net.DialTimeout("tcp", u.Host, 150*time.Millisecond)
	if err != nil {
		return ""
	}
	c.Close()
	return u.Host
}

// fatal 图形版没有控制台，用消息框显示致命错误。
func fatal(msg string) {
	log.Print(msg)
	title, _ := syscall.UTF16PtrFromString(appName + " v" + appVersion)
	text, _ := syscall.UTF16PtrFromString(msg)
	user32.NewProc("MessageBoxW").Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), 0x10) // MB_ICONERROR
	os.Exit(1)
}

type winGUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	clsidFileOpenDialog = winGUID{0xDC1C5A9C, 0xE88A, 0x4DDE, [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7}}
	iidIFileOpenDialog  = winGUID{0xD57C7288, 0xD4AD, 0x4768, [8]byte{0xBE, 0x02, 0x9D, 0x96, 0x95, 0x32, 0xD9, 0x60}}
)

type comObj struct {
	vtbl *[24]uintptr
}

// pickFolderDialog 调用 Windows 原生 COM IFileOpenDialog 选择文件夹（STA 线程隔离）。
func pickFolderDialog(title string) (string, bool, error) {
	type res struct {
		path      string
		cancelled bool
		err       error
	}
	ch := make(chan res, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		hr, _, _ := procCoInitializeEx.Call(0, 0x2|0x4) // COINIT_APARTMENTTHREADED | COINIT_DISABLE_OLE1DDE
		if int32(hr) < 0 && uint32(hr) != 0x80010106 {  // RPC_E_CHANGED_MODE
			ch <- res{err: fmt.Errorf("初始化文件对话框失败（0x%08X）", uint32(hr))}
			return
		}
		if int32(hr) >= 0 {
			defer procCoUninitialize.Call()
		}

		var dlg *comObj
		hr, _, _ = procCoCreateInstance.Call(
			uintptr(unsafe.Pointer(&clsidFileOpenDialog)),
			0,
			0x17, // CLSCTX_ALL
			uintptr(unsafe.Pointer(&iidIFileOpenDialog)),
			uintptr(unsafe.Pointer(&dlg)),
		)
		if int32(hr) < 0 || dlg == nil {
			ch <- res{err: fmt.Errorf("无法创建文件夹选择框（0x%08X）", uint32(hr))}
			return
		}
		dlgPtr := uintptr(unsafe.Pointer(dlg))
		defer syscall.SyscallN(dlg.vtbl[2], dlgPtr) // IUnknown::Release

		var opts uint32
		syscall.SyscallN(dlg.vtbl[10], dlgPtr, uintptr(unsafe.Pointer(&opts))) // GetOptions
		opts |= 0x20 | 0x40 | 0x800                                             // FOS_PICKFOLDERS | FOS_FORCEFILESYSTEM | FOS_PATHMUSTEXIST
		syscall.SyscallN(dlg.vtbl[9], dlgPtr, uintptr(opts))                    // SetOptions

		if strings.TrimSpace(title) != "" {
			tPtr, _ := syscall.UTF16PtrFromString(title)
			syscall.SyscallN(dlg.vtbl[17], dlgPtr, uintptr(unsafe.Pointer(tPtr))) // SetTitle
			runtime.KeepAlive(tPtr)
		}

		owner := findPanelWindow(panelTitle)
		showHR, _, _ := syscall.SyscallN(dlg.vtbl[3], dlgPtr, owner) // IModalWindow::Show
		if uint32(showHR) == 0x800704C7 {                            // HRESULT_FROM_WIN32(ERROR_CANCELLED)
			ch <- res{cancelled: true}
			return
		}
		if int32(showHR) < 0 {
			ch <- res{err: fmt.Errorf("文件夹选择框打开失败（0x%08X）", uint32(showHR))}
			return
		}

		var item *comObj
		resHR, _, _ := syscall.SyscallN(dlg.vtbl[20], dlgPtr, uintptr(unsafe.Pointer(&item))) // GetResult
		if int32(resHR) < 0 || item == nil {
			ch <- res{err: fmt.Errorf("读取所选文件夹失败（0x%08X）", uint32(resHR))}
			return
		}
		itemPtr := uintptr(unsafe.Pointer(item))
		defer syscall.SyscallN(item.vtbl[2], itemPtr) // IShellItem::Release

		var pszPath uintptr
		dnHR, _, _ := syscall.SyscallN(item.vtbl[5], itemPtr, 0x80058000, uintptr(unsafe.Pointer(&pszPath))) // SIGDN_FILESYSPATH
		if int32(dnHR) < 0 || pszPath == 0 {
			ch <- res{err: fmt.Errorf("解析文件夹路径失败（0x%08X）", uint32(dnHR))}
			return
		}
		defer procCoTaskMemFree.Call(pszPath)

		n, _, _ := procLstrlenW.Call(pszPath)
		buf := make([]uint16, n+1)
		if n > 0 {
			procMoveMemory.Call(uintptr(unsafe.Pointer(&buf[0])), pszPath, n*2)
		}
		ch <- res{path: syscall.UTF16ToString(buf)}
	}()
	r := <-ch
	return r.path, r.cancelled, r.err
}

func userCommonFolders() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	var dirs []string
	for _, sub := range []string{"Downloads", "Desktop", "Documents"} {
		p := filepath.Join(home, sub)
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			dirs = append(dirs, p)
		}
	}
	return dirs
}

const (
	autoStartRegSubkey       = `Software\Microsoft\Windows\CurrentVersion\Run`
	autoStartValueName       = "GBF-Dock"
	legacyAutoStartValueName = "GBFCache"
	regKeyWriteAccess        = 0x20006
	regSZType                = 1
)

func parseCommandExePath(cmdLine string) string {
	s := strings.TrimSpace(cmdLine)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, `"`) {
		if end := strings.IndexByte(s[1:], '"'); end >= 0 {
			return s[1 : 1+end]
		}
	}
	if idx := strings.IndexAny(s, " \t"); idx >= 0 {
		return s[:idx]
	}
	return s
}

func getAutoStart() bool {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return false
	}
	exe = filepath.Clean(exe)
	subkey, _ := syscall.UTF16PtrFromString(autoStartRegSubkey)
	var hk syscall.Handle
	if syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, subkey, 0, syscall.KEY_READ, &hk) != nil {
		return false
	}
	defer syscall.RegCloseKey(hk)

	valName, _ := syscall.UTF16PtrFromString(autoStartValueName)
	var typ, size uint32
	if syscall.RegQueryValueEx(hk, valName, nil, &typ, nil, &size) != nil || typ != regSZType || size < 2 {
		return false
	}
	buf := make([]uint16, size/2+1)
	if syscall.RegQueryValueEx(hk, valName, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &size) != nil {
		return false
	}
	target := parseCommandExePath(syscall.UTF16ToString(buf))
	if target == "" {
		return false
	}
	return strings.EqualFold(filepath.Clean(target), exe)
}

func setAutoStart(enable bool) error {
	subkey, _ := syscall.UTF16PtrFromString(autoStartRegSubkey)
	var hk syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, subkey, 0, regKeyWriteAccess, &hk); err != nil {
		return fmt.Errorf("无法打开开机自启注册表项：%w", err)
	}
	defer syscall.RegCloseKey(hk)

	valName, _ := syscall.UTF16PtrFromString(autoStartValueName)
	legacyValName, _ := syscall.UTF16PtrFromString(legacyAutoStartValueName)
	if !enable {
		ret, _, _ := procRegDeleteValueW.Call(uintptr(hk), uintptr(unsafe.Pointer(valName)))
		runtime.KeepAlive(valName)
		procRegDeleteValueW.Call(uintptr(hk), uintptr(unsafe.Pointer(legacyValName)))
		runtime.KeepAlive(legacyValName)
		if ret != 0 && ret != 2 { // 0=ERROR_SUCCESS, 2=ERROR_FILE_NOT_FOUND
			return fmt.Errorf("关闭开机自启失败（错误码 %d）", ret)
		}
		return nil
	}

	exe, err := os.Executable()
	if err != nil || exe == "" {
		return fmt.Errorf("无法获取程序路径：%w", err)
	}
	cmd := fmt.Sprintf(`"%s" -minimized`, filepath.Clean(exe))
	u, _ := syscall.UTF16FromString(cmd)
	ret, _, _ := procRegSetValueExW.Call(
		uintptr(hk),
		uintptr(unsafe.Pointer(valName)),
		0,
		regSZType,
		uintptr(unsafe.Pointer(&u[0])),
		uintptr(len(u)*2),
	)
	runtime.KeepAlive(valName)
	runtime.KeepAlive(u)
	if ret != 0 {
		return fmt.Errorf("写入开机自启失败（错误码 %d）", ret)
	}
	procRegDeleteValueW.Call(uintptr(hk), uintptr(unsafe.Pointer(legacyValName)))
	runtime.KeepAlive(legacyValName)
	return nil
}
