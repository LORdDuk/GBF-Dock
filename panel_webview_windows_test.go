//go:build windows

package main

import (
	"runtime"
	"syscall"
	"testing"
	"unsafe"
)

func TestMinMaxInfoLayoutAndClamp(t *testing.T) {
	if sz := unsafe.Sizeof(minMaxInfo{}); sz != 40 {
		t.Fatalf("sizeof(minMaxInfo) = %d, want 40", sz)
	}
	if off := unsafe.Offsetof(minMaxInfo{}.PtMinTrackSize); off != 24 {
		t.Fatalf("offsetof(PtMinTrackSize) = %d, want 24", off)
	}

	var mmi minMaxInfo
	applyMinMaxInfo(0, uintptr(unsafe.Pointer(&mmi)))
	scale := windowDPIScale(0)
	wantW := int32(float64(panelMinWidthDIP) * scale)
	wantH := int32(float64(panelMinHeightDIP) * scale)
	if mmi.PtMinTrackSize.X != wantW || mmi.PtMinTrackSize.Y != wantH {
		t.Fatalf("applyMinMaxInfo(0) got (%d,%d), want (%d,%d)", mmi.PtMinTrackSize.X, mmi.PtMinTrackSize.Y, wantW, wantH)
	}

	if err := registerPanelClass(); err != nil {
		t.Fatalf("registerPanelClass failed: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		clsPtr, _ := syscall.UTF16PtrFromString("GBFDockPanelWnd")
		titlePtr, _ := syscall.UTF16PtrFromString("GBF-Dock MMI Test")
		hInst, _, _ := procGetModuleHandleW.Call(0)
		hwnd, _, createErr := procCreateWindowExW.Call(
			0,
			uintptr(unsafe.Pointer(clsPtr)),
			uintptr(unsafe.Pointer(titlePtr)),
			wsOverlappedWindow|wsClipChildren,
			0, 0, 800, 700,
			0, 0, hInst, 0,
		)
		if hwnd == 0 {
			done <- createErr
			return
		}
		defer procDestroyWindow.Call(hwnd)

		var msgMMI minMaxInfo
		procSendMessageW.Call(hwnd, wmGetMinMaxInfo, 0, uintptr(unsafe.Pointer(&msgMMI)))
		winScale := windowDPIScale(hwnd)
		expW := int32(float64(panelMinWidthDIP) * winScale)
		expH := int32(float64(panelMinHeightDIP) * winScale)
		if msgMMI.PtMinTrackSize.X != expW || msgMMI.PtMinTrackSize.Y != expH {
			t.Errorf("WM_GETMINMAXINFO PtMinTrackSize = (%d,%d), want (%d,%d)",
				msgMMI.PtMinTrackSize.X, msgMMI.PtMinTrackSize.Y, expW, expH)
		}
		done <- nil
	}()
	if err := <-done; err != nil {
		t.Fatalf("window MMI test failed: %v", err)
	}
}

func TestWebView2RuntimeDiscoveryAndSmoke(t *testing.T) {
	dllPath, ver, err := findWebView2Runtime()
	if err != nil {
		t.Skipf("WebView2 runtime unavailable on this runner: %v", err)
	}
	t.Logf("Discovered WebView2 %s at %s", ver, dllPath)

	if err := registerPanelClass(); err != nil {
		t.Fatalf("registerPanelClass: %v", err)
	}

	resCh := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		hr, _, _ := procCoInitializeEx.Call(0, coinitApartment)
		if int32(hr) >= 0 {
			defer procCoUninitialize.Call()
		}

		clsPtr, _ := syscall.UTF16PtrFromString("GBFDockPanelWnd")
		titlePtr, _ := syscall.UTF16PtrFromString("GBF-Dock WebView2 Smoke")
		hInst, _, _ := procGetModuleHandleW.Call(0)
		hwnd, _, createErr := procCreateWindowExW.Call(
			0,
			uintptr(unsafe.Pointer(clsPtr)),
			uintptr(unsafe.Pointer(titlePtr)),
			wsOverlappedWindow|wsClipChildren,
			0, 0, 720, 620,
			0, 0, hInst, 0,
		)
		if hwnd == 0 {
			resCh <- createErr
			return
		}
		defer procDestroyWindow.Call(hwnd)

		sess, err := createWebView2Session(hwnd, dllPath, ver, t.TempDir(), "about:blank", true)
		if err != nil {
			resCh <- err
			return
		}
		comRelease(sess.wv)
		comCall(sess.ctrl, 24) // Close
		comRelease(sess.ctrl)
		comRelease(sess.env)
		resCh <- nil
	}()

	if err := <-resCh; err != nil {
		t.Fatalf("WebView2 hidden session smoke test failed: %v", err)
	}
}
