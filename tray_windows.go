//go:build windows

package main

import (
	"errors"
	"fmt"
	"log"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// 系统托盘图标与 Win11 悬浮状态卡片（Flyout）：
// - 单击托盘图标：弹出小框体实时显示「缓存读取率」与「已缓存空间」，鼠标移开自动消失。
// - 双击托盘图标：拦截重复点击事件，严格只弹出并前置一个控制面板窗口。
// - 右键托盘图标：弹出菜单（打开控制面板 / 启动游戏 / 退出 GBF-Dock）。

var (
	user32                  = syscall.NewLazyDLL("user32.dll")
	shell32                 = syscall.NewLazyDLL("shell32.dll")
	gdi32                   = syscall.NewLazyDLL("gdi32.dll")
	dwmapi                  = syscall.NewLazyDLL("dwmapi.dll")
	procRegisterClassExW    = user32.NewProc("RegisterClassExW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procRegisterWindowMsgW  = user32.NewProc("RegisterWindowMessageW")
	procLoadImageW          = user32.NewProc("LoadImageW")
	procLoadCursorW         = user32.NewProc("LoadCursorW")
	procGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procAppendMenuW         = user32.NewProc("AppendMenuW")
	procSetMenuDefaultItem  = user32.NewProc("SetMenuDefaultItem")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procPostMessageW        = user32.NewProc("PostMessageW")
	procSetTimer            = user32.NewProc("SetTimer")
	procKillTimer           = user32.NewProc("KillTimer")
	procGetWindowRect       = user32.NewProc("GetWindowRect")
	procSetWindowPos        = user32.NewProc("SetWindowPos")
	procInvalidateRect      = user32.NewProc("InvalidateRect")
	procBeginPaint          = user32.NewProc("BeginPaint")
	procEndPaint            = user32.NewProc("EndPaint")
	procFillRect            = user32.NewProc("FillRect")
	procFrameRect           = user32.NewProc("FrameRect")
	procDrawTextW           = user32.NewProc("DrawTextW")
	procGetAsyncKeyState    = user32.NewProc("GetAsyncKeyState")
	procMonitorFromPoint    = user32.NewProc("MonitorFromPoint")
	procGetMonitorInfoW     = user32.NewProc("GetMonitorInfoW")
	procSystemParamsInfoW   = user32.NewProc("SystemParametersInfoW")
	procGetDpiForWindow     = user32.NewProc("GetDpiForWindow")
	procSetDpiAwareCtx      = user32.NewProc("SetProcessDpiAwarenessContext")
	procSetThreadDpiCtx     = user32.NewProc("SetThreadDpiAwarenessContext")
	procShellNotifyIconW    = shell32.NewProc("Shell_NotifyIconW")
	procGetModuleHandleW    = kernel32.NewProc("GetModuleHandleW")

	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject           = gdi32.NewProc("SelectObject")
	procDeleteDC               = gdi32.NewProc("DeleteDC")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
	procCreateSolidBrush       = gdi32.NewProc("CreateSolidBrush")
	procCreatePen              = gdi32.NewProc("CreatePen")
	procGetStockObject         = gdi32.NewProc("GetStockObject")
	procSetBkMode              = gdi32.NewProc("SetBkMode")
	procSetTextColor           = gdi32.NewProc("SetTextColor")
	procCreateFontW            = gdi32.NewProc("CreateFontW")
	procRoundRect              = gdi32.NewProc("RoundRect")
	procEllipse                = gdi32.NewProc("Ellipse")
	procBitBlt                 = gdi32.NewProc("BitBlt")

	procDwmSetWindowAttribute = dwmapi.NewProc("DwmSetWindowAttribute")
)

const (
	wmNull           = 0x0000
	wmPaint          = 0x000F
	wmQueryEndSess   = 0x0011
	wmEraseBkgnd     = 0x0014
	wmEndSession     = 0x0016
	wmMouseActivate  = 0x0021
	wmTimer          = 0x0113
	wmLButtonUp      = 0x0202
	wmLButtonDbl     = 0x0203
	wmRButtonUp      = 0x0205
	wmTrayCallback   = 0x8001 // WM_APP + 1

	nimAdd     = 0
	nimModify  = 1
	nimDelete  = 2
	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04
	nifInfo    = 0x10
	niifInfo   = 0x01

	mfString       = 0x0000
	mfSeparator    = 0x0800
	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100
	tpmNoNotify    = 0x0080

	imageIcon  = 1
	idcArrow   = 32512
	smCxSmIcon = 49
	smCySmIcon = 50

	menuOpen = 1
	menuGame = 2
	menuQuit = 3

	timerSingleClick  = 1
	timerFlyoutWatch  = 2
	timerFlyoutPaint  = 3

	wsPopup         = 0x80000000
	wsExTopmost     = 0x00000008
	wsExToolWindow  = 0x00000080
	wsExNoActivate  = 0x08000000
	swHide          = 0
	swpNoActivate   = 0x0010
	swpShowWindow   = 0x0040
	hwndTopmost     = ^uintptr(0)
	maNoActivate    = 3
	vkLButton       = 0x01
	vkRButton       = 0x02

	dwmwaCornerPref = 33
	dwmwaBorderClr  = 34
	dwmwcpRound     = 2

	nullPen        = 8
	transparentBk  = 1
	srccopy        = 0x00CC0020
	cleartypeQual  = 5
	defaultCharset = 1
	dtLeft         = 0x00000000
	dtRight        = 0x00000002
	dtVCenter      = 0x00000004
	dtSingleLine   = 0x00000020
	dtEndEllipsis  = 0x00008000
)

type winPoint struct {
	X, Y int32
}

type winRect struct {
	Left, Top, Right, Bottom int32
}

// MONITORINFO（64 位布局，40 字节）
type monitorInfo struct {
	Size    uint32
	Monitor winRect
	Work    winRect
	Flags   uint32
}

// PAINTSTRUCT（64 位布局，72 字节）
type paintStruct struct {
	Hdc         uintptr
	FErase      int32
	RcPaint     winRect
	FRestore    int32
	FIncUpdate  int32
	RgbReserved [32]byte
}

// NOTIFYICONDATAW（64 位布局，共 976 字节）
type notifyIconData struct {
	Size            uint32
	Wnd             uintptr
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            uintptr
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GuidItem        [16]byte
	BalloonIcon     uintptr
}

// WNDCLASSEXW（64 位布局，共 80 字节）
type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

// MSG（64 位布局，共 48 字节）
type winMsg struct {
	Hwnd     uintptr
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       winPoint
	LPrivate uint32
}

var tray struct {
	mu       sync.Mutex
	nid      notifyIconData
	active   bool
	notified bool
}

type flyoutState struct {
	hwnd         uintptr
	visible      bool
	openPt       winPoint
	openedAt     time.Time
	entered      bool
	outsideTicks int
	scale        float64
	width        int32
	height       int32
	lightTheme   bool
	app          *app
}

var flyout flyoutState

// startTray 在独立的系统线程上创建托盘图标和悬浮状态窗；失败时返回 false，程序退回“关窗即退出”。
func startTray(a *app) bool {
	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread() // 窗口消息必须在创建窗口的线程上处理
		ready <- runTray(a, ready)
	}()
	if err := <-ready; err != nil {
		log.Printf("托盘图标不可用：%v", err)
		return false
	}
	return true
}

func runTray(a *app, ready chan error) error {
	// 启用 Per-Monitor v2 DPI 感知（包括线程级覆盖，兼容已声明系统 DPI 清单的进程），确保高分屏下清晰锐利
	if procSetDpiAwareCtx.Find() == nil {
		procSetDpiAwareCtx.Call(^uintptr(3)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2
	}
	if procSetThreadDpiCtx.Find() == nil {
		procSetThreadDpiCtx.Call(^uintptr(3))
	}

	instance, _, _ := procGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString("GBFDockTray")
	taskbarName, _ := syscall.UTF16PtrFromString("TaskbarCreated")
	taskbarCreated, _, _ := procRegisterWindowMsgW.Call(uintptr(unsafe.Pointer(taskbarName)))

	var ignoreNextLButtonUp bool
	var pendingClickPt winPoint

	wndProc := syscall.NewCallback(func(hwnd, msg, wparam, lparam uintptr) uintptr {
		switch {
		case msg == wmTrayCallback && lparam == wmLButtonDbl:
			// 双击：立即取消单击定时器，并吞掉随后的第二次 WM_LBUTTONUP，只打开单个主面板
			procKillTimer.Call(hwnd, timerSingleClick)
			ignoreNextLButtonUp = true
			hideFlyout()
			go a.showWindow()
			return 0

		case msg == wmTrayCallback && lparam == wmLButtonUp:
			if ignoreNextLButtonUp {
				ignoreNextLButtonUp = false
				return 0
			}
			if flyout.visible {
				hideFlyout()
				return 0
			}
			procGetCursorPos.Call(uintptr(unsafe.Pointer(&pendingClickPt)))
			procSetTimer.Call(hwnd, timerSingleClick, 250, 0)
			return 0

		case msg == wmTrayCallback && lparam == wmRButtonUp:
			procKillTimer.Call(hwnd, timerSingleClick)
			hideFlyout()
			trayMenu(a, hwnd)
			return 0

		case msg == wmTimer && wparam == timerSingleClick:
			procKillTimer.Call(hwnd, timerSingleClick)
			showFlyout(a, pendingClickPt)
			return 0

		case msg == wmQueryEndSess:
			return 1

		case msg == wmEndSession && wparam != 0:
			a.cleanupOnExit()
			return 0

		case msg == taskbarCreated && taskbarCreated != 0: // Explorer 重启后托盘被清空，重新放回图标
			tray.mu.Lock()
			procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&tray.nid)))
			tray.mu.Unlock()
			return 0
		}
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
		return r
	})

	wc := wndClassEx{WndProc: wndProc, Instance: instance, ClassName: className}
	wc.Size = uint32(unsafe.Sizeof(wc))
	if r, _, e := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return e
	}
	// 隐藏窗口，只用来接收托盘消息与系统关机通知
	hwnd, _, e := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(className)),
		0, 0, 0, 0, 0, 0, 0, instance, 0)
	if hwnd == 0 {
		return e
	}

	initFlyoutWindow(a, instance)

	cx, _, _ := procGetSystemMetrics.Call(smCxSmIcon)
	cy, _, _ := procGetSystemMetrics.Call(smCySmIcon)
	icon, _, _ := procLoadImageW.Call(instance, 1, imageIcon, cx, cy, 0) // exe 里内嵌的 1 号图标

	tray.mu.Lock()
	tray.nid = notifyIconData{Wnd: hwnd, ID: 1, Flags: nifMessage | nifIcon | nifTip, CallbackMessage: wmTrayCallback, Icon: icon}
	tray.nid.Size = uint32(unsafe.Sizeof(tray.nid))
	copyUTF16(tray.nid.Tip[:], appName+" v"+appVersion+" · 单击看状态 / 双击开面板")
	ok, _, e := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&tray.nid)))
	tray.active = ok != 0
	tray.mu.Unlock()
	if ok == 0 {
		return errors.New("Shell_NotifyIcon 失败：" + e.Error())
	}
	ready <- nil

	var m winMsg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return nil
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func initFlyoutWindow(a *app, instance uintptr) {
	flyout.app = a
	className, _ := syscall.UTF16PtrFromString("GBFDockFlyout")
	cursor, _, _ := procLoadCursorW.Call(0, idcArrow)

	flyoutProc := syscall.NewCallback(func(hwnd, msg, wparam, lparam uintptr) uintptr {
		switch msg {
		case wmMouseActivate:
			return maNoActivate
		case wmEraseBkgnd:
			return 1
		case wmPaint:
			paintFlyout(hwnd)
			return 0
		case wmLButtonUp:
			hideFlyout()
			go a.showWindow()
			return 0
		case wmTimer:
			switch wparam {
			case timerFlyoutWatch:
				watchFlyout(hwnd)
			case timerFlyoutPaint:
				if flyout.visible {
					procInvalidateRect.Call(hwnd, 0, 0)
				}
			}
			return 0
		}
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
		return r
	})

	wc := wndClassEx{
		WndProc:   flyoutProc,
		Instance:  instance,
		Cursor:    cursor,
		ClassName: className,
	}
	wc.Size = uint32(unsafe.Sizeof(wc))
	if r, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return
	}

	hwnd, _, _ := procCreateWindowExW.Call(
		wsExTopmost|wsExToolWindow|wsExNoActivate,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(className)),
		wsPopup,
		0, 0, 276, 148,
		0, 0, instance, 0,
	)
	if hwnd == 0 {
		return
	}
	flyout.hwnd = hwnd
	if procDwmSetWindowAttribute.Find() == nil {
		round := uint32(dwmwcpRound)
		procDwmSetWindowAttribute.Call(hwnd, dwmwaCornerPref, uintptr(unsafe.Pointer(&round)), 4)
	}
}

func showFlyout(a *app, clickPt winPoint) {
	if flyout.hwnd == 0 {
		return
	}
	flyout.app = a
	flyout.openPt = clickPt
	flyout.openedAt = time.Now()
	flyout.entered = false
	flyout.outsideTicks = 0
	flyout.lightTheme = isWindowsLightTheme()

	dpi := float64(96)
	if procGetDpiForWindow.Find() == nil {
		if d, _, _ := procGetDpiForWindow.Call(flyout.hwnd); d >= 96 {
			dpi = float64(d)
		}
	}
	scale := dpi / 96.0
	w := int32(280 * scale)
	h := int32(150 * scale)
	flyout.scale = scale
	flyout.width = w
	flyout.height = h

	work := workAreaForPoint(clickPt)
	margin := int32(10 * scale)

	x := clickPt.X - w/2
	if x < work.Left+margin {
		x = work.Left + margin
	}
	if x+w > work.Right-margin {
		x = work.Right - w - margin
	}

	var y int32
	switch {
	case clickPt.Y >= work.Bottom-int32(6*scale): // 任务栏在底部
		y = work.Bottom - h - margin
	case clickPt.Y <= work.Top+int32(6*scale): // 任务栏在顶部
		y = work.Top + margin
	default: // 任务栏在左右两侧或溢出弹出菜单内
		y = clickPt.Y - h - margin
		if y < work.Top+margin {
			y = clickPt.Y + margin
		}
		if y+h > work.Bottom-margin {
			y = work.Bottom - h - margin
		}
	}

	if procDwmSetWindowAttribute.Find() == nil {
		border := rgb(0x2E, 0x35, 0x45)
		if flyout.lightTheme {
			border = rgb(0xD8, 0xE0, 0xEB)
		}
		procDwmSetWindowAttribute.Call(flyout.hwnd, dwmwaBorderClr, uintptr(unsafe.Pointer(&border)), 4)
	}

	flyout.visible = true
	procSetWindowPos.Call(flyout.hwnd, hwndTopmost, uintptr(x), uintptr(y), uintptr(w), uintptr(h), swpNoActivate|swpShowWindow)
	procInvalidateRect.Call(flyout.hwnd, 0, 0)
	procSetTimer.Call(flyout.hwnd, timerFlyoutWatch, 50, 0)
	procSetTimer.Call(flyout.hwnd, timerFlyoutPaint, 1000, 0)
}

func hideFlyout() {
	if flyout.hwnd == 0 || !flyout.visible {
		return
	}
	flyout.visible = false
	procKillTimer.Call(flyout.hwnd, timerFlyoutWatch)
	procKillTimer.Call(flyout.hwnd, timerFlyoutPaint)
	procShowWindow.Call(flyout.hwnd, swHide)
}

func watchFlyout(hwnd uintptr) {
	if !flyout.visible {
		return
	}
	var cur winPoint
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&cur)))
	var rc winRect
	procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rc)))

	pad := int32(4 * flyout.scale)
	inFlyout := cur.X >= rc.Left-pad && cur.X <= rc.Right+pad && cur.Y >= rc.Top-pad && cur.Y <= rc.Bottom+pad
	if inFlyout {
		flyout.entered = true
		flyout.outsideTicks = 0
		return
	}

	if flyout.entered {
		// 鼠标已经进过小框体，现在移出了小框体 -> 立即消失
		hideFlyout()
		return
	}

	// 若打开后超过 350ms 在框外点击了鼠标，立即收起
	if time.Since(flyout.openedAt) > 350*time.Millisecond {
		lDown, _, _ := procGetAsyncKeyState.Call(vkLButton)
		rDown, _, _ := procGetAsyncKeyState.Call(vkRButton)
		if (uint16(lDown)&0x8000) != 0 || (uint16(rDown)&0x8000) != 0 {
			hideFlyout()
			return
		}
	}

	// 刚点击托盘图标后：若鼠标仍停在托盘图标附近，或正在垂直走向悬浮窗的走廊区域，暂不关闭
	spotR := int32(28 * flyout.scale)
	nearTraySpot := abs32(cur.X-flyout.openPt.X) <= spotR && abs32(cur.Y-flyout.openPt.Y) <= spotR

	corridor := false
	if cur.X >= rc.Left-pad && cur.X <= rc.Right+pad {
		if flyout.openPt.Y > rc.Bottom && cur.Y >= rc.Bottom && cur.Y <= flyout.openPt.Y+spotR/2 {
			corridor = true
		} else if flyout.openPt.Y < rc.Top && cur.Y <= rc.Top && cur.Y >= flyout.openPt.Y-spotR/2 {
			corridor = true
		}
	}

	if nearTraySpot || corridor {
		flyout.outsideTicks = 0
		return
	}
	flyout.outsideTicks++
	if flyout.outsideTicks >= 2 {
		hideFlyout()
	}
}

func workAreaForPoint(pt winPoint) winRect {
	ptPacked := uintptr(uint32(pt.X)) | (uintptr(uint32(pt.Y)) << 32)
	hMon, _, _ := procMonitorFromPoint.Call(ptPacked, 2) // MONITOR_DEFAULTTONEAREST
	if hMon != 0 {
		var mi monitorInfo
		mi.Size = uint32(unsafe.Sizeof(mi))
		if r, _, _ := procGetMonitorInfoW.Call(hMon, uintptr(unsafe.Pointer(&mi))); r != 0 {
			return mi.Work
		}
	}
	var rc winRect
	procSystemParamsInfoW.Call(48, 0, uintptr(unsafe.Pointer(&rc)), 0) // SPI_GETWORKAREA
	return rc
}

func paintFlyout(hwnd uintptr) {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
	if hdc == 0 {
		return
	}
	defer procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))

	w := flyout.width
	h := flyout.height
	if w <= 0 || h <= 0 {
		return
	}
	s := flyout.scale
	if s <= 0 {
		s = 1
	}

	memDC, _, _ := procCreateCompatibleDC.Call(hdc)
	bmp, _, _ := procCreateCompatibleBitmap.Call(hdc, uintptr(w), uintptr(h))
	oldBmp, _, _ := procSelectObject.Call(memDC, bmp)
	defer func() {
		procSelectObject.Call(memDC, oldBmp)
		procDeleteObject.Call(bmp)
		procDeleteDC.Call(memDC)
	}()

	// FlClash 风格高对比度深/浅色主题配色
	bgClr := rgb(0x15, 0x18, 0x1E)
	cardClr := rgb(0x1E, 0x22, 0x2B)
	borderClr := rgb(0x2E, 0x35, 0x45)
	textClr := rgb(0xFF, 0xFF, 0xFF)
	mutedClr := rgb(0x94, 0xA3, 0xB8)
	accentClr := rgb(0x2F, 0x7C, 0xF6)
	trackClr := rgb(0x26, 0x2C, 0x3A)
	okClr := rgb(0x4A, 0xDE, 0x80)
	if flyout.lightTheme {
		bgClr = rgb(0xF1, 0xF4, 0xF9)
		cardClr = rgb(0xFF, 0xFF, 0xFF)
		borderClr = rgb(0xDC, 0xE3, 0xEE)
		textClr = rgb(0x0F, 0x17, 0x2A)
		mutedClr = rgb(0x64, 0x74, 0x8B)
		accentClr = rgb(0x25, 0x63, 0xEB)
		trackClr = rgb(0xE2, 0xE8, 0xF0)
		okClr = rgb(0x15, 0x80, 0x3D)
	}

	// 背景
	bgBrush, _, _ := procCreateSolidBrush.Call(uintptr(bgClr))
	fullRc := winRect{0, 0, w, h}
	procFillRect.Call(memDC, uintptr(unsafe.Pointer(&fullRc)), bgBrush)
	procDeleteObject.Call(bgBrush)

	// 在不支持 DWM 圆角边框的老系统上保留 1px 描边
	if procDwmSetWindowAttribute.Find() != nil {
		bBrush, _, _ := procCreateSolidBrush.Call(uintptr(borderClr))
		procFrameRect.Call(memDC, uintptr(unsafe.Pointer(&fullRc)), bBrush)
		procDeleteObject.Call(bBrush)
	}

	procSetBkMode.Call(memDC, transparentBk)

	// 读取实时数据
	a := flyout.app
	var rateText, rateSub, sizeText, sizeSub, statusText string
	var rateRatio float64
	active := false
	if a != nil && a.p != nil {
		hits := a.p.hits.Load()
		merged := a.p.merged.Load()
		misses := a.p.misses.Load()
		total := hits + merged + misses
		if total > 0 {
			rateRatio = float64(hits+merged) / float64(total)
			rateText = fmt.Sprintf("%.1f%%", rateRatio*100)
			rateSub = fmt.Sprintf("命中 %d / %d", hits+merged, total)
		} else {
			rateText = "0.0%"
			rateSub = "暂无资源请求"
		}
		cBytes := a.p.cachedBytes.Load()
		cFiles := a.p.cachedFiles.Load()
		sizeText = formatBytesUI(cBytes)
		sizeSub = fmt.Sprintf("共 %d 个文件", cFiles)
		active = a.isProxyActive()
	}
	if active {
		statusText = "代理接管中"
	} else {
		statusText = "待机中"
	}

	pad := int32(14 * s)
	titleFont := makeFont(int32(14*s), 700, "Microsoft YaHei UI")
	smallFont := makeFont(int32(12*s), 400, "Microsoft YaHei UI")
	valFont := makeFont(int32(22*s), 700, "Segoe UI")
	defer func() {
		procDeleteObject.Call(titleFont)
		procDeleteObject.Call(smallFont)
		procDeleteObject.Call(valFont)
	}()

	// 顶部标题与状态点
	dotSize := int32(8 * s)
	dotX := pad
	dotY := int32(16 * s)
	dotClr := accentClr
	if active {
		dotClr = okClr
	}
	drawFilledCircle(memDC, dotX, dotY, dotX+dotSize, dotY+dotSize, dotClr)

	titleRc := winRect{dotX + dotSize + int32(7*s), int32(9 * s), w - pad - int32(76*s), int32(29 * s)}
	drawText(memDC, titleFont, textClr, appName+" v"+appVersion, titleRc, dtLeft|dtVCenter|dtSingleLine)

	stClr := mutedClr
	if active {
		stClr = okClr
	}
	stRc := winRect{w - pad - int32(84*s), int32(9 * s), w - pad, int32(29 * s)}
	drawText(memDC, smallFont, stClr, statusText, stRc, dtRight|dtVCenter|dtSingleLine)

	// 双栏 Fluent 指标卡片
	cardTop := int32(36 * s)
	cardH := int32(74 * s)
	gap := int32(10 * s)
	cardW := (w - pad*2 - gap) / 2

	leftCard := winRect{pad, cardTop, pad + cardW, cardTop + cardH}
	rightCard := winRect{pad + cardW + gap, cardTop, w - pad, cardTop + cardH}
	drawRoundedBox(memDC, leftCard, int32(8*s), cardClr, borderClr)
	drawRoundedBox(memDC, rightCard, int32(8*s), cardClr, borderClr)

	inPad := int32(10 * s)
	// 左卡：缓存读取率
	drawText(memDC, smallFont, mutedClr, "缓存读取率", winRect{leftCard.Left + inPad, leftCard.Top + int32(7*s), leftCard.Right - inPad, leftCard.Top + int32(22*s)}, dtLeft|dtSingleLine)
	drawText(memDC, valFont, accentClr, rateText, winRect{leftCard.Left + inPad, leftCard.Top + int32(23*s), leftCard.Right - inPad, leftCard.Top + int32(49*s)}, dtLeft|dtVCenter|dtSingleLine|dtEndEllipsis)
	drawText(memDC, smallFont, mutedClr, rateSub, winRect{leftCard.Left + inPad, leftCard.Top + int32(50*s), leftCard.Right - inPad, leftCard.Bottom - int32(5*s)}, dtLeft|dtSingleLine|dtEndEllipsis)

	// 右卡：已缓存空间
	drawText(memDC, smallFont, mutedClr, "已缓存空间", winRect{rightCard.Left + inPad, rightCard.Top + int32(7*s), rightCard.Right - inPad, rightCard.Top + int32(22*s)}, dtLeft|dtSingleLine)
	drawText(memDC, valFont, textClr, sizeText, winRect{rightCard.Left + inPad, rightCard.Top + int32(23*s), rightCard.Right - inPad, rightCard.Top + int32(49*s)}, dtLeft|dtVCenter|dtSingleLine|dtEndEllipsis)
	drawText(memDC, smallFont, mutedClr, sizeSub, winRect{rightCard.Left + inPad, rightCard.Top + int32(50*s), rightCard.Right - inPad, rightCard.Bottom - int32(5*s)}, dtLeft|dtSingleLine|dtEndEllipsis)

	// 命中率进度条
	barTop := cardTop + cardH + int32(9*s)
	barH := int32(4 * s)
	if barH < 3 {
		barH = 3
	}
	barRc := winRect{pad, barTop, w - pad, barTop + barH}
	drawRoundedBox(memDC, barRc, barH, trackClr, trackClr)
	if rateRatio > 0 {
		if rateRatio > 1 {
			rateRatio = 1
		}
		fillW := int32(float64(barRc.Right-barRc.Left) * rateRatio)
		if fillW < barH {
			fillW = barH
		}
		fillRc := winRect{barRc.Left, barRc.Top, barRc.Left + fillW, barRc.Bottom}
		drawRoundedBox(memDC, fillRc, barH, accentClr, accentClr)
	}

	// 底部操作提示
	footRc := winRect{pad, barTop + barH + int32(5*s), w - pad, h - int32(7*s)}
	drawText(memDC, smallFont, mutedClr, "双击图标打开面板 · 移开鼠标消失", footRc, dtLeft|dtVCenter|dtSingleLine)

	procBitBlt.Call(hdc, 0, 0, uintptr(w), uintptr(h), memDC, 0, 0, srccopy)
}

func makeFont(px int32, weight int32, face string) uintptr {
	f, _ := syscall.UTF16PtrFromString(face)
	h, _, _ := procCreateFontW.Call(
		uintptr(uint32(-px)), 0, 0, 0, uintptr(weight),
		0, 0, 0, defaultCharset, 0, 0, cleartypeQual, 0, uintptr(unsafe.Pointer(f)),
	)
	return h
}

func drawText(hdc, font uintptr, color uint32, text string, rc winRect, flags uint32) {
	u, _ := syscall.UTF16FromString(text)
	if len(u) <= 1 {
		return
	}
	oldFont, _, _ := procSelectObject.Call(hdc, font)
	procSetTextColor.Call(hdc, uintptr(color))
	procDrawTextW.Call(hdc, uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(&rc)), uintptr(flags))
	procSelectObject.Call(hdc, oldFont)
}

func drawRoundedBox(hdc uintptr, rc winRect, radius int32, fillClr, borderClr uint32) {
	brush, _, _ := procCreateSolidBrush.Call(uintptr(fillClr))
	pen, _, _ := procCreatePen.Call(0, 1, uintptr(borderClr))
	oldBrush, _, _ := procSelectObject.Call(hdc, brush)
	oldPen, _, _ := procSelectObject.Call(hdc, pen)
	procRoundRect.Call(hdc, uintptr(rc.Left), uintptr(rc.Top), uintptr(rc.Right), uintptr(rc.Bottom), uintptr(radius), uintptr(radius))
	procSelectObject.Call(hdc, oldBrush)
	procSelectObject.Call(hdc, oldPen)
	procDeleteObject.Call(brush)
	procDeleteObject.Call(pen)
}

func drawFilledCircle(hdc uintptr, l, t, r, b int32, fillClr uint32) {
	brush, _, _ := procCreateSolidBrush.Call(uintptr(fillClr))
	nPen, _, _ := procGetStockObject.Call(nullPen)
	oldBrush, _, _ := procSelectObject.Call(hdc, brush)
	oldPen, _, _ := procSelectObject.Call(hdc, nPen)
	procEllipse.Call(hdc, uintptr(l), uintptr(t), uintptr(r), uintptr(b))
	procSelectObject.Call(hdc, oldBrush)
	procSelectObject.Call(hdc, oldPen)
	procDeleteObject.Call(brush)
}

func rgb(r, g, b byte) uint32 {
	return uint32(r) | (uint32(g) << 8) | (uint32(b) << 16)
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

func formatBytesUI(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.2f GB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	default:
		return fmt.Sprintf("%d KB", (n+500)/1000)
	}
}

func isWindowsLightTheme() bool {
	subkey, err := syscall.UTF16PtrFromString(`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`)
	if err != nil {
		return false
	}
	var hk syscall.Handle
	if syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, subkey, 0, syscall.KEY_READ, &hk) != nil {
		return false
	}
	defer syscall.RegCloseKey(hk)
	valName, _ := syscall.UTF16PtrFromString("AppsUseLightTheme")
	var typ uint32
	var data uint32
	size := uint32(4)
	if syscall.RegQueryValueEx(hk, valName, nil, &typ, (*byte)(unsafe.Pointer(&data)), &size) != nil {
		return false
	}
	return data == 1
}

func trayMenu(a *app, hwnd uintptr) {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)
	for _, item := range []struct {
		id    uintptr
		label string
	}{{menuOpen, "打开控制面板"}, {menuGame, "启动游戏"}, {0, ""}, {menuQuit, "退出 GBF-Dock"}} {
		if item.id == 0 {
			procAppendMenuW.Call(menu, mfSeparator, 0, 0)
			continue
		}
		label, _ := syscall.UTF16PtrFromString(item.label)
		procAppendMenuW.Call(menu, mfString, item.id, uintptr(unsafe.Pointer(label)))
	}
	procSetMenuDefaultItem.Call(menu, menuOpen, 0)
	var pt winPoint
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(hwnd) // 不这样做，点菜单外面时菜单不会消失
	cmd, _, _ := procTrackPopupMenu.Call(menu, tpmRightButton|tpmReturnCmd|tpmNoNotify,
		uintptr(pt.X), uintptr(pt.Y), 0, hwnd, 0)
	procPostMessageW.Call(hwnd, wmNull, 0, 0)
	switch cmd {
	case menuOpen:
		go a.showWindow()
	case menuGame:
		go func() {
			out, err := a.launchGame(false)
			if err == nil && out.NeedRestart {
				if !confirmRestartBrowserDialog(out.BrowserLabel) {
					return
				}
				out, err = a.launchGame(true)
			}
			if err != nil {
				log.Printf("托盘启动游戏失败：%v", err)
			} else if out.Message != "" {
				log.Print(out.Message)
			}
		}()
	case menuQuit:
		a.exit()
	}
}

// trayNotify 第一次关闭面板窗口时提醒：程序还在托盘里运行。
func trayNotify(title, text string) {
	tray.mu.Lock()
	defer tray.mu.Unlock()
	if !tray.active || tray.notified {
		return
	}
	tray.notified = true
	n := tray.nid
	n.Flags = nifInfo
	n.InfoFlags = niifInfo
	copyUTF16(n.InfoTitle[:], title)
	copyUTF16(n.Info[:], text)
	procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&n)))
}

// removeTray 退出前移除图标，避免托盘里残留一个“幽灵图标”。
func removeTray() {
	tray.mu.Lock()
	defer tray.mu.Unlock()
	if tray.active {
		procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&tray.nid)))
		tray.active = false
	}
}

func copyUTF16(dst []uint16, s string) {
	u, _ := syscall.UTF16FromString(s)
	if len(u) > len(dst) {
		u = append(u[:len(dst)-1], 0)
	}
	copy(dst, u)
}
