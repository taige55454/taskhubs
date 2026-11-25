//go:build windows

package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"os"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

const (
	wmDestroy       = 0x0002
	wmCommand       = 0x0111
	wmUser          = 0x0400
	wmTrayCallback  = wmUser + 1
	wmAppQuit       = wmUser + 2
	wmLButtonUp     = 0x0202
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

	mbYesNo        = 0x00000004
	mbIconQuestion = 0x00000020
	mbIconInfo     = 0x00000040
	idYes          = 6

	errorAlreadyExists = 183

	idMenuProcInfo  = 1001
	idMenuOpenWeb   = 1002
	idMenuAutoStart = 1003
	idMenuFirewall  = 1004
	idMenuResetPwd  = 1005
	idMenuQuitAll   = 1006

	regRunPath      = `Software\Microsoft\Windows\CurrentVersion\Run`
	autoStartAppKey = "TaskHubIntranetServer"
	singleMutexName = `Local\TaskHubIntranetServer_SingleInstance_Mutex`
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	advapi32 = syscall.NewLazyDLL("advapi32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")

	procCreateMutexW        = kernel32.NewProc("CreateMutexW")
	procCloseHandle         = kernel32.NewProc("CloseHandle")
	procGetModuleHandleW    = kernel32.NewProc("GetModuleHandleW")
	procRegisterClassExW    = user32.NewProc("RegisterClassExW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procPostMessageW        = user32.NewProc("PostMessageW")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procLoadIconW           = user32.NewProc("LoadIconW")
	procCreateIconIndirect  = user32.NewProc("CreateIconIndirect")
	procDestroyIcon         = user32.NewProc("DestroyIcon")
	procGetDC               = user32.NewProc("GetDC")
	procReleaseDC           = user32.NewProc("ReleaseDC")
	procMessageBoxW         = user32.NewProc("MessageBoxW")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procAppendMenuW         = user32.NewProc("AppendMenuW")
	procTrackPopupMenu      = user32.NewProc("TrackPopupMenu")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procSetForegroundWnd    = user32.NewProc("SetForegroundWindow")
	procShellNotifyIconW    = shell32.NewProc("Shell_NotifyIconW")
	procShellExecuteW       = shell32.NewProc("ShellExecuteW")
	procCreateDIBSection    = gdi32.NewProc("CreateDIBSection")
	procCreateBitmap        = gdi32.NewProc("CreateBitmap")
	procDeleteObject        = gdi32.NewProc("DeleteObject")
	procRegOpenKeyExW       = advapi32.NewProc("RegOpenKeyExW")
	procRegQueryValueExW    = advapi32.NewProc("RegQueryValueExW")
	procRegSetValueExW      = advapi32.NewProc("RegSetValueExW")
	procRegDeleteValueW     = advapi32.NewProc("RegDeleteValueW")
	procRegCloseKey         = advapi32.NewProc("RegCloseKey")
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

type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

type iconInfo struct {
	fIcon    int32
	xHotspot uint32
	yHotspot uint32
	hbmMask  uintptr
	hbmColor uintptr
}

var trayGlobal struct {
	hwnd     uintptr
	procName string
	store    *Store
	cancel   context.CancelFunc
	logger   *slog.Logger
}

// acquireSingleInstance 创建命名互斥锁，若已运行返回 alreadyRunning = true
func acquireSingleInstance(_, _ string) (func(), bool) {
	namePtr, _ := syscall.UTF16PtrFromString(singleMutexName)
	hMutex, _, lastErr := procCreateMutexW.Call(0, 1, uintptr(unsafe.Pointer(namePtr)))
	if hMutex == 0 {
		return func() {}, false
	}
	if lastErr == syscall.Errno(errorAlreadyExists) {
		_, _, _ = procCloseHandle.Call(hMutex)
		return func() {}, true
	}
	return func() {
		_, _, _ = procCloseHandle.Call(hMutex)
	}, false
}

// createPrettyTrayIcon 内存中用 GDI 渲染 64x64 抗锯齿苹果蓝圆角渐变 + 纯白对勾高清图标
func createPrettyTrayIcon() (uintptr, bool) {
	const sz = 64
	var bi bitmapInfoHeader
	bi.biSize = uint32(unsafe.Sizeof(bi))
	bi.biWidth = sz
	bi.biHeight = -sz // Top-down DIB
	bi.biPlanes = 1
	bi.biBitCount = 32
	bi.biCompression = 0 // BI_RGB

	hdc, _, _ := procGetDC.Call(0)
	var bitsPtr uintptr
	hbmColor, _, _ := procCreateDIBSection.Call(
		hdc,
		uintptr(unsafe.Pointer(&bi)),
		0,
		uintptr(unsafe.Pointer(&bitsPtr)),
		0, 0,
	)
	if hdc != 0 {
		_, _, _ = procReleaseDC.Call(0, hdc)
	}
	if hbmColor == 0 || bitsPtr == 0 {
		hDef, _, _ := procLoadIconW.Call(0, uintptr(idiApplication))
		return hDef, false
	}

	pixels := unsafe.Slice((*uint32)(unsafe.Pointer(bitsPtr)), sz*sz)
	distSeg := func(px, py, ax, ay, bx, by float64) float64 {
		vx, vy := bx-ax, by-ay
		wx, wy := px-ax, py-ay
		c1 := wx*vx + wy*vy
		if c1 <= 0 {
			return math.Hypot(px-ax, py-ay)
		}
		c2 := vx*vx + vy*vy
		if c2 <= c1 {
			return math.Hypot(px-bx, py-by)
		}
		b := c1 / c2
		return math.Hypot(px-(ax+b*vx), py-(ay+b*vy))
	}
	sdRoundBox := func(px, py, cx, cy, half, rad float64) float64 {
		dx := math.Abs(px-cx) - (half - rad)
		dy := math.Abs(py-cy) - (half - rad)
		ox, oy := math.Max(dx, 0), math.Max(dy, 0)
		return math.Hypot(ox, oy) + math.Min(math.Max(dx, dy), 0) - rad
	}

	for y := 0; y < sz; y++ {
		t := float64(y) / float64(sz-1)
		// 顶部亮天蓝 (#2E90FA) -> 底部苹果蓝 (#0066EE)
		bgR := 46.0*(1-t) + 0.0*t
		bgG := 144.0*(1-t) + 102.0*t
		bgB := 250.0*(1-t) + 238.0*t

		for x := 0; x < sz; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			dBox := sdRoundBox(fx, fy, 32.0, 32.0, 29.0, 14.0)
			alpha := math.Max(0, math.Min(1, 0.5-dBox))
			if alpha <= 0 {
				pixels[y*sz+x] = 0
				continue
			}

			// 白色圆润对勾轨迹：(18,33) -> (28,43) -> (47,22)
			dChk := math.Min(
				distSeg(fx, fy, 18.5, 33.5, 27.5, 42.5),
				distSeg(fx, fy, 27.5, 42.5, 46.5, 22.0),
			)
			chkMix := math.Max(0, math.Min(1, 4.2-dChk))

			r := bgR*(1-chkMix) + 255.0*chkMix
			g := bgG*(1-chkMix) + 255.0*chkMix
			b := bgB*(1-chkMix) + 255.0*chkMix

			// Win32 ARGB 预乘透明度
			aByte := uint32(alpha*255.0 + 0.5)
			rByte := uint32(r*alpha + 0.5)
			gByte := uint32(g*alpha + 0.5)
			bByte := uint32(b*alpha + 0.5)
			pixels[y*sz+x] = (aByte << 24) | (rByte << 16) | (gByte << 8) | bByte
		}
	}

	maskBytes := make([]byte, (sz*sz)/8)
	hbmMask, _, _ := procCreateBitmap.Call(sz, sz, 1, 1, uintptr(unsafe.Pointer(&maskBytes[0])))

	ii := iconInfo{
		fIcon:    1,
		hbmMask:  hbmMask,
		hbmColor: hbmColor,
	}
	hIcon, _, _ := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))

	// CreateIconIndirect 已拷贝位图数据，立即释放临时 HBITMAP 防 GDI 句柄泄漏
	if hbmMask != 0 {
		_, _, _ = procDeleteObject.Call(hbmMask)
	}
	_, _, _ = procDeleteObject.Call(hbmColor)

	if hIcon == 0 {
		hDef, _, _ := procLoadIconW.Call(0, uintptr(idiApplication))
		return hDef, false
	}
	return hIcon, true
}

func runTray(ctx context.Context, cancel context.CancelFunc, procName string, store *Store, logger *slog.Logger) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	trayGlobal.procName = procName
	trayGlobal.store = store
	trayGlobal.cancel = cancel
	trayGlobal.logger = logger

	hInst, _, _ := procGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString("TaskHubTrayWndClass")
	wndTitle, _ := syscall.UTF16PtrFromString("TaskHubTrayHiddenWindow")

	hIcon, customIcon := createPrettyTrayIcon()
	if customIcon {
		defer procDestroyIcon.Call(hIcon)
	}

	wc := wndClassExW{
		lpfnWndProc:   syscall.NewCallback(trayWndProc),
		hInstance:     hInst,
		hIcon:         hIcon,
		hIconSm:       hIcon,
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
		logger.Warn("创建托盘窗口失败，退化为后台等待模式")
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

	tipRunes, _ := syscall.UTF16FromString("岗位协作系统（左键打开页面 / 右键管理） - " + procName)
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
		case wmLButtonUp, wmLButtonDblClk:
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
		case idMenuFirewall:
			allowWindowsFirewallRule(hwnd)
		case idMenuResetPwd:
			confirmAndResetAdminPwd(hwnd)
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
	procLabel, _ := syscall.UTF16PtrFromString("运行进程: " + trayGlobal.procName)
	openLabel, _ := syscall.UTF16PtrFromString("🌐 打开管理网页 (" + localURL + ")")
	autoLabel, _ := syscall.UTF16PtrFromString("🚀 开机静默自启")
	fwLabel, _ := syscall.UTF16PtrFromString("🛡️ 一键放行局域网防火墙 (手机无法访问时点此)")
	resetLabel, _ := syscall.UTF16PtrFromString("🔑 忘记密码？重置管理员密码为 admin123")
	quitLabel, _ := syscall.UTF16PtrFromString("⏹ 彻底关闭程序 (退出服务)")

	_, _, _ = procAppendMenuW.Call(hMenu, mfString|mfGrayed, idMenuProcInfo, uintptr(unsafe.Pointer(procLabel)))
	_, _, _ = procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
	_, _, _ = procAppendMenuW.Call(hMenu, mfString, idMenuOpenWeb, uintptr(unsafe.Pointer(openLabel)))

	var autoFlags uintptr = mfString
	if isWindowsAutoStartEnabled() {
		autoFlags |= mfChecked
	}
	_, _, _ = procAppendMenuW.Call(hMenu, autoFlags, idMenuAutoStart, uintptr(unsafe.Pointer(autoLabel)))
	_, _, _ = procAppendMenuW.Call(hMenu, mfString, idMenuFirewall, uintptr(unsafe.Pointer(fwLabel)))
	_, _, _ = procAppendMenuW.Call(hMenu, mfString, idMenuResetPwd, uintptr(unsafe.Pointer(resetLabel)))
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

func confirmAndResetAdminPwd(hwnd uintptr) {
	if trayGlobal.store == nil {
		return
	}
	qTxt, _ := syscall.UTF16PtrFromString("确定要将主机管理员的登录密码重置为初始密码「admin123」吗？")
	qTitle, _ := syscall.UTF16PtrFromString("重置管理员密码确认")
	ret, _, _ := procMessageBoxW.Call(hwnd, uintptr(unsafe.Pointer(qTxt)), uintptr(unsafe.Pointer(qTitle)), mbYesNo|mbIconQuestion)
	if ret != idYes {
		return
	}
	if err := trayGlobal.store.ResetAdminPasswords(context.Background()); err != nil {
		errTxt, _ := syscall.UTF16PtrFromString("重置失败：" + err.Error())
		_, _, _ = procMessageBoxW.Call(hwnd, uintptr(unsafe.Pointer(errTxt)), uintptr(unsafe.Pointer(qTitle)), mbIconInfo)
		return
	}
	okTxt, _ := syscall.UTF16PtrFromString("管理员密码已成功重置为：admin123\n现在将为您打开登录页面，登录后请及时修改新密码。")
	_, _, _ = procMessageBoxW.Call(hwnd, uintptr(unsafe.Pointer(okTxt)), uintptr(unsafe.Pointer(qTitle)), mbIconInfo)
	openBrowser(getTrayLocalURL())
}

// allowWindowsFirewallRule 按需唤起 UAC 为当前 exe 添加 Windows 防火墙入站放行规则
func allowWindowsFirewallRule(hwnd uintptr) {
	exePath, err := os.Executable()
	if err != nil {
		return
	}
	localURL := getTrayLocalURL()
	port := "8080"
	if idx := strings.LastIndex(localURL, ":"); idx != -1 {
		port = localURL[idx+1:]
	}
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, _ := syscall.UTF16PtrFromString("netsh.exe")
	argsStr := fmt.Sprintf(`advfirewall firewall add rule name="TaskHub Intranet (%s)" dir=in action=allow program="%s" enable=yes profile=private,domain,public`, port, exePath)
	args, _ := syscall.UTF16PtrFromString(argsStr)
	_, _, _ = procShellExecuteW.Call(hwnd, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)), uintptr(unsafe.Pointer(args)), 0, 0)
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
		0, 0, 0,
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
	// 开机自启时附加 -autostart 参数，静默驻留托盘不自动弹窗打扰
	cmdUTF16, err := syscall.UTF16FromString(`"` + exePath + `" -autostart`)
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
