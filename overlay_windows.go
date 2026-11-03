package main

// A small vertical toolbar glued to each scrcpy window: Screenshot / Record / File / Camera.
// It never takes focus (WS_EX_NOACTIVATE), so the keyboard keeps going to the phone.
// Shown only while the scrcpy window is the active one.

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	gdi32                     = syscall.NewLazyDLL("gdi32.dll")
	comdlg32                  = syscall.NewLazyDLL("comdlg32.dll")
	dwmapi                    = syscall.NewLazyDLL("dwmapi.dll")
	procBeginPaint            = user32.NewProc("BeginPaint")
	procEndPaint              = user32.NewProc("EndPaint")
	procInvalidateRect        = user32.NewProc("InvalidateRect")
	procSetWindowPos          = user32.NewProc("SetWindowPos")
	procGetWindowRect         = user32.NewProc("GetWindowRect")
	procIsWindow              = user32.NewProc("IsWindow")
	procIsZoomed              = user32.NewProc("IsZoomed")
	procGetForegroundWindow   = user32.NewProc("GetForegroundWindow")
	procSetTimer              = user32.NewProc("SetTimer")
	procKillTimer             = user32.NewProc("KillTimer")
	procTrackMouseEvent       = user32.NewProc("TrackMouseEvent")
	procLoadCursorW           = user32.NewProc("LoadCursorW")
	procMonitorFromWindow     = user32.NewProc("MonitorFromWindow")
	procGetMonitorInfoW       = user32.NewProc("GetMonitorInfoW")
	procSetWindowRgn          = user32.NewProc("SetWindowRgn")
	procSetLayeredWindowAttrs = user32.NewProc("SetLayeredWindowAttributes")
	procFillRect              = user32.NewProc("FillRect")
	procDrawTextW             = user32.NewProc("DrawTextW")
	procPeekMessageW          = user32.NewProc("PeekMessageW")
	procPostThreadMessageW    = user32.NewProc("PostThreadMessageW")
	procSetWinEventHook       = user32.NewProc("SetWinEventHook")
	procUnhookWinEvent        = user32.NewProc("UnhookWinEvent")
	procGetDpiForWindow       = user32.NewProc("GetDpiForWindow")
	procSetThreadDpiAwareness = user32.NewProc("SetThreadDpiAwarenessContext")
	procGetCurrentThreadId    = kernel32.NewProc("GetCurrentThreadId")
	procCreateCompatibleDC    = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBmp   = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject          = gdi32.NewProc("SelectObject")
	procDeleteObject          = gdi32.NewProc("DeleteObject")
	procDeleteDC              = gdi32.NewProc("DeleteDC")
	procBitBlt                = gdi32.NewProc("BitBlt")
	procCreateSolidBrush      = gdi32.NewProc("CreateSolidBrush")
	procCreateFontW           = gdi32.NewProc("CreateFontW")
	procSetTextColor          = gdi32.NewProc("SetTextColor")
	procSetBkMode             = gdi32.NewProc("SetBkMode")
	procRoundRect             = gdi32.NewProc("RoundRect")
	procGetStockObject        = gdi32.NewProc("GetStockObject")
	procCreateRoundRectRgn    = gdi32.NewProc("CreateRoundRectRgn")
	procGetOpenFileNameW      = comdlg32.NewProc("GetOpenFileNameW")
	procDwmGetWindowAttribute = dwmapi.NewProc("DwmGetWindowAttribute")
)

type rect struct{ L, T, R, B int32 }

type paintStruct struct {
	Hdc         uintptr
	FErase      int32
	RcPaint     rect
	FRestore    int32
	FIncUpdate  int32
	RgbReserved [32]byte
}

type trackMouseEvent struct {
	CbSize    uint32
	DwFlags   uint32
	HwndTrack uintptr
	HoverTime uint32
}

type monitorInfo struct {
	CbSize    uint32
	RcMonitor rect
	RcWork    rect
	DwFlags   uint32
}

type openFileName struct {
	LStructSize       uint32
	HwndOwner         uintptr
	HInstance         uintptr
	LpstrFilter       *uint16
	LpstrCustomFilter *uint16
	NMaxCustFilter    uint32
	NFilterIndex      uint32
	LpstrFile         *uint16
	NMaxFile          uint32
	LpstrFileTitle    *uint16
	NMaxFileTitle     uint32
	LpstrInitialDir   *uint16
	LpstrTitle        *uint16
	Flags             uint32
	NFileOffset       uint16
	NFileExtension    uint16
	LpstrDefExt       *uint16
	LCustData         uintptr
	LpfnHook          uintptr
	LpTemplateName    *uint16
	PvReserved        uintptr
	DwReserved        uint32
	FlagsEx           uint32
}

const (
	btnShot = iota
	btnRec
	btnFile
	btnCam
	btnApps
	btnScreen
	btnSound
	btnRemote
	btnCount
)

// Remote column: one tap each, no menus (indices start at btnCount).
var remSpecs = []struct{ act, icon, label string }{
	{"back", "\ue72b", "Назад"}, {"home", "\ue80f", "Домой"}, {"recents", "\ue7c4", "Недавние"}, {"shade", "\ue91c", "Шторка"},
	{"vol_down", "\ue993", "Тише"}, {"vol_up", "\ue995", "Громче"}, {"mute", "\ue74f", "Без звука"},
	{"bright_down", "\uec8a", "Темнее"}, {"bright_up", "\ue706", "Ярче"},
	{"dnd", "\ue708", "Не беспок."}, {"torch", "\uea80", "Фонарик"}, {"rotate", "\ue7ad", "Повернуть"}, {"lock", "\ue72e", "Блок"},
}

const mainBtns = btnRemote // main column: everything before the old "Пульт" button

func totalBtns() int { return btnCount + len(remSpecs) }

type tbFlash struct {
	text  string
	color uint32
	until time.Time
}

type toolbar struct {
	hwnd, target uintptr
	hook         uintptr
	pid          uint32
	id, serial   string
	hover        int
	pressed      int
	tracking     bool
	busy         [btnCount + 13]bool
	flash        [btnCount + 13]tbFlash
	scale        float64
	w, h         int32
	lastPos      rect
	lastFps      int
	shown        bool
	iconFont     uintptr
	smallIcon    uintptr
	tinyFont     uintptr
	labelFont    uintptr
	app          *App
}

var ov struct {
	once     sync.Once
	ok       bool
	tid      uintptr
	cmds     chan func()
	bars     map[uintptr]*toolbar // by toolbar hwnd
	byID     map[string]*toolbar
	cls      *uint16
	hInst    uintptr
	ourPid   uint32
	eventCb  uintptr
	wndProc  uintptr
	findMu   sync.Mutex
	findPid  uint32
	findHwnd uintptr
	findCb   uintptr
	hkApp    *App
}

const ovWake = wmApp + 5

func ovStart() bool {
	ov.once.Do(func() {
		ov.cmds = make(chan func(), 64)
		ov.bars = map[uintptr]*toolbar{}
		ov.byID = map[string]*toolbar{}
		ov.ourPid = uint32(os.Getpid())
		ready := make(chan bool)
		go func() {
			runtime.LockOSThread()
			if procSetThreadDpiAwareness.Find() == nil {
				procSetThreadDpiAwareness.Call(^uintptr(3)) // PER_MONITOR_AWARE_V2 (-4)
			}
			syscall.NewLazyDLL("ole32.dll").NewProc("CoInitializeEx").Call(0, 2) // STA, for the file dialog
			tid, _, _ := procGetCurrentThreadId.Call()
			var m winMsg
			procPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, 0) // create the queue
			ov.hInst, _, _ = procGetModuleHandleW.Call(0)
			ov.cls, _ = syscall.UTF16PtrFromString("PhoneOnPCToolbar")
			ov.wndProc = syscall.NewCallback(tbWndProc)
			ov.eventCb = syscall.NewCallback(tbWinEvent)
			cur, _, _ := procLoadCursorW.Call(0, 32649) // IDC_HAND
			wc := wndClassEx{Style: 3, LpfnWndProc: ov.wndProc, HInstance: ov.hInst, HCursor: cur, LpszClassName: ov.cls}
			wc.CbSize = uint32(unsafe.Sizeof(wc))
			if r, _, _ := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
				ready <- false
				return
			}
			ov.tid = tid
			ready <- true
			for {
				r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
				if int32(r) <= 0 {
					return
				}
				if m.HWnd == 0 && m.Message == ovWake {
					ovDrain()
					continue
				}
				if m.HWnd == 0 && m.Message == 0x0312 && ov.hkApp != nil { // WM_HOTKEY
					fg, _, _ := procGetForegroundWindow.Call()
					var pid uint32
					procGetWindowThreadPID.Call(fg, uintptr(unsafe.Pointer(&pid)))
					go ov.hkApp.onHotkey(int(m.WParam), int(pid))
					continue
				}
				procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
				procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
			}
		}()
		ov.ok = <-ready
	})
	return ov.ok
}

func ovDrain() {
	for {
		select {
		case f := <-ov.cmds:
			f()
		default:
			return
		}
	}
}

func ovRun(f func()) {
	if !ovStart() {
		return
	}
	ov.cmds <- f
	procPostThreadMessageW.Call(ov.tid, ovWake, 0, 0)
}

// findWindowOfPid returns the visible top-level window of a process.
func findWindowOfPid(pid uint32) uintptr {
	ov.findMu.Lock()
	defer ov.findMu.Unlock()
	if ov.findCb == 0 {
		ov.findCb = syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
			var p uint32
			procGetWindowThreadPID.Call(hwnd, uintptr(unsafe.Pointer(&p)))
			if p == ov.findPid {
				if vis, _, _ := procIsWindowVisible.Call(hwnd); vis != 0 {
					ov.findHwnd = hwnd
					return 0
				}
			}
			return 1
		})
	}
	ov.findPid, ov.findHwnd = pid, 0
	procEnumWindows.Call(ov.findCb, 0)
	return ov.findHwnd
}

// overlayAttach waits for scrcpy's window and glues a toolbar to it.
func overlayAttach(a *App, id, serial string, pid int) {
	go func() {
		var target uintptr
		for i := 0; i < 150 && target == 0; i++ {
			time.Sleep(200 * time.Millisecond)
			a.mu.Lock()
			s := a.sessions[id]
			alive := s != nil && s.cmd.Process != nil && s.cmd.Process.Pid == pid
			a.mu.Unlock()
			if !alive {
				return
			}
			target = findWindowOfPid(uint32(pid))
		}
		if target == 0 {
			return
		}
		ovRun(func() { tbCreate(a, id, serial, uint32(pid), target) })
	}()
}

func overlayDetach(id string) {
	if !ov.ok {
		return
	}
	ovRun(func() {
		if tb := ov.byID[id]; tb != nil {
			tbDestroy(tb)
		}
	})
}

func overlayDetachAll() {
	if !ov.ok {
		return
	}
	ovRun(func() {
		for _, tb := range ov.byID {
			tbDestroy(tb)
		}
	})
}

func tbCreate(a *App, id, serial string, pid uint32, target uintptr) {
	if old := ov.byID[id]; old != nil {
		tbDestroy(old)
	}
	tb := &toolbar{app: a, id: id, serial: serial, pid: pid, target: target, hover: -1, pressed: -1}
	exStyle := uintptr(0x80 | 0x8 | 0x08000000 | 0x80000)                                           // TOOLWINDOW|TOPMOST|NOACTIVATE|LAYERED
	hwnd, _, _ := procCreateWindowExW.Call(exStyle, uintptr(unsafe.Pointer(ov.cls)), 0, 0x80000000, // WS_POPUP
		0, 0, 10, 10, 0, 0, ov.hInst, 0)
	if hwnd == 0 {
		return
	}
	tb.hwnd = hwnd
	ov.bars[hwnd] = tb
	ov.byID[id] = tb
	procSetLayeredWindowAttrs.Call(hwnd, 0, 242, 2) // LWA_ALPHA
	tbRescale(tb)
	tb.hook, _, _ = procSetWinEventHook.Call(0x800B, 0x800B, 0, ov.eventCb, uintptr(pid), 0, 0) // LOCATIONCHANGE
	procSetTimer.Call(hwnd, 1, 120, 0)
	tbSync(tb)
}

func tbDestroy(tb *toolbar) {
	procKillTimer.Call(tb.hwnd, 1)
	if tb.hook != 0 {
		procUnhookWinEvent.Call(tb.hook)
	}
	procDestroyWindow.Call(tb.hwnd)
	if tb.iconFont != 0 {
		procDeleteObject.Call(tb.iconFont)
		procDeleteObject.Call(tb.labelFont)
	}
	if tb.smallIcon != 0 {
		procDeleteObject.Call(tb.smallIcon)
		procDeleteObject.Call(tb.tinyFont)
	}
	delete(ov.bars, tb.hwnd)
	if ov.byID[tb.id] == tb {
		delete(ov.byID, tb.id)
	}
}

func (tb *toolbar) px(v float64) int32 { return int32(v*tb.scale + 0.5) }

func tbRescale(tb *toolbar) {
	s := 1.0
	if procGetDpiForWindow.Find() == nil {
		if d, _, _ := procGetDpiForWindow.Call(tb.target); d > 0 {
			s = float64(d) / 96
		}
	}
	if s == tb.scale && tb.iconFont != 0 {
		return
	}
	tb.scale = s
	if tb.iconFont != 0 {
		procDeleteObject.Call(tb.iconFont)
		procDeleteObject.Call(tb.labelFont)
	}
	mk := func(size float64, weight int, face string) uintptr {
		f, _ := syscall.UTF16PtrFromString(face)
		h, _, _ := procCreateFontW.Call(uintptr(int64(-tb.px(size))), 0, 0, 0, uintptr(weight), 0, 0, 0,
			1, 0, 0, 5, 0, uintptr(unsafe.Pointer(f)))
		return h
	}
	tb.iconFont = mk(18, 400, "Segoe MDL2 Assets")
	tb.labelFont = mk(11.5, 600, "Bahnschrift")
	tb.w = tb.px(142)
	mainH := tb.px(52)*mainBtns + tb.px(4)*(mainBtns-1)
	remH := tb.px(35)*int32(len(remSpecs)) + tb.px(3)*int32(len(remSpecs)-1)
	if remH > mainH {
		mainH = remH
	}
	tb.h = tb.px(8)*2 + tb.px(26) + mainH
	if tb.smallIcon != 0 {
		procDeleteObject.Call(tb.smallIcon)
		procDeleteObject.Call(tb.tinyFont)
	}
	tb.smallIcon = mk(15, 400, "Segoe MDL2 Assets")
	tb.tinyFont = mk(10, 600, "Bahnschrift")
	rgn, _, _ := procCreateRoundRectRgn.Call(0, 0, uintptr(tb.w+1), uintptr(tb.h+1), 0, 0)
	procSetWindowRgn.Call(tb.hwnd, rgn, 1)
	tb.lastPos = rect{}
}

func tbWinEvent(hook, event, hwnd, idObject, idChild, thread, t uintptr) uintptr {
	if int32(idObject) != 0 { // OBJID_WINDOW
		return 0
	}
	for _, tb := range ov.bars {
		if tb.target == hwnd {
			tbSync(tb)
		}
	}
	return 0
}

func targetRect(h uintptr) rect {
	var r rect
	if procDwmGetWindowAttribute.Find() == nil {
		if res, _, _ := procDwmGetWindowAttribute.Call(h, 9, uintptr(unsafe.Pointer(&r)), unsafe.Sizeof(r)); res == 0 {
			return r // DWMWA_EXTENDED_FRAME_BOUNDS: the visible frame
		}
	}
	procGetWindowRect.Call(h, uintptr(unsafe.Pointer(&r)))
	return r
}

func tbSync(tb *toolbar) {
	if ok, _, _ := procIsWindow.Call(tb.target); ok == 0 {
		tbShow(tb, false, rect{})
		return
	}
	tbRescale(tb)
	visible := true
	if ic, _, _ := procIsIconic.Call(tb.target); ic != 0 {
		visible = false
	}
	if v, _, _ := procIsWindowVisible.Call(tb.target); v == 0 {
		visible = false
	}
	fg, _, _ := procGetForegroundWindow.Call()
	if fg != tb.target {
		var p uint32
		procGetWindowThreadPID.Call(fg, uintptr(unsafe.Pointer(&p)))
		if p != ov.ourPid { // our own menus/dialogs keep it visible
			visible = false
		}
	}
	r := targetRect(tb.target)
	mon, _, _ := procMonitorFromWindow.Call(tb.target, 2)
	mi := monitorInfo{CbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
	procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi)))
	work := mi.RcWork
	zoomed, _, _ := procIsZoomed.Call(tb.target)
	full := zoomed != 0 || (r.L <= mi.RcMonitor.L && r.R >= mi.RcMonitor.R) || (r.R-r.L) >= (work.R-work.L)-tb.px(8)
	gap := tb.px(10)
	var x int32
	if !full && r.R+gap+tb.w <= work.R {
		x = r.R + gap // beside the window
	} else {
		x = r.R - tb.w - tb.px(14) // inside, on the right edge (black area when maximized)
	}
	y := r.T + (r.B-r.T-tb.h)/2
	if y+tb.h > work.B {
		y = work.B - tb.h
	}
	if y < work.T {
		y = work.T
	}
	tbShow(tb, visible, rect{x, y, x + tb.w, y + tb.h})
}

func tbShow(tb *toolbar, visible bool, pos rect) {
	if !visible {
		if tb.shown {
			procShowWindow.Call(tb.hwnd, 0)
			tb.shown = false
		}
		return
	}
	if pos != tb.lastPos || !tb.shown {
		procSetWindowPos.Call(tb.hwnd, ^uintptr(0), uintptr(pos.L), uintptr(pos.T), uintptr(tb.w), uintptr(tb.h),
			0x10|0x40) // HWND_TOPMOST, NOACTIVATE|SHOWWINDOW
		tb.lastPos = pos
		tb.shown = true
	}
}

func (tb *toolbar) btnRect(i int) rect {
	pad := tb.px(8)
	if i >= btnCount { // remote column
		j := int32(i - btnCount)
		top := pad + tb.px(26) + j*(tb.px(35)+tb.px(3))
		return rect{tb.px(76), top, tb.w - tb.px(4), top + tb.px(35)}
	}
	bh, g := tb.px(52), tb.px(4)
	top := pad + tb.px(26) + int32(i)*(bh+g) // under the fps line
	return rect{tb.px(4), top, tb.px(72), top + bh}
}

func (tb *toolbar) btnIndices() []int {
	var r []int
	for i := 0; i < mainBtns; i++ {
		r = append(r, i)
	}
	for j := range remSpecs {
		r = append(r, btnCount+j)
	}
	return r
}

func (tb *toolbar) hit(x, y int32) int {
	for _, i := range tb.btnIndices() {
		r := tb.btnRect(i)
		if x >= r.L && x < r.R && y >= r.T && y < r.B {
			return i
		}
	}
	return -1
}

func rgb(r, g, b uint32) uint32 { return r | g<<8 | b<<16 }

var (
	// yellow / black, matching the main window
	colBg     = rgb(0x0d, 0x0d, 0x0d)
	colHover  = rgb(0xff, 0xe1, 0x00)
	colText   = rgb(0xff, 0xff, 0xff)
	colMuted  = rgb(0xbd, 0xbd, 0xbd)
	colRed    = rgb(0xff, 0x3b, 0x3b)
	colTeal   = rgb(0xff, 0xe1, 0x00)
	colOkText = rgb(0xff, 0xe1, 0x00)
	colBlack  = rgb(0, 0, 0)
)

func fmtClock(sec int64) string {
	if sec < 0 {
		sec = 0
	}
	m, s := sec/60, sec%60
	if m >= 60 {
		return fmt.Sprintf("%d:%02d:%02d", m/60, m%60, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func drawText(dc uintptr, s string, r rect, font uintptr, color uint32) {
	procSelectObject.Call(dc, font)
	procSetTextColor.Call(dc, uintptr(color))
	u, _ := syscall.UTF16FromString(s)
	procDrawTextW.Call(dc, uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(&r)),
		0x1|0x4|0x20|0x800) // CENTER|VCENTER|SINGLELINE|NOPREFIX
}

func tbPaint(tb *toolbar) {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(tb.hwnd, uintptr(unsafe.Pointer(&ps)))
	defer procEndPaint.Call(tb.hwnd, uintptr(unsafe.Pointer(&ps)))
	mem, _, _ := procCreateCompatibleDC.Call(hdc)
	bmp, _, _ := procCreateCompatibleBmp.Call(hdc, uintptr(tb.w), uintptr(tb.h))
	oldBmp, _, _ := procSelectObject.Call(mem, bmp)
	defer func() {
		procSelectObject.Call(mem, oldBmp)
		procDeleteObject.Call(bmp)
		procDeleteDC.Call(mem)
	}()
	bg, _, _ := procCreateSolidBrush.Call(uintptr(colBg))
	full := rect{0, 0, tb.w, tb.h}
	procFillRect.Call(mem, uintptr(unsafe.Pointer(&full)), bg)
	procDeleteObject.Call(bg)
	// yellow frame
	yb, _, _ := procCreateSolidBrush.Call(uintptr(colHover))
	bw := tb.px(2)
	for _, e := range []rect{{0, 0, tb.w, bw}, {0, tb.h - bw, tb.w, tb.h}, {0, 0, bw, tb.h}, {tb.w - bw, 0, tb.w, tb.h}} {
		e := e
		procFillRect.Call(mem, uintptr(unsafe.Pointer(&e)), yb)
	}
	procDeleteObject.Call(yb)
	procSetBkMode.Call(mem, 1)
	// frames per second actually reaching the PC
	{
		f := tb.app.fpsOf(tb.id)
		txt, col := "… к/с", colMuted
		if f >= 0 {
			txt = strconv.Itoa(f) + " к/с"
			switch {
			case f >= 45:
				col = colHover
			case f >= 25:
				col = colText
			default:
				col = colRed
			}
		}
		drawText(mem, txt, rect{0, tb.px(6), tb.w, tb.px(6) + tb.px(24)}, tb.labelFont, col)
		sep, _, _ := procCreateSolidBrush.Call(uintptr(rgb(0x33, 0x33, 0x33)))
		line := rect{tb.px(10), tb.px(31), tb.w - tb.px(10), tb.px(32)}
		procFillRect.Call(mem, uintptr(unsafe.Pointer(&line)), sep)
		procDeleteObject.Call(sep)
	}

	rec, since, cam := tb.app.toolbarInfo(tb.id)
	now := time.Now()
	type spec struct {
		icon, label string
		color       uint32
	}
	specs := [btnCount + 13]spec{
		{"", "Скриншот", colText},
		{"", "Запись", colText},
		{"", "Файл", colText},
		{"", "Камера", colText},
		{"", "Приложения", colText},
		{"", "Погасить", colText},
		{"", "Звук: ПК", colText},
		{"", "Пульт", colText},
	}
	off, audio := tb.app.screenState(tb.id)
	if off {
		specs[btnScreen] = spec{"", "Включить", colTeal}
	}
	switch audio {
	case "phone":
		specs[btnSound] = spec{"", "Звук: тел.", colText}
	case "both":
		specs[btnSound] = spec{"", "Звук: оба", colText}
	}
	if rec {
		specs[btnRec] = spec{"", fmtClock(now.Unix() - since), colRed}
	}
	if cam != "" {
		specs[btnCam].color = colTeal
		specs[btnCam].label = map[string]string{"front": "Передняя", "back": "Задняя"}[cam]
	}
	rs := tb.app.remoteState(tb.id)
	for j, rsp := range remSpecs {
		sp := spec{rsp.icon, rsp.label, colText}
		if (rsp.act == "dnd" && rs.DND) || (rsp.act == "torch" && rs.Torch) {
			sp.color = colTeal
		}
		specs[btnCount+j] = sp
	}
	// divider between the two columns
	{
		dv, _, _ := procCreateSolidBrush.Call(uintptr(rgb(0x33, 0x33, 0x33)))
		line := rect{tb.px(73) + 1, tb.px(36), tb.px(73) + 2, tb.h - tb.px(8)}
		procFillRect.Call(mem, uintptr(unsafe.Pointer(&line)), dv)
		procDeleteObject.Call(dv)
	}
	nullPen, _, _ := procGetStockObject.Call(8)
	for _, i := range tb.btnIndices() {
		r := tb.btnRect(i)
		if i == tb.hover || i == tb.pressed {
			c := colHover
			if i == tb.pressed {
				c = rgb(0xff, 0xff, 0xff)
			}
			br, _, _ := procCreateSolidBrush.Call(uintptr(c))
			ob, _, _ := procSelectObject.Call(mem, br)
			op, _, _ := procSelectObject.Call(mem, nullPen)
			procRoundRect.Call(mem, uintptr(r.L), uintptr(r.T), uintptr(r.R), uintptr(r.B), 0, 0)
			procSelectObject.Call(mem, ob)
			procSelectObject.Call(mem, op)
			procDeleteObject.Call(br)
		}
		sp := specs[i]
		labelColor := colMuted
		if sp.color != colText {
			labelColor = sp.color
		}
		if f := tb.flash[i]; now.Before(f.until) {
			sp.label, labelColor = f.text, f.color
		} else if tb.busy[i] {
			sp.label = "…"
		}
		if i == tb.hover || i == tb.pressed {
			sp.color, labelColor = colBlack, colBlack
		}
		if i >= btnCount {
			drawText(mem, sp.icon, rect{r.L, r.T + tb.px(2), r.R, r.T + tb.px(20)}, tb.smallIcon, sp.color)
			drawText(mem, sp.label, rect{r.L, r.T + tb.px(19), r.R, r.B - tb.px(1)}, tb.tinyFont, labelColor)
			continue
		}
		drawText(mem, sp.icon, rect{r.L, r.T + tb.px(5), r.R, r.T + tb.px(30)}, tb.iconFont, sp.color)
		drawText(mem, sp.label, rect{r.L, r.T + tb.px(30), r.R, r.B - tb.px(4)}, tb.labelFont, labelColor)
	}
	procBitBlt.Call(hdc, 0, 0, uintptr(tb.w), uintptr(tb.h), mem, 0, 0, 0x00CC0020) // SRCCOPY
}

func tbInvalidate(tb *toolbar) { procInvalidateRect.Call(tb.hwnd, 0, 0) }

// tbDone is called from worker goroutines when an action finishes.
func tbDone(id string, btn int, text string, ok bool) {
	ovRun(func() {
		tb := ov.byID[id]
		if tb == nil {
			return
		}
		tb.busy[btn] = false
		c := colOkText
		if !ok {
			c = colRed
		}
		if text != "" {
			tb.flash[btn] = tbFlash{text, c, time.Now().Add(1800 * time.Millisecond)}
		}
		tbInvalidate(tb)
	})
}

func tbWndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	tb := ov.bars[hwnd]
	switch uint32(msg) {
	case 0x0021: // WM_MOUSEACTIVATE
		return 3 // MA_NOACTIVATE
	case 0x000F: // WM_PAINT
		if tb != nil {
			tbPaint(tb)
			return 0
		}
	case 0x0014: // WM_ERASEBKGND
		return 1
	case 0x0113: // WM_TIMER
		if tb != nil {
			tbSync(tb)
			rec, _, _ := tb.app.toolbarInfo(tb.id)
			if f := tb.app.fpsOf(tb.id); f != tb.lastFps {
				tb.lastFps = f
				tbInvalidate(tb)
			}
			if rec || tb.flashActive() {
				tbInvalidate(tb)
			}
		}
		return 0
	case 0x0200: // WM_MOUSEMOVE
		if tb != nil {
			if !tb.tracking {
				tme := trackMouseEvent{DwFlags: 0x2, HwndTrack: hwnd} // TME_LEAVE
				tme.CbSize = uint32(unsafe.Sizeof(tme))
				procTrackMouseEvent.Call(uintptr(unsafe.Pointer(&tme)))
				tb.tracking = true
			}
			h := tb.hit(int32(int16(lparam&0xFFFF)), int32(int16((lparam>>16)&0xFFFF)))
			if h != tb.hover {
				tb.hover = h
				tbInvalidate(tb)
			}
		}
		return 0
	case 0x02A3: // WM_MOUSELEAVE
		if tb != nil {
			tb.tracking = false
			tb.hover, tb.pressed = -1, -1
			tbInvalidate(tb)
		}
		return 0
	case 0x0201: // WM_LBUTTONDOWN
		if tb != nil {
			tb.pressed = tb.hit(int32(int16(lparam&0xFFFF)), int32(int16((lparam>>16)&0xFFFF)))
			tbInvalidate(tb)
		}
		return 0
	case 0x0202: // WM_LBUTTONUP
		if tb != nil {
			b := tb.hit(int32(int16(lparam&0xFFFF)), int32(int16((lparam>>16)&0xFFFF)))
			p := tb.pressed
			tb.pressed = -1
			tbInvalidate(tb)
			if b >= 0 && b == p {
				tbClick(tb, b)
			}
		}
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

func (tb *toolbar) flashActive() bool {
	now := time.Now()
	for _, f := range tb.flash {
		if !f.until.IsZero() && now.Before(f.until.Add(200*time.Millisecond)) {
			return true
		}
	}
	return false
}

func tbClick(tb *toolbar, b int) {
	if tb.busy[b] {
		return
	}
	a, id, serial, target := tb.app, tb.id, tb.serial, tb.target
	go a.sfx("tick")
	if b >= btnCount { // remote: one tap
		act := remSpecs[b-btnCount].act
		go func() {
			st, err := a.remote(serial, act)
			switch {
			case err != nil:
				tbDone(id, b, "Ошибка", false)
			case act == "dnd":
				tbDone(id, b, map[bool]string{true: "Вкл", false: "Выкл"}[st.DND], true)
			case act == "torch":
				tbDone(id, b, map[bool]string{true: "Вкл", false: "Выкл"}[st.Torch], true)
			default:
				tbDone(id, b, "", true)
			}
		}()
		tbInvalidate(tb)
		return
	}
	switch b {
	case btnShot:
		tb.busy[b] = true
		go func() {
			_, err := a.screenshot(serial)
			if err != nil {
				a.event("error", "", err.Error())
				tbDone(id, b, "Ошибка", false)
				return
			}
			tbDone(id, b, "Готово", true)
		}()
	case btnRec:
		rec, _, _ := a.toolbarInfo(id)
		tb.busy[b] = true
		go func() {
			// The scrcpy window restarts; a fresh toolbar attaches to the new one.
			if err := a.setRecording(serial, !rec); err != nil {
				a.event("error", "", err.Error())
				tbDone(id, b, "Ошибка", false)
			}
		}()
	case btnFile:
		files := openFilesDialog(tb.hwnd)
		procSetForegroundWindow.Call(target)
		if len(files) == 0 {
			return
		}
		tb.busy[b] = true
		go func() {
			ok, last := 0, ""
			for _, f := range files {
				msg, err := a.pushLocalPath(serial, f)
				if err != nil {
					a.event("error", "", filepathBase(f)+": "+err.Error())
					continue
				}
				ok++
				last = msg
			}
			if ok == len(files) {
				if ok > 1 {
					last = fmt.Sprintf("Отправлено файлов: %d", ok)
				}
				a.event("info", "", last)
				tbDone(id, b, "Готово", true)
			} else {
				tbDone(id, b, "Ошибка", false)
			}
		}()
	case btnCam:
		_, _, cam := a.toolbarInfo(id)
		items := []TrayItem{
			{Text: "Задняя камера", Checked: cam == "back", Action: func() { a.cameraOn(serial, "back") }},
			{Text: "Передняя камера", Checked: cam == "front", Action: func() { a.cameraOn(serial, "front") }},
			{Separator: true},
			{Text: "Закрыть камеру", Disabled: cam == "", Action: func() { a.stopWait(id + camSuffix) }},
		}
		r := tb.btnRect(btnCam)
		var wr rect
		procGetWindowRect.Call(tb.hwnd, uintptr(unsafe.Pointer(&wr)))
		popupMenuAt(tb.hwnd, items, wr.L, wr.T+r.T)
		procSetForegroundWindow.Call(target)
	case btnScreen:
		off, _ := a.screenState(id)
		// scrcpy shortcuts: Alt+O = phone screen off (mirroring continues), Alt+Shift+O = on.
		procSetForegroundWindow.Call(target)
		if off {
			sendKeys(0xA4, 0xA0, 'O') // LAlt+LShift+O
		} else {
			sendKeys(0xA4, 'O')
		}
		a.setScreenOffFlag(id, !off)
		if !off {
			tb.flash[b] = tbFlash{"Погашен", colOkText, time.Now().Add(1200 * time.Millisecond)}
		}
	case btnSound:
		_, audio := a.screenState(id)
		set := func(m string) func() {
			return func() {
				if err := a.setAudioMode(serial, m); err != nil {
					a.event("error", "", err.Error())
					tbDone(id, btnSound, "Ошибка", false)
				}
			}
		}
		items := []TrayItem{
			{Text: "Звук на ПК", Checked: audio == "pc", Action: set("pc")},
			{Text: "Звук на телефоне", Checked: audio == "phone", Action: set("phone")},
			{Text: "На ПК и на телефоне (экспериментально)", Checked: audio == "both", Action: set("both")},
		}
		r := tb.btnRect(btnSound)
		var wr rect
		procGetWindowRect.Call(tb.hwnd, uintptr(unsafe.Pointer(&wr)))
		popupMenuAt(tb.hwnd, items, wr.L, wr.T+r.T)
		procSetForegroundWindow.Call(target)
	case btnRemote:
		st := a.remoteState(id)
		do := func(act string) func() {
			return func() {
				if _, err := a.remote(serial, act); err != nil {
					a.event("error", "", err.Error())
				}
			}
		}
		items := []TrayItem{
			{Text: "Громкость +", Action: do("vol_up")},
			{Text: "Громкость −", Action: do("vol_down")},
			{Text: "Без звука", Action: do("mute")},
			{Separator: true},
			{Text: "Ярче", Action: do("bright_up")},
			{Text: "Темнее", Action: do("bright_down")},
			{Separator: true},
			{Text: "Не беспокоить", Checked: st.DND, Action: do("dnd")},
			{Text: "Фонарик", Checked: st.Torch, Action: do("torch")},
			{Separator: true},
			{Text: "Повернуть экран", Action: do("rotate")},
			{Text: "Автоповорот", Action: do("autorotate")},
			{Text: "Заблокировать телефон", Action: do("lock")},
		}
		r := tb.btnRect(btnRemote)
		var wr rect
		procGetWindowRect.Call(tb.hwnd, uintptr(unsafe.Pointer(&wr)))
		popupMenuAt(tb.hwnd, items, wr.L, wr.T+r.T)
		procSetForegroundWindow.Call(target)
	case btnApps:
		a.mu.Lock()
		running := map[string]bool{}
		for _, run := range a.appRunsLocked(id) {
			running[run.Pkg] = true
		}
		a.mu.Unlock()
		var items []TrayItem
		for _, e := range a.recentApps(id) {
			e := e
			items = append(items, TrayItem{Text: e.Label, Checked: running[e.Pkg], Disabled: running[e.Pkg],
				Action: func() { a.openApp(serial, e.Pkg, e.Label) }})
		}
		if len(items) > 0 {
			items = append(items, TrayItem{Separator: true})
		}
		items = append(items, TrayItem{Text: "Все приложения…", Action: func() {
			a.requestUI("apps", serial)
			if a.showUI != nil {
				a.showUI()
			}
		}})
		r := tb.btnRect(btnApps)
		var wr rect
		procGetWindowRect.Call(tb.hwnd, uintptr(unsafe.Pointer(&wr)))
		popupMenuAt(tb.hwnd, items, wr.L, wr.T+r.T)
		procSetForegroundWindow.Call(target)
	}
	tbInvalidate(tb)
}

func filepathBase(p string) string {
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// popupMenuAt shows a menu to the left of the toolbar.
func popupMenuAt(owner uintptr, items []TrayItem, x, y int32) {
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
	procSetForegroundWindow.Call(owner)
	id, _, _ := procTrackPopupMenu.Call(h, tpmReturn|tpmNoNotify|0x0008, // TPM_RIGHTALIGN
		uintptr(x-4), uintptr(y), 0, owner, 0)
	procPostMessageW.Call(owner, wmNull, 0, 0)
	if id > 0 && int(id) <= len(items) && items[id-1].Action != nil {
		go items[id-1].Action()
	}
}

func openFilesDialog(owner uintptr) []string {
	buf := make([]uint16, 32768)
	filter, _ := syscall.UTF16FromString("Все файлы\x00*.*\x00Фото и видео\x00*.jpg;*.jpeg;*.png;*.heic;*.mp4;*.mov;*.webm\x00Приложения (APK)\x00*.apk\x00")
	filter = append(filter, 0)
	title, _ := syscall.UTF16PtrFromString("Отправить на телефон")
	ofn := openFileName{
		HwndOwner:   owner,
		LpstrFilter: &filter[0],
		LpstrFile:   &buf[0],
		NMaxFile:    uint32(len(buf)),
		LpstrTitle:  title,
		Flags:       0x200 | 0x80000 | 0x1000 | 0x800 | 0x8, // MULTISELECT|EXPLORER|FILEMUSTEXIST|PATHMUSTEXIST|NOCHANGEDIR
	}
	ofn.LStructSize = uint32(unsafe.Sizeof(ofn))
	procSetForegroundWindow.Call(owner)
	if r, _, _ := procGetOpenFileNameW.Call(uintptr(unsafe.Pointer(&ofn))); r == 0 {
		return nil
	}
	// Result: "dir\0file1\0file2\0\0" or "fullpath\0\0".
	var parts []string
	start := 0
	for i := 0; i < len(buf); i++ {
		if buf[i] == 0 {
			if i == start {
				break
			}
			parts = append(parts, syscall.UTF16ToString(buf[start:i]))
			start = i + 1
		}
	}
	if len(parts) <= 1 {
		return parts
	}
	dir := strings.TrimRight(parts[0], `\`)
	var files []string
	for _, f := range parts[1:] {
		files = append(files, dir+`\`+f)
	}
	return files
}

func focusPid(pid int) {
	h := findWindowOfPid(uint32(pid))
	if h == 0 {
		return
	}
	if ic, _, _ := procIsIconic.Call(h); ic != 0 {
		procShowWindow.Call(h, 9)
	}
	procSetForegroundWindow.Call(h)
}

var (
	procRegisterHotKey   = user32.NewProc("RegisterHotKey")
	procUnregisterHotKey = user32.NewProc("UnregisterHotKey")
	procMessageBeep      = user32.NewProc("MessageBeep")
)

func beep(ok bool) {
	if ok {
		procMessageBeep.Call(0) // MB_OK
	} else {
		procMessageBeep.Call(0x10) // MB_ICONHAND
	}
}

// applyHotkeys (re)registers global hotkeys on the overlay thread and reports conflicts.
func (a *App) applyHotkeys() {
	a.mu.Lock()
	specs := []string{a.cfg.Settings.HotkeyShot, a.cfg.Settings.HotkeyRec}
	a.mu.Unlock()
	names := []string{"скриншота", "записи"}
	ovRun(func() {
		ov.hkApp = a
		var errs []string
		for i, sp := range specs {
			id := uintptr(i + 1)
			procUnregisterHotKey.Call(0, id)
			h, err := parseHotkey(sp)
			if err != nil {
				errs = append(errs, sp+": "+err.Error())
				continue
			}
			if h.VK == 0 {
				continue
			}
			if r, _, _ := procRegisterHotKey.Call(0, id, uintptr(h.Mods|0x4000), uintptr(h.VK)); r == 0 { // MOD_NOREPEAT
				errs = append(errs, sp+" для "+names[i]+" уже занята другой программой")
			}
		}
		a.mu.Lock()
		a.hkErr = strings.Join(errs, "; ")
		a.mu.Unlock()
	})
}

type kbInput struct {
	Type  uint32
	_     uint32
	Vk    uint16
	Scan  uint16
	Flags uint32
	Time  uint32
	_     uint32
	Extra uintptr
	_     [8]byte
}

var (
	procSendInput     = user32.NewProc("SendInput")
	procMapVirtualKey = user32.NewProc("MapVirtualKeyW")
)

// sendKeys presses the given virtual keys in order and releases them in reverse.
func sendKeys(vks ...uint16) {
	var in []kbInput
	mk := func(vk uint16, up bool) kbInput {
		sc, _, _ := procMapVirtualKey.Call(uintptr(vk), 0)
		k := kbInput{Type: 1, Vk: vk, Scan: uint16(sc)}
		if up {
			k.Flags = 2 // KEYEVENTF_KEYUP
		}
		return k
	}
	for _, vk := range vks {
		in = append(in, mk(vk, false))
	}
	for i := len(vks) - 1; i >= 0; i-- {
		in = append(in, mk(vks[i], true))
	}
	time.Sleep(30 * time.Millisecond) // let scrcpy take focus first
	procSendInput.Call(uintptr(len(in)), uintptr(unsafe.Pointer(&in[0])), unsafe.Sizeof(in[0]))
}

var procKeybdEvent = user32.NewProc("keybd_event")

// forceForeground: Windows only lets the active app raise windows; a synthetic Alt
// tap is the well-known way to be allowed to.
func forceForeground(h uintptr) {
	if ic, _, _ := procIsIconic.Call(h); ic != 0 {
		procShowWindow.Call(h, 9)
	}
	if r, _, _ := procSetForegroundWindow.Call(h); r != 0 {
		return
	}
	procKeybdEvent.Call(0x12, 0, 0, 0) // VK_MENU down
	procKeybdEvent.Call(0x12, 0, 2, 0) // up
	procSetForegroundWindow.Call(h)
}

var (
	procGetClientRect  = user32.NewProc("GetClientRect")
	procClientToScreen = user32.NewProc("ClientToScreen")
)

// revealWindow waits for scrcpy's window. If it shows up before revealAt (the UI
// animation isn't done yet) it's hidden until then. Then it's moved so its picture
// sits exactly on the animation's target rect, shown and brought to the front.
func revealWindow(pid int, pl *Place, revealAt time.Time, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		h := findWindowOfPid(uint32(pid))
		if h == 0 {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		hidden := false
		if wait := time.Until(revealAt); wait > 0 {
			procShowWindow.Call(h, 0) // SW_HIDE
			hidden = true
			time.Sleep(wait)
		}
		if pl != nil {
			done := make(chan struct{})
			ovRun(func() { placeOnAppMonitor(h, pl); close(done) })
			select {
			case <-done:
			case <-time.After(time.Second):
			}
		}
		if hidden {
			procShowWindow.Call(h, 5) // SW_SHOW
		}
		forceForeground(h)
		return true
	}
	return false
}

// placeOnAppMonitor runs on the per-monitor-DPI-aware overlay thread, so all
// coordinates are physical pixels.
func placeOnAppMonitor(h uintptr, pl *Place) {
	ref := findAppWindow(windowTitle)
	if ref == 0 {
		ref = h
	}
	mon, _, _ := procMonitorFromWindow.Call(ref, 2)
	mi := monitorInfo{CbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
	if r, _, _ := procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi))); r == 0 {
		return
	}
	m := mi.RcMonitor
	mw, mh := float64(m.R-m.L), float64(m.B-m.T)
	cx := m.L + int32(pl.X*mw+.5)
	cy := m.T + int32(pl.Y*mh+.5)
	cw := int32(pl.W*mw + .5)
	ch := int32(pl.H*mh + .5)
	// Grow the client rect by the window's frame (title bar, borders).
	var wr, cr rect
	procGetWindowRect.Call(h, uintptr(unsafe.Pointer(&wr)))
	procGetClientRect.Call(h, uintptr(unsafe.Pointer(&cr)))
	var org struct{ X, Y int32 }
	procClientToScreen.Call(h, uintptr(unsafe.Pointer(&org)))
	left, top := org.X-wr.L, org.Y-wr.T
	right, bottom := wr.R-(org.X+cr.R), wr.B-(org.Y+cr.B)
	procSetWindowPos.Call(h, 0, uintptr(cx-left), uintptr(cy-top), uintptr(cw+left+right), uintptr(ch+top+bottom),
		0x0004|0x0010) // NOZORDER|NOACTIVATE
}

// placementArgs: start scrcpy already at (about) the right spot, so nothing jumps.
func placementArgs(pl *Place) []string {
	if pl == nil {
		return nil
	}
	var out []string
	done := make(chan struct{})
	ovRun(func() {
		defer close(done)
		ref := findAppWindow(windowTitle)
		if ref == 0 {
			return
		}
		mon, _, _ := procMonitorFromWindow.Call(ref, 2)
		mi := monitorInfo{CbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
		if r, _, _ := procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi))); r == 0 {
			return
		}
		m := mi.RcMonitor
		mw, mh := float64(m.R-m.L), float64(m.B-m.T)
		out = []string{
			"--window-x=" + strconv.Itoa(int(m.L)+int(pl.X*mw+.5)),
			"--window-y=" + strconv.Itoa(int(m.T)+int(pl.Y*mh+.5)),
			"--window-width=" + strconv.Itoa(int(pl.W*mw+.5)),
			"--window-height=" + strconv.Itoa(int(pl.H*mh+.5)),
		}
	})
	select {
	case <-done:
	case <-time.After(time.Second):
	}
	return out
}

// ---------------------------------------------------------------- incoming call popup

type callPopup struct {
	hwnd     uintptr
	info     CallInfo
	devName  string
	act      func(string)
	hover    int
	tracking bool
	scale    float64
	w, h     int32
	fBig     uintptr
	fSmall   uintptr
	fBtn     uintptr
	blink    bool
	regOnce  sync.Once
	cls      *uint16
}

var cp callPopup

func (c *callPopup) px(v float64) int32 { return int32(v*c.scale + .5) }

func cpButtons() []struct {
	act, label string
	bg, fg     uint32
	r          rect
} {
	type b = struct {
		act, label string
		bg, fg     uint32
		r          rect
	}
	pad, bh := cp.px(14), cp.px(38)
	top := cp.h - pad - bh
	if cp.info.State == "ringing" {
		half := (cp.w - pad*3) / 2
		return []b{
			{"answer", "Ответить", colHover, colBlack, rect{pad, top, pad + half, top + bh}},
			{"decline", "Сбросить", colRed, colText, rect{pad*2 + half, top, cp.w - pad, top + bh}},
		}
	}
	return []b{{"hangup", "Завершить", colRed, colText, rect{pad, top, cp.w - pad, top + bh}}}
}

func cpWndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	switch uint32(msg) {
	case 0x0021: // WM_MOUSEACTIVATE
		return 3
	case 0x0014:
		return 1
	case 0x000F:
		cpPaint()
		return 0
	case 0x0113: // WM_TIMER
		cp.blink = !cp.blink
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0
	case 0x0200:
		if !cp.tracking {
			tme := trackMouseEvent{DwFlags: 0x2, HwndTrack: hwnd}
			tme.CbSize = uint32(unsafe.Sizeof(tme))
			procTrackMouseEvent.Call(uintptr(unsafe.Pointer(&tme)))
			cp.tracking = true
		}
		x, y := int32(int16(lparam&0xFFFF)), int32(int16((lparam>>16)&0xFFFF))
		h := -1
		for i, b := range cpButtons() {
			if x >= b.r.L && x < b.r.R && y >= b.r.T && y < b.r.B {
				h = i
			}
		}
		if h != cp.hover {
			cp.hover = h
			procInvalidateRect.Call(hwnd, 0, 0)
		}
		return 0
	case 0x02A3:
		cp.tracking, cp.hover = false, -1
		procInvalidateRect.Call(hwnd, 0, 0)
		return 0
	case 0x0202: // WM_LBUTTONUP
		x, y := int32(int16(lparam&0xFFFF)), int32(int16((lparam>>16)&0xFFFF))
		for _, b := range cpButtons() {
			if x >= b.r.L && x < b.r.R && y >= b.r.T && y < b.r.B && cp.act != nil {
				act, f := b.act, cp.act
				go f(act)
			}
		}
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

func cpPaint() {
	var ps paintStruct
	hdc, _, _ := procBeginPaint.Call(cp.hwnd, uintptr(unsafe.Pointer(&ps)))
	defer procEndPaint.Call(cp.hwnd, uintptr(unsafe.Pointer(&ps)))
	mem, _, _ := procCreateCompatibleDC.Call(hdc)
	bmp, _, _ := procCreateCompatibleBmp.Call(hdc, uintptr(cp.w), uintptr(cp.h))
	old, _, _ := procSelectObject.Call(mem, bmp)
	defer func() { procSelectObject.Call(mem, old); procDeleteObject.Call(bmp); procDeleteDC.Call(mem) }()
	fill := func(r rect, c uint32) {
		br, _, _ := procCreateSolidBrush.Call(uintptr(c))
		procFillRect.Call(mem, uintptr(unsafe.Pointer(&r)), br)
		procDeleteObject.Call(br)
	}
	fill(rect{0, 0, cp.w, cp.h}, colBg)
	frame := colHover
	if cp.info.State == "ringing" && cp.blink {
		frame = colText
	}
	bw := cp.px(3)
	for _, e := range []rect{{0, 0, cp.w, bw}, {0, cp.h - bw, cp.w, cp.h}, {0, 0, bw, cp.h}, {cp.w - bw, 0, cp.w, cp.h}} {
		fill(e, frame)
	}
	procSetBkMode.Call(mem, 1)
	pad := cp.px(14)
	head := "ВХОДЯЩИЙ ЗВОНОК"
	if cp.info.State == "offhook" {
		head = "РАЗГОВОР · " + fmtClock(time.Now().Unix()-cp.info.Since)
	}
	// yellow label plate
	fill(rect{pad, pad, pad + cp.px(190), pad + cp.px(22)}, colHover)
	drawTextL(mem, head, rect{pad + cp.px(8), pad, pad + cp.px(190), pad + cp.px(22)}, cp.fSmall, colBlack)
	drawTextL(mem, cp.info.Name, rect{pad, pad + cp.px(28), cp.w - pad, pad + cp.px(62)}, cp.fBig, colText)
	drawTextL(mem, cp.devName, rect{pad, pad + cp.px(60), cp.w - pad, pad + cp.px(78)}, cp.fSmall, colMuted)
	for i, b := range cpButtons() {
		bg := b.bg
		if i == cp.hover {
			bg = colText
		}
		fg := b.fg
		if i == cp.hover {
			fg = colBlack
		}
		fill(b.r, bg)
		drawText(mem, b.label, b.r, cp.fBtn, fg)
	}
	procBitBlt.Call(hdc, 0, 0, uintptr(cp.w), uintptr(cp.h), mem, 0, 0, 0x00CC0020)
}

func drawTextL(dc uintptr, s string, r rect, font uintptr, color uint32) {
	procSelectObject.Call(dc, font)
	procSetTextColor.Call(dc, uintptr(color))
	u, _ := syscall.UTF16FromString(s)
	procDrawTextW.Call(dc, uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(&r)),
		0x0|0x4|0x20|0x800|0x8000) // LEFT|VCENTER|SINGLELINE|NOPREFIX|END_ELLIPSIS
}

var procMonitorFromPoint = user32.NewProc("MonitorFromPoint")

// callPopupShow shows/updates the call popup at the bottom-right of the main screen.
func callPopupShow(info CallInfo, devName string, act func(string)) {
	ovRun(func() {
		cp.regOnce.Do(func() {
			cp.cls, _ = syscall.UTF16PtrFromString("PhoneOnPCCall")
			cur, _, _ := procLoadCursorW.Call(0, 32649)
			wc := wndClassEx{Style: 3, LpfnWndProc: syscall.NewCallback(cpWndProc), HInstance: ov.hInst, HCursor: cur, LpszClassName: cp.cls}
			wc.CbSize = uint32(unsafe.Sizeof(wc))
			procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
		})
		changed := cp.info.State != info.State || cp.info.Name != info.Name
		cp.info, cp.devName, cp.act = info, devName, act
		if cp.hwnd == 0 {
			cp.hover = -1
			h, _, _ := procCreateWindowExW.Call(0x80|0x8|0x08000000, uintptr(unsafe.Pointer(cp.cls)), 0, 0x80000000,
				0, 0, 10, 10, 0, 0, ov.hInst, 0)
			if h == 0 {
				return
			}
			cp.hwnd = h
			cp.scale = 1
			if procGetDpiForWindow.Find() == nil {
				if d, _, _ := procGetDpiForWindow.Call(h); d > 0 {
					cp.scale = float64(d) / 96
				}
			}
			mk := func(size float64, weight int, face string) uintptr {
				f, _ := syscall.UTF16PtrFromString(face)
				r, _, _ := procCreateFontW.Call(uintptr(int64(-cp.px(size))), 0, 0, 0, uintptr(weight), 0, 0, 0, 1, 0, 0, 5, 0, uintptr(unsafe.Pointer(f)))
				return r
			}
			cp.fBig, cp.fSmall, cp.fBtn = mk(26, 700, "Bahnschrift"), mk(12.5, 700, "Bahnschrift"), mk(16, 700, "Bahnschrift")
			cp.w, cp.h = cp.px(340), cp.px(150)
			mon, _, _ := procMonitorFromPoint.Call(0, 1) // primary
			mi := monitorInfo{CbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
			procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi)))
			x, y := mi.RcWork.R-cp.w-cp.px(18), mi.RcWork.B-cp.h-cp.px(18)
			procSetWindowPos.Call(h, ^uintptr(0), uintptr(x), uintptr(y), uintptr(cp.w), uintptr(cp.h), 0x10|0x40)
			procSetTimer.Call(h, 1, 500, 0)
			changed = true
		}
		if changed {
			procInvalidateRect.Call(cp.hwnd, 0, 0)
		}
	})
}

func callPopupHide() {
	if !ov.ok {
		return
	}
	ovRun(func() {
		if cp.hwnd != 0 {
			procKillTimer.Call(cp.hwnd, 1)
			procDestroyWindow.Call(cp.hwnd)
			for _, f := range []uintptr{cp.fBig, cp.fSmall, cp.fBtn} {
				procDeleteObject.Call(f)
			}
			cp.hwnd = 0
			cp.info = CallInfo{}
		}
	})
}

// hideWindowOfPid hides a helper window (the flashlight's camera session).
func hideWindowOfPid(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if h := findWindowOfPid(uint32(pid)); h != 0 {
			procShowWindow.Call(h, 0)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}
