//go:build windows

package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	webviewClientID        = "{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}"
	webviewInitTimeout     = 15 * time.Second
	timerWebViewInitPump   = 9001
	wmDestroy              = 0x0002
	wmMove                 = 0x0003
	wmSize                 = 0x0005
	wmActivate             = 0x0006
	wmSettingChange        = 0x001A
	wmGetMinMaxInfo        = 0x0024
	wmMoving               = 0x0216
	wmExitSizeMove         = 0x0232
	wmDpiChanged           = 0x02E0
	wmPanelShow            = 0x800A
	wmPanelDestroy         = 0x800B
	wsOverlappedWindow     = 0x00CF0000
	wsClipChildren         = 0x02000000
	sizeMinimized          = 1
	swShow                 = 5
	dwmwaUseImmersiveDark  = 20
	coinitApartment        = 0x2
	keyWow6432             = 0x0200
	keyWow6464             = 0x0100
	dpiAwarenessPerMonitor = ^uintptr(3) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)

	sOK          uintptr = 0
	eFail        uintptr = 0x80004005
	ePointer     uintptr = 0x80004003
	eNoInterface uintptr = 0x80004002
	eOutOfMem    uintptr = 0x8007000E
)

var (
	procGetClientRect    = user32.NewProc("GetClientRect")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procSendMessageW     = user32.NewProc("SendMessageW")
	procUpdateWindow     = user32.NewProc("UpdateWindow")
	procCoTaskMemAlloc   = ole32.NewProc("CoTaskMemAlloc")
	panelClassOnce       sync.Once
	panelClassErr        error
	panelWndProcPtr      uintptr
	panelCOMVtblOnce     sync.Once
	panelEnvOptionsVtbl  [11]uintptr
	panelCompletionVtbl  [4]uintptr
)

type minMaxInfo struct {
	PtReserved     winPoint
	PtMaxSize      winPoint
	PtMaxPosition  winPoint
	PtMinTrackSize winPoint
	PtMaxTrackSize winPoint
}

func applyMinMaxInfo(hwnd, lParam uintptr) {
	if lParam == 0 {
		return
	}
	scale := windowDPIScale(hwnd)
	minW := int32(float64(panelMinWidthDIP) * scale)
	minH := int32(float64(panelMinHeightDIP) * scale)

	var rc winRect
	if r, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rc))); r != 0 {
		center := winPoint{X: (rc.Left + rc.Right) / 2, Y: (rc.Top + rc.Bottom) / 2}
		work := workAreaForPoint(center)
		workW := work.Right - work.Left
		workH := work.Bottom - work.Top
		if workW > 0 && minW > workW {
			minW = workW
		}
		if workH > 0 && minH > workH {
			minH = workH
		}
	}

	var mmi minMaxInfo
	procMoveMemory.Call(uintptr(unsafe.Pointer(&mmi)), lParam, unsafe.Sizeof(mmi))
	if minW > 0 {
		mmi.PtMinTrackSize.X = minW
	}
	if minH > 0 {
		mmi.PtMinTrackSize.Y = minH
	}
	procMoveMemory.Call(lParam, uintptr(unsafe.Pointer(&mmi)), unsafe.Sizeof(mmi))
}

type comGUID struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	iidIUnknown      = comGUID{0x00000000, 0x0000, 0x0000, [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
	iidEnvOptions    = comGUID{0x2fde08a8, 0x1e9a, 0x4766, [8]byte{0x8c, 0x05, 0x95, 0xa9, 0xce, 0xb9, 0xd1, 0xc5}}
	iidEnvCompleted  = comGUID{0x4e8a3389, 0xc9d8, 0x4bd2, [8]byte{0xb6, 0xb5, 0x12, 0x4f, 0xee, 0x6c, 0xc1, 0x4d}}
	iidCtrlCompleted = comGUID{0x6c4819f3, 0xc9b7, 0x4260, [8]byte{0x81, 0x27, 0xc9, 0xf5, 0xbd, 0xe7, 0xf6, 0x8c}}
	iidController2   = comGUID{0xc979903e, 0xd4ca, 0x4228, [8]byte{0x92, 0xeb, 0x47, 0xee, 0x3f, 0xa9, 0x6e, 0xab}}
)

type wvComObj struct {
	vtbl uintptr
	id   uintptr
}

type comState struct {
	iid        comGUID
	refs       atomic.Int32
	version    string
	extraArgs  string
	onComplete func(hr, obj uintptr)
}

var (
	comNextID atomic.Uintptr
	comStates sync.Map // uintptr -> *comState
)

func initCOMVtables() {
	panelCOMVtblOnce.Do(func() {
		qi := syscall.NewCallback(comQueryInterfaceCB)
		addRef := syscall.NewCallback(comAddRefCB)
		release := syscall.NewCallback(comReleaseCB)

		panelEnvOptionsVtbl = [11]uintptr{
			qi,
			addRef,
			release,
			syscall.NewCallback(envOptsGetArgsCB),
			syscall.NewCallback(envOptsPutPtrCB),
			syscall.NewCallback(envOptsGetNullStrCB),
			syscall.NewCallback(envOptsPutPtrCB),
			syscall.NewCallback(envOptsGetVersionCB),
			syscall.NewCallback(envOptsPutPtrCB),
			syscall.NewCallback(envOptsGetBoolCB),
			syscall.NewCallback(envOptsPutBoolCB),
		}
		panelCompletionVtbl = [4]uintptr{
			qi,
			addRef,
			release,
			syscall.NewCallback(completedInvokeCB),
		}
	})
}

func writeUintptrAt(dst, val uintptr) {
	procMoveMemory.Call(dst, uintptr(unsafe.Pointer(&val)), unsafe.Sizeof(val))
}

func writeInt32At(dst uintptr, val int32) {
	procMoveMemory.Call(dst, uintptr(unsafe.Pointer(&val)), unsafe.Sizeof(val))
}

func allocCOMObj(vtbl uintptr, st *comState) uintptr {
	initCOMVtables()
	mem, _, _ := procCoTaskMemAlloc.Call(unsafe.Sizeof(wvComObj{}))
	if mem == 0 {
		return 0
	}
	id := comNextID.Add(1)
	st.refs.Store(1)
	comStates.Store(id, st)
	obj := wvComObj{vtbl: vtbl, id: id}
	procMoveMemory.Call(mem, uintptr(unsafe.Pointer(&obj)), unsafe.Sizeof(obj))
	return mem
}

func lookupCOMState(this uintptr) (*wvComObj, *comState) {
	if this == 0 {
		return nil, nil
	}
	var obj wvComObj
	procMoveMemory.Call(uintptr(unsafe.Pointer(&obj)), this, unsafe.Sizeof(obj))
	v, ok := comStates.Load(obj.id)
	if !ok {
		return &obj, nil
	}
	return &obj, v.(*comState)
}

func comQueryInterfaceCB(this, riid, ppv uintptr) uintptr {
	if ppv == 0 {
		return ePointer
	}
	_, st := lookupCOMState(this)
	if st == nil || riid == 0 {
		writeUintptrAt(ppv, 0)
		return eNoInterface
	}
	var want comGUID
	procMoveMemory.Call(uintptr(unsafe.Pointer(&want)), riid, unsafe.Sizeof(want))
	if want == iidIUnknown || want == st.iid {
		st.refs.Add(1)
		writeUintptrAt(ppv, this)
		return sOK
	}
	writeUintptrAt(ppv, 0)
	return eNoInterface
}

func comAddRefCB(this uintptr) uintptr {
	_, st := lookupCOMState(this)
	if st == nil {
		return 0
	}
	return uintptr(st.refs.Add(1))
}

func comReleaseCB(this uintptr) uintptr {
	obj, st := lookupCOMState(this)
	if st == nil {
		return 0
	}
	n := st.refs.Add(-1)
	if n <= 0 {
		comStates.Delete(obj.id)
		procCoTaskMemFree.Call(this)
		return 0
	}
	return uintptr(n)
}

func coTaskMemUTF16(s string) (uintptr, error) {
	if s == "" {
		return 0, nil
	}
	u, err := syscall.UTF16FromString(s)
	if err != nil {
		return 0, err
	}
	sz := uintptr(len(u)) * 2
	p, _, _ := procCoTaskMemAlloc.Call(sz)
	if p == 0 {
		return 0, errors.New("CoTaskMemAlloc failed")
	}
	procMoveMemory.Call(p, uintptr(unsafe.Pointer(&u[0])), sz)
	return p, nil
}

func envOptsGetArgsCB(this, out uintptr) uintptr {
	if out == 0 {
		return ePointer
	}
	_, st := lookupCOMState(this)
	if st == nil {
		writeUintptrAt(out, 0)
		return eFail
	}
	if st.extraArgs == "" {
		writeUintptrAt(out, 0)
		return sOK
	}
	p, err := coTaskMemUTF16(st.extraArgs)
	if err != nil {
		writeUintptrAt(out, 0)
		return eOutOfMem
	}
	writeUintptrAt(out, p)
	return sOK
}

func envOptsGetNullStrCB(this, out uintptr) uintptr {
	if out == 0 {
		return ePointer
	}
	writeUintptrAt(out, 0)
	return sOK
}

func envOptsGetVersionCB(this, out uintptr) uintptr {
	if out == 0 {
		return ePointer
	}
	_, st := lookupCOMState(this)
	if st == nil {
		writeUintptrAt(out, 0)
		return eFail
	}
	p, err := coTaskMemUTF16(st.version)
	if err != nil {
		writeUintptrAt(out, 0)
		return eOutOfMem
	}
	writeUintptrAt(out, p)
	return sOK
}

func envOptsGetBoolCB(this, out uintptr) uintptr {
	if out == 0 {
		return ePointer
	}
	writeInt32At(out, 0)
	return sOK
}

func envOptsPutPtrCB(this, _ uintptr) uintptr  { return sOK }
func envOptsPutBoolCB(this, _ uintptr) uintptr { return sOK }

func completedInvokeCB(this, hr, result uintptr) uintptr {
	_, st := lookupCOMState(this)
	if st == nil {
		return eFail
	}
	if result != 0 {
		comAddRef(result)
	}
	if st.onComplete != nil {
		st.onComplete(hr, result)
	} else if result != 0 {
		comRelease(result)
	}
	return sOK
}

func comCall(obj uintptr, slot int, args ...uintptr) uintptr {
	if obj == 0 {
		return eFail
	}
	var vtbl uintptr
	procMoveMemory.Call(uintptr(unsafe.Pointer(&vtbl)), obj, unsafe.Sizeof(vtbl))
	if vtbl == 0 {
		return eFail
	}
	var fn uintptr
	procMoveMemory.Call(uintptr(unsafe.Pointer(&fn)), vtbl+uintptr(slot)*unsafe.Sizeof(uintptr(0)), unsafe.Sizeof(fn))
	all := make([]uintptr, 0, len(args)+1)
	all = append(all, obj)
	all = append(all, args...)
	r, _, _ := syscall.SyscallN(fn, all...)
	return r
}

func comQueryInterface(obj uintptr, iid *comGUID) uintptr {
	var out uintptr
	hr := comCall(obj, 0, uintptr(unsafe.Pointer(iid)), uintptr(unsafe.Pointer(&out)))
	if int32(hr) < 0 {
		return 0
	}
	return out
}

func comAddRef(obj uintptr) {
	if obj != 0 {
		comCall(obj, 1)
	}
}

func comRelease(obj uintptr) {
	if obj != 0 {
		comCall(obj, 2)
	}
}

func checkHR(name string, hr uintptr) error {
	if int32(hr) < 0 {
		return fmt.Errorf("%s failed: HRESULT 0x%08x", name, uint32(hr))
	}
	return nil
}

func packColorARGB(a, r, g, b byte) uintptr {
	return uintptr(uint32(a) | uint32(r)<<8 | uint32(g)<<16 | uint32(b)<<24)
}

func compareDotVersions(a, b string) int {
	ap := strings.Split(strings.TrimSpace(a), ".")
	bp := strings.Split(strings.TrimSpace(b), ".")
	n := len(ap)
	if len(bp) > n {
		n = len(bp)
	}
	for i := 0; i < n; i++ {
		var ai, bi int
		if i < len(ap) {
			ai, _ = strconv.Atoi(ap[i])
		}
		if i < len(bp) {
			bi, _ = strconv.Atoi(bp[i])
		}
		if ai != bi {
			if ai > bi {
				return 1
			}
			return -1
		}
	}
	return 0
}

func isValidDotVersion(v string) bool {
	if v == "" || v == "0.0.0.0" {
		return false
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return false
	}
	nonZero := false
	for _, part := range parts {
		if part == "" {
			return false
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return false
		}
		if n > 0 {
			nonZero = true
		}
	}
	return nonZero
}

func readEdgeWebViewReg(root syscall.Handle, subkey string, wowFlags uint32) (version, location string) {
	subPtr, err := syscall.UTF16PtrFromString(subkey)
	if err != nil {
		return "", ""
	}
	var hk syscall.Handle
	if syscall.RegOpenKeyEx(root, subPtr, 0, syscall.KEY_READ|wowFlags, &hk) != nil {
		return "", ""
	}
	defer syscall.RegCloseKey(hk)
	readStr := func(name string) string {
		np, err := syscall.UTF16PtrFromString(name)
		if err != nil {
			return ""
		}
		var typ, nBytes uint32
		if syscall.RegQueryValueEx(hk, np, nil, &typ, nil, &nBytes) != nil || nBytes == 0 || nBytes > 4096 {
			return ""
		}
		buf := make([]uint16, nBytes/2+1)
		if syscall.RegQueryValueEx(hk, np, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &nBytes) != nil {
			return ""
		}
		return strings.TrimSpace(syscall.UTF16ToString(buf))
	}
	return readStr("pv"), readStr("location")
}

func findWebView2Runtime() (dllPath, version string, err error) {
	if runtime.GOARCH != "amd64" {
		return "", "", fmt.Errorf("unsupported arch %s for direct WebView2", runtime.GOARCH)
	}
	const arch = "x64"
	var bestVer, bestDLL string
	tryCandidate := func(ver, dir string) {
		if !isValidDotVersion(ver) || dir == "" {
			return
		}
		for _, p := range []string{
			filepath.Join(dir, "EBWebView", arch, "EmbeddedBrowserWebView.dll"),
			filepath.Join(dir, arch, "EmbeddedBrowserWebView.dll"),
		} {
			if st, statErr := os.Stat(p); statErr == nil && !st.IsDir() {
				if bestDLL == "" || compareDotVersions(ver, bestVer) > 0 {
					bestVer = ver
					bestDLL = p
				}
				return
			}
		}
	}

	var defaultRoots []string
	for _, envName := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
		if v := strings.TrimSpace(os.Getenv(envName)); v != "" {
			defaultRoots = append(defaultRoots, filepath.Join(v, "Microsoft", "EdgeWebView", "Application"))
		}
	}

	for _, rk := range []struct {
		root syscall.Handle
		path string
		flag uint32
	}{
		{syscall.HKEY_CURRENT_USER, `Software\Microsoft\EdgeUpdate\Clients\` + webviewClientID, 0},
		{syscall.HKEY_LOCAL_MACHINE, `SOFTWARE\Microsoft\EdgeUpdate\Clients\` + webviewClientID, keyWow6432},
		{syscall.HKEY_LOCAL_MACHINE, `SOFTWARE\Microsoft\EdgeUpdate\Clients\` + webviewClientID, keyWow6464},
	} {
		ver, loc := readEdgeWebViewReg(rk.root, rk.path, rk.flag)
		if !isValidDotVersion(ver) {
			continue
		}
		if loc != "" && filepath.IsAbs(loc) {
			tryCandidate(ver, filepath.Join(loc, ver))
			tryCandidate(ver, loc)
		}
		for _, rootDir := range defaultRoots {
			tryCandidate(ver, filepath.Join(rootDir, ver))
		}
	}

	// Fallback directory scan if EdgeUpdate registry keys are missing or restricted.
	scanRoots := append([]string{}, defaultRoots...)
	if pf86 := strings.TrimSpace(os.Getenv("ProgramFiles(x86)")); pf86 != "" {
		scanRoots = append(scanRoots, filepath.Join(pf86, "Microsoft", "Edge", "Application"))
	}
	for _, rootDir := range scanRoots {
		entries, rErr := os.ReadDir(rootDir)
		if rErr != nil {
			continue
		}
		for _, ent := range entries {
			if ent.IsDir() && isValidDotVersion(ent.Name()) {
				tryCandidate(ent.Name(), filepath.Join(rootDir, ent.Name()))
			}
		}
	}

	if bestDLL == "" {
		return "", "", errors.New("未找到 Microsoft Edge WebView2 Runtime")
	}
	return bestDLL, bestVer, nil
}

type nativePanelState struct {
	mu       sync.Mutex
	hwnd     uintptr
	env      uintptr
	ctrl     uintptr
	wv       uintptr
	cur      windowBounds
	visible  bool
	dark     bool
	bgBrush  uintptr
	closedCh chan struct{}
}

var nativePanel nativePanelState

func registerPanelClass() error {
	panelClassOnce.Do(func() {
		clsName, err := syscall.UTF16PtrFromString("GBFDockPanelWnd")
		if err != nil {
			panelClassErr = err
			return
		}
		panelWndProcPtr = syscall.NewCallback(nativePanelWndProc)
		hInst, _, _ := procGetModuleHandleW.Call(0)
		icon, _, _ := procLoadImageW.Call(hInst, 1, imageIcon, 0, 0, 0x00000040)
		cursor, _, _ := procLoadCursorW.Call(0, idcArrow)
		wc := wndClassEx{
			Size:      uint32(unsafe.Sizeof(wndClassEx{})),
			Style:     0x0003, // CS_HREDRAW | CS_VREDRAW
			WndProc:   panelWndProcPtr,
			Instance:  hInst,
			Icon:      icon,
			IconSm:    icon,
			Cursor:    cursor,
			ClassName: clsName,
		}
		if atom, _, e := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
			panelClassErr = fmt.Errorf("RegisterClassExW(GBFDockPanelWnd): %v", e)
		}
	})
	return panelClassErr
}

func applyWindowThemeAttributes(hwnd uintptr, dark bool) {
	if hwnd == 0 || procDwmSetWindowAttribute.Find() != nil {
		return
	}
	val := int32(0)
	if dark {
		val = 1
	}
	procDwmSetWindowAttribute.Call(hwnd, dwmwaUseImmersiveDark, uintptr(unsafe.Pointer(&val)), 4)
}

func syncWebViewControllerBounds(hwnd, ctrl uintptr) {
	if hwnd == 0 || ctrl == 0 {
		return
	}
	var cr winRect
	if r, _, _ := procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&cr))); r != 0 {
		comCall(ctrl, 6, uintptr(unsafe.Pointer(&cr))) // ICoreWebView2Controller::put_Bounds
	}
}

func snapshotNativeBounds(hwnd uintptr, prev windowBounds) windowBounds {
	if hwnd == 0 {
		return prev
	}
	if zoomed, _, _ := procIsZoomed.Call(hwnd); zoomed != 0 {
		prev.Maximized = true
		return prev
	}
	var rc winRect
	if r, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&rc))); r != 0 {
		w := rc.Right - rc.Left
		h := rc.Bottom - rc.Top
		if w >= 320 && h >= 240 {
			return windowBounds{X: rc.Left, Y: rc.Top, Width: w, Height: h, Maximized: false}
		}
	}
	return prev
}

func nativePanelWndProc(hwnd, msg, wParam, lParam uintptr) uintptr {
	switch uint32(msg) {
	case wmGetMinMaxInfo:
		applyMinMaxInfo(hwnd, lParam)
		return 0

	case wmSize:
		nativePanel.mu.Lock()
		ctrl := nativePanel.ctrl
		cur := nativePanel.cur
		vis := nativePanel.visible
		if wParam != sizeMinimized {
			if zoomed, _, _ := procIsZoomed.Call(hwnd); zoomed != 0 {
				if !cur.Maximized {
					cur.Maximized = true
					nativePanel.cur = cur
					if vis {
						saveWindowBounds(cur)
					}
				}
			} else if cur.Maximized {
				cur = snapshotNativeBounds(hwnd, cur)
				nativePanel.cur = cur
				if vis {
					saveWindowBounds(cur)
				}
			}
		}
		nativePanel.mu.Unlock()
		if wParam != sizeMinimized && ctrl != 0 {
			syncWebViewControllerBounds(hwnd, ctrl)
		}
		return 0

	case wmMove, wmMoving:
		nativePanel.mu.Lock()
		ctrl := nativePanel.ctrl
		nativePanel.mu.Unlock()
		if ctrl != 0 {
			comCall(ctrl, 23) // NotifyParentWindowPositionChanged
		}
		return 0

	case wmExitSizeMove:
		nativePanel.mu.Lock()
		next := snapshotNativeBounds(hwnd, nativePanel.cur)
		if next != nativePanel.cur {
			nativePanel.cur = next
			saveWindowBounds(next)
		}
		nativePanel.mu.Unlock()
		return 0

	case wmDpiChanged:
		if lParam != 0 {
			var suggested winRect
			procMoveMemory.Call(uintptr(unsafe.Pointer(&suggested)), lParam, unsafe.Sizeof(suggested))
			procSetWindowPos.Call(hwnd, 0,
				uintptr(suggested.Left), uintptr(suggested.Top),
				uintptr(suggested.Right-suggested.Left), uintptr(suggested.Bottom-suggested.Top),
				swpNoZOrder|swpNoActivate)
		}
		nativePanel.mu.Lock()
		ctrl := nativePanel.ctrl
		nativePanel.mu.Unlock()
		if ctrl != 0 {
			syncWebViewControllerBounds(hwnd, ctrl)
		}
		return 0

	case wmActivate:
		if (wParam & 0xFFFF) != 0 {
			nativePanel.mu.Lock()
			ctrl := nativePanel.ctrl
			vis := nativePanel.visible
			nativePanel.mu.Unlock()
			if vis && ctrl != 0 {
				comCall(ctrl, 12, 0) // MoveFocus(COREWEBVIEW2_MOVE_FOCUS_REASON_PROGRAMMATIC)
			}
		}
		return 0

	case wmSettingChange:
		dark := !isWindowsLightTheme()
		nativePanel.mu.Lock()
		changed := nativePanel.dark != dark
		nativePanel.dark = dark
		nativePanel.mu.Unlock()
		if changed {
			applyWindowThemeAttributes(hwnd, dark)
		}
		return 0

	case wmClose:
		nativePanel.mu.Lock()
		next := snapshotNativeBounds(hwnd, nativePanel.cur)
		nativePanel.cur = next
		nativePanel.visible = false
		ctrl := nativePanel.ctrl
		nativePanel.mu.Unlock()
		saveWindowBounds(next)
		if ctrl != 0 {
			comCall(ctrl, 4, 0) // put_IsVisible(false)
		}
		procShowWindow.Call(hwnd, swHide)
		return 0

	case wmPanelShow:
		nativePanel.mu.Lock()
		nativePanel.visible = true
		ctrl := nativePanel.ctrl
		maxed := nativePanel.cur.Maximized
		nativePanel.mu.Unlock()
		if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
			procShowWindow.Call(hwnd, swRestore)
		} else if maxed {
			procShowWindow.Call(hwnd, swMaximize)
		} else {
			procShowWindow.Call(hwnd, swShow)
		}
		if ctrl != 0 {
			syncWebViewControllerBounds(hwnd, ctrl)
			comCall(ctrl, 4, 1)  // put_IsVisible(true)
			comCall(ctrl, 12, 0) // MoveFocus
		}
		procUpdateWindow.Call(hwnd)
		procBringWindowToTop.Call(hwnd)
		procSetForegroundWindow.Call(hwnd)
		return 0

	case wmPanelDestroy:
		nativePanel.mu.Lock()
		next := snapshotNativeBounds(hwnd, nativePanel.cur)
		nativePanel.cur = next
		nativePanel.visible = false
		ctrl := nativePanel.ctrl
		wv := nativePanel.wv
		env := nativePanel.env
		nativePanel.ctrl = 0
		nativePanel.wv = 0
		nativePanel.env = 0
		nativePanel.mu.Unlock()
		saveWindowBounds(next)
		if wv != 0 {
			comRelease(wv)
		}
		if ctrl != 0 {
			comCall(ctrl, 24) // ICoreWebView2Controller::Close
			comRelease(ctrl)
		}
		if env != 0 {
			comRelease(env)
		}
		procDestroyWindow.Call(hwnd)
		return 0

	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}

	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wParam, lParam)
	return r
}

func pumpMessagesUntil(hwnd uintptr, done <-chan struct{}, timeout time.Duration) error {
	procSetTimer.Call(hwnd, timerWebViewInitPump, 25, 0)
	defer procKillTimer.Call(hwnd, timerWebViewInitPump)
	deadline := time.Now().Add(timeout)
	var m winMsg
	for {
		select {
		case <-done:
			return nil
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("WebView2 初始化超时（>%s）", timeout)
		}
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			return errors.New("窗口消息循环提前结束")
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func focusNativePanel() bool {
	nativePanel.mu.Lock()
	hwnd := nativePanel.hwnd
	nativePanel.mu.Unlock()
	if hwnd == 0 {
		return false
	}
	if alive, _, _ := procIsWindow.Call(hwnd); alive == 0 {
		return false
	}
	procPostMessageW.Call(hwnd, wmPanelShow, 0, 0)
	return true
}

func closeNativePanel() {
	nativePanel.mu.Lock()
	hwnd := nativePanel.hwnd
	closedCh := nativePanel.closedCh
	nativePanel.mu.Unlock()
	if hwnd == 0 {
		return
	}
	procPostMessageW.Call(hwnd, wmPanelDestroy, 0, 0)
	if closedCh != nil {
		select {
		case <-closedCh:
		case <-time.After(2 * time.Second):
		}
	}
}

type webviewSession struct {
	env  uintptr
	ctrl uintptr
	wv   uintptr
}

func createWebView2Session(hwnd uintptr, dllPath, version, userDataDir, initialURL string, dark bool) (webviewSession, error) {
	dll, err := syscall.LoadDLL(dllPath)
	if err != nil {
		return webviewSession{}, fmt.Errorf("加载 EmbeddedBrowserWebView.dll 失败：%w", err)
	}
	createEnvProc, err := dll.FindProc("CreateWebViewEnvironmentWithOptionsInternal")
	if err != nil {
		return webviewSession{}, fmt.Errorf("未找到 CreateWebViewEnvironmentWithOptionsInternal：%w", err)
	}

	if err := os.MkdirAll(userDataDir, 0o755); err != nil {
		return webviewSession{}, fmt.Errorf("创建 WebView2 配置目录失败：%w", err)
	}
	uddPtr, err := syscall.UTF16PtrFromString(userDataDir)
	if err != nil {
		return webviewSession{}, err
	}
	optsObj := allocCOMObj(uintptr(unsafe.Pointer(&panelEnvOptionsVtbl[0])), &comState{
		iid:     iidEnvOptions,
		version: version,
	})
	if optsObj == 0 {
		return webviewSession{}, errors.New("allocCOMObj(envOptions) failed")
	}
	defer comRelease(optsObj)

	var envHR, envPtr uintptr
	envDone := make(chan struct{})
	envHandler := allocCOMObj(uintptr(unsafe.Pointer(&panelCompletionVtbl[0])), &comState{
		iid: iidEnvCompleted,
		onComplete: func(hr, obj uintptr) {
			envHR = hr
			envPtr = obj
			close(envDone)
		},
	})
	if envHandler == 0 {
		return webviewSession{}, errors.New("allocCOMObj(envHandler) failed")
	}
	defer comRelease(envHandler)

	hr, _, _ := createEnvProc.Call(
		1, // checkRunningInstance = true
		0, // Evergreen runtimeType
		uintptr(unsafe.Pointer(uddPtr)),
		optsObj,
		envHandler,
	)
	if err := checkHR("CreateWebViewEnvironmentWithOptionsInternal", hr); err != nil {
		return webviewSession{}, err
	}
	if err := pumpMessagesUntil(hwnd, envDone, webviewInitTimeout); err != nil {
		return webviewSession{}, err
	}
	if err := checkHR("EnvironmentCompleted", envHR); err != nil {
		if envPtr != 0 {
			comRelease(envPtr)
		}
		return webviewSession{}, err
	}
	if envPtr == 0 {
		return webviewSession{}, errors.New("EnvironmentCompleted returned nil environment")
	}

	var ctrlHR, ctrlPtr uintptr
	ctrlDone := make(chan struct{})
	ctrlHandler := allocCOMObj(uintptr(unsafe.Pointer(&panelCompletionVtbl[0])), &comState{
		iid: iidCtrlCompleted,
		onComplete: func(hr, obj uintptr) {
			ctrlHR = hr
			ctrlPtr = obj
			close(ctrlDone)
		},
	})
	if ctrlHandler == 0 {
		comRelease(envPtr)
		return webviewSession{}, errors.New("allocCOMObj(ctrlHandler) failed")
	}
	defer comRelease(ctrlHandler)

	hr = comCall(envPtr, 3, hwnd, ctrlHandler) // CreateCoreWebView2Controller
	if err := checkHR("CreateCoreWebView2Controller", hr); err != nil {
		comRelease(envPtr)
		return webviewSession{}, err
	}
	if err := pumpMessagesUntil(hwnd, ctrlDone, webviewInitTimeout); err != nil {
		comRelease(envPtr)
		return webviewSession{}, err
	}
	if err := checkHR("ControllerCompleted", ctrlHR); err != nil {
		if ctrlPtr != 0 {
			comRelease(ctrlPtr)
		}
		comRelease(envPtr)
		return webviewSession{}, err
	}
	if ctrlPtr == 0 {
		comRelease(envPtr)
		return webviewSession{}, errors.New("ControllerCompleted returned nil controller")
	}

	var wvPtr uintptr
	if err := checkHR("get_CoreWebView2", comCall(ctrlPtr, 25, uintptr(unsafe.Pointer(&wvPtr)))); err != nil || wvPtr == 0 {
		comCall(ctrlPtr, 24)
		comRelease(ctrlPtr)
		comRelease(envPtr)
		if err == nil {
			err = errors.New("get_CoreWebView2 returned nil")
		}
		return webviewSession{}, err
	}

	if ctrl2 := comQueryInterface(ctrlPtr, &iidController2); ctrl2 != 0 {
		bg := packColorARGB(0xFF, 0x11, 0x13, 0x17)
		if !dark {
			bg = packColorARGB(0xFF, 0xF1, 0xF4, 0xF9)
		}
		comCall(ctrl2, 27, bg) // ICoreWebView2Controller2::put_DefaultBackgroundColor
		comRelease(ctrl2)
	}

	syncWebViewControllerBounds(hwnd, ctrlPtr)
	if initialURL != "" {
		if urlPtr, uErr := syscall.UTF16PtrFromString(initialURL); uErr == nil {
			_ = checkHR("Navigate", comCall(wvPtr, 5, uintptr(unsafe.Pointer(urlPtr))))
		}
	}

	return webviewSession{env: envPtr, ctrl: ctrlPtr, wv: wvPtr}, nil
}

func launchNativePanel(url string, wb windowBounds) error {
	if focusNativePanel() {
		return nil
	}
	dllPath, version, err := findWebView2Runtime()
	if err != nil {
		return err
	}
	if err := registerPanelClass(); err != nil {
		return err
	}

	readyCh := make(chan error, 1)
	closedCh := make(chan struct{})

	go func() {
		defer close(closedCh)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		if procSetThreadDpiCtx.Find() == nil {
			procSetThreadDpiCtx.Call(dpiAwarenessPerMonitor)
		}
		hr, _, _ := procCoInitializeEx.Call(0, coinitApartment)
		if int32(hr) >= 0 {
			defer procCoUninitialize.Call()
		}

		clsPtr, _ := syscall.UTF16PtrFromString("GBFDockPanelWnd")
		titlePtr, _ := syscall.UTF16PtrFromString(panelTitle)
		hInst, _, _ := procGetModuleHandleW.Call(0)
		hwnd, _, createErr := procCreateWindowExW.Call(
			0,
			uintptr(unsafe.Pointer(clsPtr)),
			uintptr(unsafe.Pointer(titlePtr)),
			wsOverlappedWindow|wsClipChildren,
			uintptr(wb.X), uintptr(wb.Y), uintptr(wb.Width), uintptr(wb.Height),
			0, 0, hInst, 0,
		)
		if hwnd == 0 {
			readyCh <- fmt.Errorf("CreateWindowExW failed: %v", createErr)
			return
		}

		dark := !isWindowsLightTheme()
		applyWindowThemeAttributes(hwnd, dark)

		nativePanel.mu.Lock()
		nativePanel.hwnd = hwnd
		nativePanel.cur = wb
		nativePanel.dark = dark
		nativePanel.visible = false
		nativePanel.closedCh = closedCh
		nativePanel.mu.Unlock()

		userDataDir := filepath.Join(filepath.Dir(windowBoundsPath()), "webview2-profile")
		sess, err := createWebView2Session(hwnd, dllPath, version, userDataDir, url, dark)
		if err != nil {
			log.Printf("内嵌 WebView2 启动失败，准备回退浏览器模式：%v", err)
			nativePanel.mu.Lock()
			nativePanel.hwnd = 0
			nativePanel.closedCh = nil
			nativePanel.mu.Unlock()
			procDestroyWindow.Call(hwnd)
			readyCh <- err
			return
		}

		nativePanel.mu.Lock()
		nativePanel.env = sess.env
		nativePanel.ctrl = sess.ctrl
		nativePanel.wv = sess.wv
		nativePanel.visible = true
		nativePanel.mu.Unlock()

		// Re-apply initial clamped bounds with accurate per-monitor DPI before first show.
		scale := windowDPIScale(hwnd)
		minW := int32(float64(panelMinWidthDIP) * scale)
		minH := int32(float64(panelMinHeightDIP) * scale)
		showWB := wb
		if showWB.Width < minW {
			showWB.Width = minW
		}
		if showWB.Height < minH {
			showWB.Height = minH
		}
		if !showWB.Maximized {
			procSetWindowPos.Call(hwnd, 0, uintptr(showWB.X), uintptr(showWB.Y), uintptr(showWB.Width), uintptr(showWB.Height), swpNoZOrder|swpNoActivate)
		}
		syncWebViewControllerBounds(hwnd, sess.ctrl)
		comCall(sess.ctrl, 4, 1) // put_IsVisible(true)
		if showWB.Maximized {
			procShowWindow.Call(hwnd, swMaximize)
		} else {
			procShowWindow.Call(hwnd, swShow)
		}
		procUpdateWindow.Call(hwnd)
		procBringWindowToTop.Call(hwnd)
		procSetForegroundWindow.Call(hwnd)

		readyCh <- nil

		var m winMsg
		for {
			ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(ret) <= 0 {
				break
			}
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}

		nativePanel.mu.Lock()
		nativePanel.hwnd = 0
		nativePanel.env = 0
		nativePanel.ctrl = 0
		nativePanel.wv = 0
		nativePanel.closedCh = nil
		nativePanel.mu.Unlock()
	}()

	return <-readyCh
}
