//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	procExpandEnvironmentStringsW = kernel32.NewProc("ExpandEnvironmentStringsW")
	procQueryFullProcessImageNameW = kernel32.NewProc("QueryFullProcessImageNameW")
	procGetWindowThreadProcessId  = user32.NewProc("GetWindowThreadProcessId")
	procMessageBoxW               = user32.NewProc("MessageBoxW")

	clsidShellLink = winGUID{0x00021401, 0x0000, 0x0000, [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
	iidIShellLinkW = winGUID{0x000214F9, 0x0000, 0x0000, [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
	iidIPersistFile = winGUID{0x0000010B, 0x0000, 0x0000, [8]byte{0xC0, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x46}}
)

type comdlgFilterSpec struct {
	Name *uint16
	Spec *uint16
}

func expandWindowsEnvVars(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	src, err := syscall.UTF16PtrFromString(s)
	if err != nil {
		return s
	}
	buf := make([]uint16, 1024)
	n, _, _ := procExpandEnvironmentStringsW.Call(
		uintptr(unsafe.Pointer(src)),
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(len(buf)),
	)
	runtime.KeepAlive(src)
	if n == 0 || int(n) > len(buf) {
		return s
	}
	return syscall.UTF16ToString(buf[:n])
}

func readRegAppPath(root syscall.Handle, exeName string) string {
	subkey, err := syscall.UTF16PtrFromString(`Software\Microsoft\Windows\CurrentVersion\App Paths\` + exeName)
	if err != nil {
		return ""
	}
	var hk syscall.Handle
	if syscall.RegOpenKeyEx(root, subkey, 0, syscall.KEY_READ, &hk) != nil {
		return ""
	}
	defer syscall.RegCloseKey(hk)

	var typ, size uint32
	if syscall.RegQueryValueEx(hk, nil, nil, &typ, nil, &size) != nil || (typ != 1 && typ != 2) || size < 2 {
		return ""
	}
	buf := make([]uint16, size/2+1)
	if syscall.RegQueryValueEx(hk, nil, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &size) != nil {
		return ""
	}
	val := strings.Trim(strings.TrimSpace(syscall.UTF16ToString(buf)), `"`)
	if typ == 2 {
		val = expandWindowsEnvVars(val)
	}
	return val
}

// detectInstalledBrowsers 扫描本机常见 Chromium 系浏览器的安装路径（注册表 App Paths + 标准安装目录）。
func detectInstalledBrowsers() []browserCandidate {
	localApp := os.Getenv("LOCALAPPDATA")
	progFiles := os.Getenv("ProgramFiles")
	progX86 := os.Getenv("ProgramFiles(x86)")
	userProfile := os.Getenv("USERPROFILE")

	type browserSpec struct {
		name      string
		regExes   []string
		stdPaths  []string
	}
	specs := []browserSpec{
		{
			name:    "Google Chrome",
			regExes: []string{"chrome.exe"},
			stdPaths: []string{
				filepath.Join(progFiles, `Google\Chrome\Application\chrome.exe`),
				filepath.Join(progX86, `Google\Chrome\Application\chrome.exe`),
				filepath.Join(localApp, `Google\Chrome\Application\chrome.exe`),
			},
		},
		{
			name:    "Microsoft Edge",
			regExes: []string{"msedge.exe"},
			stdPaths: []string{
				filepath.Join(progX86, `Microsoft\Edge\Application\msedge.exe`),
				filepath.Join(progFiles, `Microsoft\Edge\Application\msedge.exe`),
				filepath.Join(localApp, `Microsoft\Edge\Application\msedge.exe`),
			},
		},
		{
			name:    "Brave",
			regExes: []string{"brave.exe"},
			stdPaths: []string{
				filepath.Join(progFiles, `BraveSoftware\Brave-Browser\Application\brave.exe`),
				filepath.Join(progX86, `BraveSoftware\Brave-Browser\Application\brave.exe`),
				filepath.Join(localApp, `BraveSoftware\Brave-Browser\Application\brave.exe`),
			},
		},
		{
			name:    "Vivaldi",
			regExes: []string{"vivaldi.exe"},
			stdPaths: []string{
				filepath.Join(localApp, `Vivaldi\Application\vivaldi.exe`),
				filepath.Join(progFiles, `Vivaldi\Application\vivaldi.exe`),
			},
		},
		{
			name: "Cent Browser (百分浏览器)",
			stdPaths: []string{
				filepath.Join(localApp, `CentBrowser\Application\chrome.exe`),
				filepath.Join(progFiles, `CentBrowser\Application\chrome.exe`),
				filepath.Join(progX86, `CentBrowser\Application\chrome.exe`),
			},
		},
		{
			name: "Opera",
			stdPaths: []string{
				filepath.Join(localApp, `Programs\Opera\opera.exe`),
				filepath.Join(localApp, `Programs\Opera\launcher.exe`),
				filepath.Join(progFiles, `Opera\opera.exe`),
				filepath.Join(localApp, `Programs\Opera GX\opera.exe`),
			},
		},
		{
			name: "Chromium",
			stdPaths: []string{
				filepath.Join(localApp, `Chromium\Application\chrome.exe`),
				filepath.Join(progFiles, `Chromium\Application\chrome.exe`),
				filepath.Join(userProfile, `scoop\apps\chromium\current\chrome.exe`),
			},
		},
		{
			name: "360 极速浏览器",
			stdPaths: []string{
				filepath.Join(localApp, `360ChromeX\Chrome\Application\360chromeX.exe`),
				filepath.Join(localApp, `360Chrome\Chrome\Application\360chrome.exe`),
			},
		},
	}

	var out []browserCandidate
	seen := make(map[string]bool)
	tryAdd := func(name, candPath string) {
		p := strings.Trim(strings.TrimSpace(candPath), `"`)
		if p == "" {
			return
		}
		p = filepath.Clean(p)
		key := strings.ToLower(p)
		if seen[key] {
			return
		}
		st, err := os.Stat(p)
		if err != nil || st.IsDir() {
			return
		}
		seen[key] = true
		lbl := name
		if strings.Contains(strings.ToLower(p), "centbrowser") {
			lbl = "Cent Browser (百分浏览器)"
		}
		out = append(out, browserCandidate{
			Name: lbl,
			Path: p,
			Kind: "exe",
		})
	}

	for _, sp := range specs {
		for _, rExe := range sp.regExes {
			tryAdd(sp.name, readRegAppPath(syscall.HKEY_CURRENT_USER, rExe))
			tryAdd(sp.name, readRegAppPath(syscall.HKEY_LOCAL_MACHINE, rExe))
		}
		for _, p := range sp.stdPaths {
			tryAdd(sp.name, p)
		}
	}
	return out
}

// pickBrowserFileDialog 调用原生 Windows COM IFileOpenDialog 选择浏览器程序 (.exe) 或快捷方式 (.lnk)。
// 显式设置 FOS_NODEREFERENCELINKS，确保选中 .lnk 时返回快捷方式本身，从而保留用户快捷方式里的启动参数。
func pickBrowserFileDialog(title string) (string, bool, error) {
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
		if int32(hr) < 0 && uint32(hr) != 0x80010106 {
			ch <- res{err: fmt.Errorf("初始化文件选择框失败（0x%08X）", uint32(hr))}
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
			ch <- res{err: fmt.Errorf("无法创建文件选择框（0x%08X）", uint32(hr))}
			return
		}
		dlgPtr := uintptr(unsafe.Pointer(dlg))
		defer syscall.SyscallN(dlg.vtbl[2], dlgPtr) // IUnknown::Release

		f1n, _ := syscall.UTF16PtrFromString("浏览器或快捷方式 (*.exe;*.lnk)")
		f1s, _ := syscall.UTF16PtrFromString("*.exe;*.lnk")
		f2n, _ := syscall.UTF16PtrFromString("浏览器快捷方式 (*.lnk)")
		f2s, _ := syscall.UTF16PtrFromString("*.lnk")
		f3n, _ := syscall.UTF16PtrFromString("浏览器程序 (*.exe)")
		f3s, _ := syscall.UTF16PtrFromString("*.exe")
		specs := []comdlgFilterSpec{
			{Name: f1n, Spec: f1s},
			{Name: f2n, Spec: f2s},
			{Name: f3n, Spec: f3s},
		}
		syscall.SyscallN(dlg.vtbl[4], dlgPtr, uintptr(len(specs)), uintptr(unsafe.Pointer(&specs[0]))) // SetFileTypes
		syscall.SyscallN(dlg.vtbl[5], dlgPtr, 1)                                                       // SetFileTypeIndex (1-based)
		runtime.KeepAlive(specs)
		runtime.KeepAlive(f1n)
		runtime.KeepAlive(f1s)
		runtime.KeepAlive(f2n)
		runtime.KeepAlive(f2s)
		runtime.KeepAlive(f3n)
		runtime.KeepAlive(f3s)

		var opts uint32
		syscall.SyscallN(dlg.vtbl[10], dlgPtr, uintptr(unsafe.Pointer(&opts))) // GetOptions
		const (
			fosForceFilesystem   = 0x00000040
			fosFileMustExist     = 0x00001000
			fosNoDereferenceLink = 0x00100000
		)
		opts |= fosForceFilesystem | fosFileMustExist | fosNoDereferenceLink
		syscall.SyscallN(dlg.vtbl[9], dlgPtr, uintptr(opts)) // SetOptions

		if strings.TrimSpace(title) != "" {
			tPtr, _ := syscall.UTF16PtrFromString(title)
			syscall.SyscallN(dlg.vtbl[17], dlgPtr, uintptr(unsafe.Pointer(tPtr))) // SetTitle
			runtime.KeepAlive(tPtr)
		}

		owner := findPanelWindow(panelTitle)
		showHR, _, _ := syscall.SyscallN(dlg.vtbl[3], dlgPtr, owner) // Show
		if uint32(showHR) == 0x800704C7 {                            // ERROR_CANCELLED
			ch <- res{cancelled: true}
			return
		}
		if int32(showHR) < 0 {
			ch <- res{err: fmt.Errorf("文件选择框打开失败（0x%08X）", uint32(showHR))}
			return
		}

		var item *comObj
		resHR, _, _ := syscall.SyscallN(dlg.vtbl[20], dlgPtr, uintptr(unsafe.Pointer(&item))) // GetResult
		if int32(resHR) < 0 || item == nil {
			ch <- res{err: fmt.Errorf("读取所选文件失败（0x%08X）", uint32(resHR))}
			return
		}
		itemPtr := uintptr(unsafe.Pointer(item))
		defer syscall.SyscallN(item.vtbl[2], itemPtr) // Release

		var pszPath uintptr
		dnHR, _, _ := syscall.SyscallN(item.vtbl[5], itemPtr, 0x80058000, uintptr(unsafe.Pointer(&pszPath))) // SIGDN_FILESYSPATH
		if int32(dnHR) < 0 || pszPath == 0 {
			ch <- res{err: fmt.Errorf("解析文件路径失败（0x%08X）", uint32(dnHR))}
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

// resolveShortcutLnk 使用 COM IShellLinkW + IPersistFile 读取 Windows 快捷方式的目标程序、参数与工作目录。
func resolveShortcutLnk(lnkPath string) (targetPath, args, workDir string, err error) {
	type res struct {
		target  string
		args    string
		workDir string
		err     error
	}
	ch := make(chan res, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		hr, _, _ := procCoInitializeEx.Call(0, 0x2|0x4)
		if int32(hr) < 0 && uint32(hr) != 0x80010106 {
			ch <- res{err: fmt.Errorf("初始化快捷方式解析失败（0x%08X）", uint32(hr))}
			return
		}
		if int32(hr) >= 0 {
			defer procCoUninitialize.Call()
		}

		var link *comObj
		hr, _, _ = procCoCreateInstance.Call(
			uintptr(unsafe.Pointer(&clsidShellLink)),
			0,
			0x1, // CLSCTX_INPROC_SERVER
			uintptr(unsafe.Pointer(&iidIShellLinkW)),
			uintptr(unsafe.Pointer(&link)),
		)
		if int32(hr) < 0 || link == nil {
			ch <- res{err: fmt.Errorf("创建 IShellLinkW 失败（0x%08X）", uint32(hr))}
			return
		}
		linkPtr := uintptr(unsafe.Pointer(link))
		defer syscall.SyscallN(link.vtbl[2], linkPtr) // Release

		var pf *comObj
		hr, _, _ = syscall.SyscallN(link.vtbl[0], linkPtr, uintptr(unsafe.Pointer(&iidIPersistFile)), uintptr(unsafe.Pointer(&pf))) // QueryInterface
		if int32(hr) < 0 || pf == nil {
			ch <- res{err: fmt.Errorf("获取 IPersistFile 接口失败（0x%08X）", uint32(hr))}
			return
		}
		pfPtr := uintptr(unsafe.Pointer(pf))
		defer syscall.SyscallN(pf.vtbl[2], pfPtr) // Release

		lnkPtr, pErr := syscall.UTF16PtrFromString(lnkPath)
		if pErr != nil {
			ch <- res{err: fmt.Errorf("快捷方式路径无效：%w", pErr)}
			return
		}
		hr, _, _ = syscall.SyscallN(pf.vtbl[5], pfPtr, uintptr(unsafe.Pointer(lnkPtr)), 0) // IPersistFile::Load(STGM_READ)
		runtime.KeepAlive(lnkPtr)
		if int32(hr) < 0 {
			ch <- res{err: fmt.Errorf("无法读取快捷方式内容（0x%08X）", uint32(hr))}
			return
		}

		targetBuf := make([]uint16, 1024)
		hr, _, _ = syscall.SyscallN(link.vtbl[3], linkPtr, uintptr(unsafe.Pointer(&targetBuf[0])), uintptr(len(targetBuf)), 0, 0) // GetPath
		var tgt string
		if int32(hr) >= 0 {
			tgt = expandWindowsEnvVars(strings.TrimSpace(syscall.UTF16ToString(targetBuf)))
		}

		argBuf := make([]uint16, 4096)
		hr, _, _ = syscall.SyscallN(link.vtbl[10], linkPtr, uintptr(unsafe.Pointer(&argBuf[0])), uintptr(len(argBuf))) // GetArguments
		var rawArgs string
		if int32(hr) >= 0 {
			rawArgs = strings.TrimSpace(syscall.UTF16ToString(argBuf))
		}

		wdBuf := make([]uint16, 1024)
		hr, _, _ = syscall.SyscallN(link.vtbl[8], linkPtr, uintptr(unsafe.Pointer(&wdBuf[0])), uintptr(len(wdBuf))) // GetWorkingDirectory
		var wd string
		if int32(hr) >= 0 {
			wd = expandWindowsEnvVars(strings.TrimSpace(syscall.UTF16ToString(wdBuf)))
		}

		ch <- res{target: tgt, args: rawArgs, workDir: wd}
	}()
	r := <-ch
	return r.target, r.args, r.workDir, r.err
}

// launchBrowserProcess 以独立子进程方式启动指定浏览器，不继承阻塞。
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

// defaultBrowserUserDataDir 返回常见 Chromium 系浏览器的默认用户配置目录（User Data）。
func defaultBrowserUserDataDir(exePath string) string {
	cleanExe := filepath.Clean(exePath)
	// 先检查绿色/便携版同级 User Data（如百分浏览器便携版）
	appDir := filepath.Dir(cleanExe)
	for _, cand := range []string{
		filepath.Join(appDir, "User Data"),
		filepath.Join(filepath.Dir(appDir), "User Data"),
	} {
		if st, err := os.Stat(filepath.Join(cand, "Local State")); err == nil && !st.IsDir() {
			return cand
		}
	}

	localApp := os.Getenv("LOCALAPPDATA")
	appData := os.Getenv("APPDATA")
	low := strings.ToLower(cleanExe)
	base := strings.ToLower(filepath.Base(cleanExe))

	switch {
	case strings.Contains(low, "centbrowser"):
		return filepath.Join(localApp, `CentBrowser\User Data`)
	case strings.Contains(low, "360chromex"):
		return filepath.Join(localApp, `360ChromeX\Chrome\User Data`)
	case strings.Contains(low, "360chrome"):
		return filepath.Join(localApp, `360Chrome\Chrome\User Data`)
	case base == "msedge.exe" && localApp != "":
		return filepath.Join(localApp, `Microsoft\Edge\User Data`)
	case base == "brave.exe" && localApp != "":
		return filepath.Join(localApp, `BraveSoftware\Brave-Browser\User Data`)
	case base == "vivaldi.exe" && localApp != "":
		return filepath.Join(localApp, `Vivaldi\User Data`)
	case strings.Contains(low, "chromium") && localApp != "":
		return filepath.Join(localApp, `Chromium\User Data`)
	case base == "chrome.exe" && localApp != "":
		return filepath.Join(localApp, `Google\Chrome\User Data`)
	case strings.Contains(low, "opera gx") && appData != "":
		return filepath.Join(appData, `Opera Software\Opera GX Stable`)
	case strings.Contains(low, "opera") && appData != "":
		return filepath.Join(appData, `Opera Software\Opera Stable`)
	}
	return ""
}

// isUserDataDirLocked 通过独占方式尝试打开 Chromium User Data 下的 lockfile，判断该配置目录是否被占用。
func isUserDataDirLocked(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	lockPath := filepath.Join(dir, "lockfile")
	pPtr, err := syscall.UTF16PtrFromString(lockPath)
	if err != nil {
		return false
	}
	h, err := syscall.CreateFile(
		pPtr,
		syscall.GENERIC_READ,
		0, // 独占尝试：Chromium 持有 lockfile 时会返回 ERROR_SHARING_VIOLATION
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err == nil {
		_ = syscall.CloseHandle(h)
		return false
	}
	const errorSharingViolation syscall.Errno = 32
	return err == errorSharingViolation
}

func getWindowPID(hwnd uintptr) uint32 {
	if hwnd == 0 {
		return 0
	}
	var pid uint32
	procGetWindowThreadProcessId.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return pid
}

func getPanelProcessID() uint32 {
	return getWindowPID(findPanelWindow(panelTitle))
}

func queryProcessImagePath(pid uint32) string {
	const processQueryLimitedInfo = 0x1000
	h, err := syscall.OpenProcess(processQueryLimitedInfo, false, pid)
	if err != nil || h == 0 {
		return ""
	}
	defer syscall.CloseHandle(h)
	buf := make([]uint16, 1024)
	sz := uint32(len(buf))
	r, _, _ := procQueryFullProcessImageNameW.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&sz)))
	if r == 0 || sz == 0 {
		return ""
	}
	return filepath.Clean(syscall.UTF16ToString(buf[:sz]))
}

func findMatchingBrowserPIDs(targetExe string, excludeRootPID uint32) []uint32 {
	wantExe := strings.ToLower(filepath.Clean(targetExe))
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer syscall.CloseHandle(snap)

	type procNode struct {
		pid  uint32
		ppid uint32
	}
	var nodes []procNode
	children := make(map[uint32][]uint32)

	var pe syscall.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	if err := syscall.Process32First(snap, &pe); err != nil {
		return nil
	}
	for {
		nodes = append(nodes, procNode{pid: pe.ProcessID, ppid: pe.ParentProcessID})
		children[pe.ParentProcessID] = append(children[pe.ParentProcessID], pe.ProcessID)
		if err := syscall.Process32Next(snap, &pe); err != nil {
			break
		}
	}

	excluded := make(map[uint32]bool)
	if excludeRootPID != 0 {
		queue := []uint32{excludeRootPID}
		for len(queue) > 0 {
			cur := queue[0]
			queue = queue[1:]
			if excluded[cur] {
				continue
			}
			excluded[cur] = true
			queue = append(queue, children[cur]...)
		}
	}

	var matched []uint32
	for _, n := range nodes {
		if excluded[n.pid] || n.pid == 0 {
			continue
		}
		if img := queryProcessImagePath(n.pid); img != "" && strings.ToLower(img) == wantExe {
			matched = append(matched, n.pid)
		}
	}
	return matched
}

// isBrowserInstanceRunning 判断目标浏览器（或指定 User Data 目录）当前是否处于打开/后台驻留状态。
func isBrowserInstanceRunning(targetExe, explicitUserDataDir string) bool {
	checkDir := strings.TrimSpace(explicitUserDataDir)
	if checkDir == "" {
		checkDir = defaultBrowserUserDataDir(targetExe)
	}
	if checkDir != "" {
		if isUserDataDirLocked(checkDir) {
			return true
		}
		if _, err := os.Stat(filepath.Join(checkDir, "lockfile")); err == nil {
			// lockfile 存在但已未被锁住：说明目标配置目录当前处于关闭状态
			return false
		}
	}
	return len(findMatchingBrowserPIDs(targetExe, getPanelProcessID())) > 0
}

var (
	enumCloseMu   sync.Mutex
	enumClosePIDs map[uint32]bool
	enumCloseOnce sync.Once
	enumClosePtr  uintptr
)

func postCloseBrowserWindows(pids []uint32) {
	enumCloseOnce.Do(func() {
		enumClosePtr = syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
			if vis, _, _ := procIsWindowVisible.Call(hwnd); vis == 0 {
				return 1
			}
			pid := getWindowPID(hwnd)
			if pid != 0 && enumClosePIDs[pid] {
				procPostMessageW.Call(hwnd, wmClose, 0, 0)
			}
			return 1
		})
	})

	pidSet := make(map[uint32]bool, len(pids))
	for _, pid := range pids {
		pidSet[pid] = true
	}

	enumCloseMu.Lock()
	enumClosePIDs = pidSet
	procEnumWindows.Call(enumClosePtr, 0)
	enumClosePIDs = nil
	enumCloseMu.Unlock()
}

// closeBrowserProcessesGracefully 温和关闭选定浏览器的旧窗口与后台驻留进程（排除 GBF-Dock 自身控制面板窗口）。
func closeBrowserProcessesGracefully(targetExe, explicitUserDataDir string) error {
	panelPID := getPanelProcessID()
	pids := findMatchingBrowserPIDs(targetExe, panelPID)
	if len(pids) == 0 && !isBrowserInstanceRunning(targetExe, explicitUserDataDir) {
		return nil
	}

	if len(pids) > 0 {
		postCloseBrowserWindows(pids)
	}

	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		panelPID = getPanelProcessID()
		if !isBrowserInstanceRunning(targetExe, explicitUserDataDir) && len(findMatchingBrowserPIDs(targetExe, panelPID)) == 0 {
			return nil
		}
		time.Sleep(120 * time.Millisecond)
	}

	// 若仍有无界面后台驻留进程（如 Edge/Chrome 关闭窗口后继续在后台运行），清理剩余进程以释放配置锁
	panelPID = getPanelProcessID()
	rem := findMatchingBrowserPIDs(targetExe, panelPID)
	for _, pid := range rem {
		const processTerminate = 0x0001
		if h, err := syscall.OpenProcess(processTerminate, false, pid); err == nil && h != 0 {
			_ = syscall.TerminateProcess(h, 0)
			_ = syscall.CloseHandle(h)
		}
	}

	releaseDeadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(releaseDeadline) {
		if !isBrowserInstanceRunning(targetExe, explicitUserDataDir) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("旧浏览器进程未能完全退出，请手动关闭后重试")
}

func confirmRestartBrowserDialog(browserLabel string) bool {
	title, _ := syscall.UTF16PtrFromString(appName + " v" + appVersion)
	text, _ := syscall.UTF16PtrFromString(fmt.Sprintf(
		"检测到「%s」当前正在运行（含后台进程）。\n\n保留原配置进行独立加速启动需要先关闭该浏览器的旧进程，重启后会自动保留您的账号登录与扩展，并在关闭浏览器时自动清除加速参数。\n\n是否立即重启「%s」并加速启动？",
		browserLabel, browserLabel,
	))
	const (
		mbYesNo        = 0x00000004
		mbIconQuestion = 0x00000020
		mbTopmost      = 0x00040000
		idYes          = 6
	)
	ret, _, _ := procMessageBoxW.Call(0, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(title)), mbYesNo|mbIconQuestion|mbTopmost)
	return ret == idYes
}
