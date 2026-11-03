package main

// Windows system-tray icon, autostart (HKCU\...\Run), window focusing and shortcuts.
// Pure syscall, no external packages.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

var (
	procRegisterClassExW         = user32.NewProc("RegisterClassExW")
	procCreateWindowExW          = user32.NewProc("CreateWindowExW")
	procDefWindowProcW           = user32.NewProc("DefWindowProcW")
	procGetMessageW              = user32.NewProc("GetMessageW")
	procTranslateMessage         = user32.NewProc("TranslateMessage")
	procDispatchMessageW         = user32.NewProc("DispatchMessageW")
	procPostQuitMessage          = user32.NewProc("PostQuitMessage")
	procDestroyWindow            = user32.NewProc("DestroyWindow")
	procRegisterWindowMessageW   = user32.NewProc("RegisterWindowMessageW")
	procLoadImageW               = user32.NewProc("LoadImageW")
	procLoadIconW                = user32.NewProc("LoadIconW")
	procGetSystemMetrics         = user32.NewProc("GetSystemMetrics")
	procCreatePopupMenu          = user32.NewProc("CreatePopupMenu")
	procAppendMenuW              = user32.NewProc("AppendMenuW")
	procTrackPopupMenu           = user32.NewProc("TrackPopupMenu")
	procDestroyMenu              = user32.NewProc("DestroyMenu")
	procGetCursorPos             = user32.NewProc("GetCursorPos")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procShowWindow               = user32.NewProc("ShowWindow")
	procIsIconic                 = user32.NewProc("IsIconic")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procAllowSetForegroundWindow = user32.NewProc("AllowSetForegroundWindow")
	procShellNotifyIconW         = shell32.NewProc("Shell_NotifyIconW")
	kernel32                     = syscall.NewLazyDLL("kernel32.dll")
	procGetModuleHandleW         = kernel32.NewProc("GetModuleHandleW")
	advapi32                     = syscall.NewLazyDLL("advapi32.dll")
	procRegSetValueExW           = advapi32.NewProc("RegSetValueExW")
	procRegDeleteValueW          = advapi32.NewProc("RegDeleteValueW")
)

const (
	wmDestroy       = 0x0002
	wmNull          = 0x0000
	wmContextMenu   = 0x007B
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
	wmApp           = 0x8000
	trayCallbackMsg = wmApp + 1
	trayWakeMsg     = wmApp + 2
	ninBalloonClick = 0x0405

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2
	nifMsg    = 0x01
	nifIcon   = 0x02
	nifTip    = 0x04
	nifInfo   = 0x10
	niifUser  = 0x04
	niifLarge = 0x20

	mfString    = 0x0000
	mfGrayed    = 0x0001
	mfChecked   = 0x0008
	mfSeparator = 0x0800
	tpmReturn   = 0x0100
	tpmNoNotify = 0x0080
	tpmRight    = 0x0002
	tpmBottom   = 0x0020
)

type notifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         [16]byte
	HBalloonIcon     uintptr
}

type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type winMsg struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
	Private uint32
}

type TrayItem struct {
	Text      string
	Checked   bool
	Disabled  bool
	Separator bool
	Action    func()
}

var tray struct {
	mu             sync.Mutex
	hwnd           uintptr
	nid            notifyIconData
	taskbarCreated uint32
	cmds           chan func()
	onOpen         func()
	menu           func() []TrayItem
	balloonAction  func()
}

func copyUTF16(dst []uint16, s string) {
	u, _ := syscall.UTF16FromString(s)
	if len(u) > len(dst) {
		u = u[:len(dst)]
		u[len(u)-1] = 0
	}
	for i := range dst {
		dst[i] = 0
	}
	copy(dst, u)
}

func trayWndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	switch uint32(msg) {
	case trayCallbackMsg:
		switch uint32(lparam & 0xFFFF) {
		case wmLButtonUp, wmLButtonDblClk:
			go tray.onOpen()
		case ninBalloonClick:
			tray.mu.Lock()
			act := tray.balloonAction
			tray.balloonAction = nil
			tray.mu.Unlock()
			if act != nil {
				go act()
			} else {
				go tray.onOpen()
			}
		case 0x0404: // NIN_BALLOONTIMEOUT
			tray.mu.Lock()
			tray.balloonAction = nil
			tray.mu.Unlock()
		case wmRButtonUp, wmContextMenu:
			trayShowMenu(hwnd)
		}
		return 0
	case trayWakeMsg:
		for {
			select {
			case f := <-tray.cmds:
				f()
			default:
				return 0
			}
		}
	case wmDestroy:
		procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&tray.nid)))
		procPostQuitMessage.Call(0)
		return 0
	}
	if tray.taskbarCreated != 0 && uint32(msg) == tray.taskbarCreated {
		// Explorer restarted: put the icon back.
		procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&tray.nid)))
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

func trayShowMenu(hwnd uintptr) {
	items := tray.menu()
	h, _, _ := procCreatePopupMenu.Call()
	if h == 0 {
		return
	}
	defer procDestroyMenu.Call(h)
	for i, it := range items {
		if it.Separator {
			procAppendMenuW.Call(h, mfSeparator, 0, 0)
			continue
		}
		flags := uintptr(mfString)
		if it.Checked {
			flags |= mfChecked
		}
		if it.Disabled {
			flags |= mfGrayed
		}
		t, _ := syscall.UTF16PtrFromString(it.Text)
		procAppendMenuW.Call(h, flags, uintptr(i+1), uintptr(unsafe.Pointer(t)))
	}
	var pt struct{ X, Y int32 }
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(hwnd)
	id, _, _ := procTrackPopupMenu.Call(h, tpmReturn|tpmNoNotify|tpmRight|tpmBottom,
		uintptr(pt.X), uintptr(pt.Y), 0, hwnd, 0)
	procPostMessageW.Call(hwnd, wmNull, 0, 0)
	if id > 0 && int(id) <= len(items) && items[id-1].Action != nil {
		go items[id-1].Action()
	}
}

func trayRun(f func()) {
	tray.mu.Lock()
	h := tray.hwnd
	tray.mu.Unlock()
	if h == 0 {
		return
	}
	select {
	case tray.cmds <- f:
		procPostMessageW.Call(h, trayWakeMsg, 0, 0)
	default:
	}
}

// startTray creates the tray icon on its own OS thread. done is closed when it goes away.
func startTray(tip string, onOpen func(), menu func() []TrayItem) (ok bool, done chan struct{}) {
	tray.cmds = make(chan func(), 32)
	tray.onOpen = onOpen
	tray.menu = menu
	ready := make(chan bool)
	done = make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer close(done)
		hInst, _, _ := procGetModuleHandleW.Call(0)
		cls, _ := syscall.UTF16PtrFromString("PhoneOnPCTrayWindow")
		wc := wndClassEx{LpfnWndProc: syscall.NewCallback(trayWndProc), HInstance: hInst, LpszClassName: cls}
		wc.CbSize = uint32(unsafe.Sizeof(wc))
		if r, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
			ready <- false
			return
		}
		title, _ := syscall.UTF16PtrFromString("Телефон на ПК")
		hwnd, _, _ := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(title)),
			0, 0, 0, 0, 0, 0, 0, hInst, 0)
		if hwnd == 0 {
			ready <- false
			return
		}
		tc, _ := syscall.UTF16PtrFromString("TaskbarCreated")
		r, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(tc)))
		tray.taskbarCreated = uint32(r)

		cx, _, _ := procGetSystemMetrics.Call(49) // SM_CXSMICON
		cy, _, _ := procGetSystemMetrics.Call(50)
		icon, _, _ := procLoadImageW.Call(hInst, 1, 1, cx, cy, 0) // resource #1, IMAGE_ICON
		if icon == 0 {
			icon, _, _ = procLoadIconW.Call(0, 32512) // IDI_APPLICATION
		}
		nid := &tray.nid
		nid.CbSize = uint32(unsafe.Sizeof(*nid))
		nid.HWnd = hwnd
		nid.UID = 1
		nid.UFlags = nifMsg | nifIcon | nifTip
		nid.UCallbackMessage = trayCallbackMsg
		nid.HIcon = icon
		copyUTF16(nid.SzTip[:], tip)
		if r, _, _ := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(nid))); r == 0 {
			procDestroyWindow.Call(hwnd)
			ready <- false
			return
		}
		tray.mu.Lock()
		tray.hwnd = hwnd
		tray.mu.Unlock()
		ready <- true

		var m winMsg
		for {
			r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
			if int32(r) <= 0 {
				break
			}
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		}
		tray.mu.Lock()
		tray.hwnd = 0
		tray.mu.Unlock()
	}()
	return <-ready, done
}

func trayNotify(title, text string) {
	trayRun(func() {
		nid := &tray.nid
		nid.UFlags = nifInfo
		copyUTF16(nid.SzInfoTitle[:], title)
		copyUTF16(nid.SzInfo[:], text)
		nid.DwInfoFlags = niifUser | niifLarge | 0x10 // NIIF_NOSOUND: our own sound plays instead
		procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(nid)))
		nid.UFlags = nifMsg | nifIcon | nifTip
		nid.SzInfo[0] = 0
	})
}

func traySetTip(text string) {
	trayRun(func() {
		nid := &tray.nid
		copyUTF16(nid.SzTip[:], text)
		nid.UFlags = nifMsg | nifIcon | nifTip
		procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(nid)))
	})
}

func trayQuit() {
	trayRun(func() { procDestroyWindow.Call(tray.hwnd) })
}

// ---------------------------------------------------------------- app window focus

var (
	focusMu    sync.Mutex
	focusOnce  sync.Once
	focusCb    uintptr
	focusTitle string
	focusHwnd  uintptr
)

// focusAppWindow brings the already open UI window to the front. Returns false if none.
func initFocusCb() {
	{
		focusCb = syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
			if vis, _, _ := procIsWindowVisible.Call(hwnd); vis == 0 {
				return 1
			}
			var buf [256]uint16
			n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), 256)
			if n > 0 && strings.HasPrefix(syscall.UTF16ToString(buf[:n]), focusTitle) {
				focusHwnd = hwnd
				return 0
			}
			return 1
		})
	}
}

func focusAppWindow(titlePrefix string) bool {
	h := findAppWindow(titlePrefix)
	if h == 0 {
		return false
	}
	if ic, _, _ := procIsIconic.Call(h); ic != 0 {
		procShowWindow.Call(h, 9) // SW_RESTORE
	}
	procSetForegroundWindow.Call(h)
	return true
}

func findAppWindow(titlePrefix string) uintptr {
	focusOnce.Do(initFocusCb)
	focusMu.Lock()
	defer focusMu.Unlock()
	focusTitle = titlePrefix
	focusHwnd = 0
	procEnumWindows.Call(focusCb, 0)
	return focusHwnd
}

func allowOtherForeground() { procAllowSetForegroundWindow.Call(^uintptr(0)) } // ASFW_ANY

// ---------------------------------------------------------------- autostart

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`
const runValue = "Телефон на ПК"

func autostartSupported() bool { return true }

func autostartEnabled() bool {
	var k syscall.Handle
	kp, _ := syscall.UTF16PtrFromString(runKey)
	if syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, kp, 0, syscall.KEY_QUERY_VALUE, &k) != nil {
		return false
	}
	defer syscall.RegCloseKey(k)
	vp, _ := syscall.UTF16PtrFromString(runValue)
	var buf [1024]uint16
	n := uint32(len(buf) * 2)
	var typ uint32
	if syscall.RegQueryValueEx(k, vp, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &n) != nil {
		return false
	}
	exe, _ := os.Executable()
	return strings.Contains(strings.ToLower(syscall.UTF16ToString(buf[:])), strings.ToLower(exe))
}

func setAutostart(on bool) error {
	var k syscall.Handle
	kp, _ := syscall.UTF16PtrFromString(runKey)
	if err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, kp, 0, syscall.KEY_SET_VALUE, &k); err != nil {
		return err
	}
	defer syscall.RegCloseKey(k)
	vp, _ := syscall.UTF16PtrFromString(runValue)
	if !on {
		procRegDeleteValueW.Call(uintptr(k), uintptr(unsafe.Pointer(vp)))
		return nil
	}
	exe, _ := os.Executable()
	data, _ := syscall.UTF16FromString(`"` + exe + `" --tray`)
	r, _, _ := procRegSetValueExW.Call(uintptr(k), uintptr(unsafe.Pointer(vp)), 0, 1, // REG_SZ
		uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)*2))
	if r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

// ---------------------------------------------------------------- shortcuts

var (
	folderDesktop  = guid{0xB4BFCC3A, 0xDB2C, 0x424C, [8]byte{0xB0, 0x29, 0x7F, 0xE9, 0x9A, 0x87, 0xC6, 0x41}}
	folderPrograms = guid{0xA77F5D77, 0x2E2B, 0x44C3, [8]byte{0xA6, 0xA2, 0xAB, 0xA6, 0x01, 0x05, 0x4A, 0x51}}
)

func shortcutsSupported() bool { return true }

// createShortcut makes "Телефон на ПК.lnk" via PowerShell + WScript.Shell
// (paths go through env vars, so Cyrillic and spaces are safe).
func createShortcut(where string) (string, error) {
	id := &folderDesktop
	if where == "startmenu" {
		id = &folderPrograms
	}
	dir := knownFolderByID(id)
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, "Desktop")
	}
	exe, _ := os.Executable()
	lnk := filepath.Join(dir, "Телефон на ПК.lnk")
	script := `$s=(New-Object -ComObject WScript.Shell).CreateShortcut($env:PL_LNK);` +
		`$s.TargetPath=$env:PL_EXE;$s.WorkingDirectory=$env:PL_DIR;$s.IconLocation=$env:PL_EXE+',0';` +
		`$s.Description='Экран телефона на ПК (scrcpy)';$s.Save()`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.Env = append(os.Environ(), "PL_LNK="+lnk, "PL_EXE="+exe, "PL_DIR="+filepath.Dir(exe))
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if _, e := os.Stat(lnk); e != nil {
		if err == nil {
			err = e
		}
		return "", &shortcutErr{strings.TrimSpace(string(out)), err}
	}
	return lnk, nil
}

type shortcutErr struct {
	out string
	err error
}

func (e *shortcutErr) Error() string {
	if e.out != "" {
		return e.out
	}
	return e.err.Error()
}

// killOtherInstances ends other running copies of this app (old versions).
func killOtherInstances() {
	exe, _ := os.Executable()
	cmd := exec.Command("taskkill", "/F", "/IM", filepath.Base(exe), "/FI", "PID ne "+strconv.Itoa(os.Getpid()))
	hideWindow(cmd)
	cmd.Run()
}

// fixAutostartPath: if autostart points to another copy of the app (an old folder),
// point it at this one.
func fixAutostartPath() {
	var k syscall.Handle
	kp, _ := syscall.UTF16PtrFromString(runKey)
	if syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, kp, 0, syscall.KEY_QUERY_VALUE, &k) != nil {
		return
	}
	vp, _ := syscall.UTF16PtrFromString(runValue)
	var buf [1024]uint16
	n := uint32(len(buf) * 2)
	var typ uint32
	err := syscall.RegQueryValueEx(k, vp, nil, &typ, (*byte)(unsafe.Pointer(&buf[0])), &n)
	syscall.RegCloseKey(k)
	if err == nil && !autostartEnabled() {
		setAutostart(true)
	}
}

// trayNotifyAction shows a Windows notification; clicking it runs act.
func trayNotifyAction(title, text string, act func()) {
	tray.mu.Lock()
	tray.balloonAction = act
	tray.mu.Unlock()
	trayNotify(title, text)
}

func foregroundPid() int {
	fg, _, _ := procGetForegroundWindow.Call()
	if fg == 0 {
		return 0
	}
	var pid uint32
	procGetWindowThreadPID.Call(fg, uintptr(unsafe.Pointer(&pid)))
	return int(pid)
}
