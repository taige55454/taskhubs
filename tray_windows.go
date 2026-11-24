//go:build windows

package main

import (
	"context"
	"log/slog"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

const (
	wmDestroy       = 0x0002
	wmCommand       = 0x0111
	wmUser          = 0x0400
	wmTrayCallback  = wmUser + 1
	wmAppQuit       = wmUser + 2
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205

	nimAdd    = 0x00000000
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004

	idiApplication = 32512

	mfString    = 0x00000000
	mfGrayed    = 0x00000001
	mfChecked   = 0x00000008
	mfSeparator = 0x00000800

	tpmRightButton = 0x0002
	tpmBottomAlign = 0x0020

	idMenuProcInfo  = 1001
	idMenuOpenWeb   = 1002
	idMenuAutoStart = 1003
	idMenuQuitAll   = 1004

	regRunPath      = `Software\Microsoft\Windows\CurrentVersion\Run`
	autoStartAppKey = "TaskHubIntranetServer"
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")

	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	procRegisterClassExW = user32.NewProc("RegisterClassExW")
	procCreateWindowExW  = user32.NewProc("CreateWindowExW")
	procDefWindowProcW   = user32.NewProc("DefWindowProcW")
	procGetMessageW      = user32.NewProc("GetMessageW")
	procTranslateMessage = user32.NewProc("TranslateMessage")
	procDispatchMessageW = user32.NewProc("DispatchMessageW")
	procPostQuitMessage  = user32.NewProc("PostQuitMessage")
	procPostMessageW     = user32.NewProc("PostMessageW")
	procDestroyWindow    = user32.NewProc("DestroyWindow")
	procLoadIconW        = user32.NewProc("LoadIconW")
	procCreatePopupMenu  = user32.NewProc("CreatePopupMenu")
	procAppendMenuW      = user32.NewProc("AppendMenuW")
	procTrackPopupMenu   = user32.NewProc("TrackPopupMenu")
	procDestroyMenu      = user32.NewProc("DestroyMenu")
	procGetCursorPos     = user32.NewProc("GetCursorPos")
	procSetForegroundWnd = user32.NewProc("SetForegroundWindow")
	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
	procRegOpenKeyExW    = advapi32.NewProc("RegOpenKeyExW")
	procRegQueryValueExW = advapi32.NewProc("RegQueryValueExW")
	procRegSetValueExW   = advapi32.NewProc("RegSetValueExW")
	procRegDeleteValueW  = advapi32.NewProc("RegDeleteValueW")
	procRegCloseKey      = advapi32.NewProc("RegCloseKey")
)

type wndClassExW struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     uintptr
	hIcon         uintptr
	hCursor       uintptr
	hbrBackground uintptr
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       uintptr
}

type point struct {
	x int32
	y int32
}

type winMsg struct {
	hwnd     uintptr
	message  uint32
	wParam   uintptr
	lParam   uintptr
	time     uint32
	pt       point
	lPrivate uint32
}

type notifyIconDataW struct {
	cbSize           uint32
	hWnd             uintptr
	uID              uint32
	uFlags           uint32
	uCallbackMessage uint32
	hIcon            uintptr
	szTip            [128]uint16
	dwState          uint32
	dwStateMask      uint32
	szInfo           [256]uint16
	uVersion         uint32
	szInfoTitle      [64]uint16
	dwInfoFlags      uint32
	guidItem         [16]byte
	hBalloonIcon     uintptr
}

var trayGlobal struct {
	hwnd     uintptr
	procName string
	cancel   context.CancelFunc
	logger   *slog.Logger
}

func runTray(ctx context.Context, cancel context.CancelFunc, procName string, logger *slog.Logger) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	trayGlobal.procName = procName
	trayGlobal.cancel = cancel
	trayGlobal.logger = logger

	hInst, _, _ := procGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString("TaskHubTrayWndClass")
	wndTitle, _ := syscall.UTF16PtrFromString("TaskHubTrayHiddenWindow")
	hIcon, _, _ := procLoadIconW.Call(0, uintptr(idiApplication))

	wc := wndClassExW{
		lpfnWndProc:   syscall.NewCallback(trayWndProc),
		hInstance:     hInst,
		hIcon:         hIcon,
		lpszClassName: className,
	}
	wc.cbSize = uint32(unsafe.Sizeof(wc))
	_, _, _ = procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	hwnd, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(wndTitle)),
		0,
		0, 0, 0, 0,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		logger.Warn("创建托盘消息窗口失败，退化为后台等待模式")
		<-ctx.Done()
		return
	}
	trayGlobal.hwnd = hwnd

	var nid notifyIconDataW
	nid.cbSize = uint32(unsafe.Sizeof(nid))
	nid.hWnd = hwnd
	nid.uID = 1
	nid.uFlags = nifMessage | nifIcon | nifTip
	nid.uCallbackMessage = wmTrayCallback
	nid.hIcon = hIcon

	tipRunes, _ := syscall.UTF16FromString("内网岗位任务系统 - " + procName)
	copy(nid.szTip[:127], tipRunes)

	_, _, _ = procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
	defer func() {
		_, _, _ = procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
	}()

	go func() {
		<-ctx.Done()
		_, _, _ = procPostMessageW.Call(hwnd, wmAppQuit, 0, 0)
	}()

	var msg winMsg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		_, _, _ = procTranslateMessage.Call(uintptr(unsafe.Pointer(&msg)))
		_, _, _ = procDispatchMessageW.Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func trayWndProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	switch msg {
	case wmTrayCallback:
		switch uint32(lParam) {
		case wmLButtonDblClk:
			openBrowser(getTrayLocalURL())
		case wmRButtonUp:
			showTrayMenu(hwnd)
		}
		return 0

	case wmCommand:
		cmdID := int(wParam & 0xffff)
		switch cmdID {
		case idMenuOpenWeb:
			openBrowser(getTrayLocalURL())
		case idMenuAutoStart:
			enabled := isWindowsAutoStartEnabled()
			if err := setWindowsAutoStart(!enabled); err != nil && trayGlobal.logger != nil {
				trayGlobal.logger.Error("切换开机自启失败", "err", err)
			}
		case idMenuQuitAll:
			if trayGlobal.cancel != nil {
				trayGlobal.cancel()
			}
			_, _, _ = procDestroyWindow.Call(hwnd)
		}
		return 0

	case wmAppQuit:
		_, _, _ = procDestroyWindow.Call(hwnd)
		return 0

	case wmDestroy:
		_, _, _ = procPostQuitMessage.Call(0)
		return 0
	}

	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
}

func showTrayMenu(hwnd uintptr) {
	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hMenu)

	localURL := getTrayLocalURL()
	procLabel, _ := syscall.UTF16PtrFromString("进程: " + trayGlobal.procName)
	openLabel, _ := syscall.UTF16PtrFromString("打开管理后台 (" + localURL + ")")
	autoLabel, _ := syscall.UTF16PtrFromString("开机自动启动")
	quitLabel, _ := syscall.UTF16PtrFromString("彻底关闭程序 (退出所有进程)")

	_, _, _ = procAppendMenuW.Call(hMenu, mfString|mfGrayed, idMenuProcInfo, uintptr(unsafe.Pointer(procLabel)))
	_, _, _ = procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
	_, _, _ = procAppendMenuW.Call(hMenu, mfString, idMenuOpenWeb, uintptr(unsafe.Pointer(openLabel)))

	var autoFlags uintptr = mfString
	if isWindowsAutoStartEnabled() {
		autoFlags |= mfChecked
	}
	_, _, _ = procAppendMenuW.Call(hMenu, autoFlags, idMenuAutoStart, uintptr(unsafe.Pointer(autoLabel)))
	_, _, _ = procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
	_, _, _ = procAppendMenuW.Call(hMenu, mfString, idMenuQuitAll, uintptr(unsafe.Pointer(quitLabel)))

	var pt point
	_, _, _ = procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	_, _, _ = procSetForegroundWnd.Call(hwnd)
	_, _, _ = procTrackPopupMenu.Call(
		hMenu,
		tpmRightButton|tpmBottomAlign,
		uintptr(pt.x),
		uintptr(pt.y),
		0,
		hwnd,
		0,
	)
}

func isWindowsAutoStartEnabled() bool {
	subKey, _ := syscall.UTF16PtrFromString(regRunPath)
	var hKey uintptr
	r0, _, _ := procRegOpenKeyExW.Call(
		uintptr(syscall.HKEY_CURRENT_USER),
		uintptr(unsafe.Pointer(subKey)),
		0,
		uintptr(syscall.KEY_READ),
		uintptr(unsafe.Pointer(&hKey)),
	)
	if r0 != 0 {
		return false
	}
	defer procRegCloseKey.Call(hKey)

	valName, _ := syscall.UTF16PtrFromString(autoStartAppKey)
	var dataLen uint32
	r1, _, _ := procRegQueryValueExW.Call(
		hKey,
		uintptr(unsafe.Pointer(valName)),
		0,
		0,
		0,
		uintptr(unsafe.Pointer(&dataLen)),
	)
	return r1 == 0 && dataLen > 0
}

func setWindowsAutoStart(enable bool) error {
	subKey, _ := syscall.UTF16PtrFromString(regRunPath)
	var hKey uintptr
	r0, _, _ := procRegOpenKeyExW.Call(
		uintptr(syscall.HKEY_CURRENT_USER),
		uintptr(unsafe.Pointer(subKey)),
		0,
		uintptr(syscall.KEY_SET_VALUE),
		uintptr(unsafe.Pointer(&hKey)),
	)
	if r0 != 0 {
		return syscall.Errno(r0)
	}
	defer procRegCloseKey.Call(hKey)

	valName, _ := syscall.UTF16PtrFromString(autoStartAppKey)
	if !enable {
		rDel, _, _ := procRegDeleteValueW.Call(hKey, uintptr(unsafe.Pointer(valName)))
		if rDel != 0 && syscall.Errno(rDel) != syscall.ERROR_FILE_NOT_FOUND {
			return syscall.Errno(rDel)
		}
		return nil
	}

	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	cmdUTF16, err := syscall.UTF16FromString(`"` + exePath + `"`)
	if err != nil {
		return err
	}
	byteLen := uint32(len(cmdUTF16) * 2)
	rSet, _, _ := procRegSetValueExW.Call(
		hKey,
		uintptr(unsafe.Pointer(valName)),
		0,
		uintptr(syscall.REG_SZ),
		uintptr(unsafe.Pointer(&cmdUTF16[0])),
		uintptr(byteLen),
	)
	if rSet != 0 {
		return syscall.Errno(rSet)
	}
	return nil
}
