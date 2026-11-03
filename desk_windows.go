package main

import (
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"
)

var (
	user32                   = syscall.NewLazyDLL("user32.dll")
	procEnumWindows          = user32.NewProc("EnumWindows")
	procGetWindowThreadPID   = user32.NewProc("GetWindowThreadProcessId")
	procPostMessageW         = user32.NewProc("PostMessageW")
	procIsWindowVisible      = user32.NewProc("IsWindowVisible")
	shell32                  = syscall.NewLazyDLL("shell32.dll")
	procSHGetKnownFolderPath = shell32.NewProc("SHGetKnownFolderPath")
	ole32                    = syscall.NewLazyDLL("ole32.dll")
	procCoTaskMemFree        = ole32.NewProc("CoTaskMemFree")

	enumMu     sync.Mutex
	enumPid    uint32
	enumFound  bool
	enumCbOnce sync.Once
	enumCb     uintptr
)

const wmClose = 0x0010

// gracefulStop sends WM_CLOSE to scrcpy's window: same as clicking ✕,
// so scrcpy finishes writing the video file properly.
func gracefulStop(cmd *exec.Cmd) {
	enumCbOnce.Do(func() {
		enumCb = syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
			var pid uint32
			procGetWindowThreadPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
			if vis, _, _ := procIsWindowVisible.Call(hwnd); pid == enumPid && vis != 0 {
				procPostMessageW.Call(hwnd, wmClose, 0, 0)
				enumFound = true
			}
			return 1
		})
	})
	enumMu.Lock()
	enumPid = uint32(cmd.Process.Pid)
	enumFound = false
	procEnumWindows.Call(enumCb, 0)
	found := enumFound
	enumMu.Unlock()
	if !found {
		cmd.Process.Kill()
	}
}

type guid struct {
	d1     uint32
	d2, d3 uint16
	d4     [8]byte
}

var (
	folderPictures = guid{0x33E28130, 0x4E1E, 0x4676, [8]byte{0x83, 0x5A, 0x98, 0x39, 0x5C, 0x3B, 0xC3, 0xBB}}
	folderVideos   = guid{0x18989B1D, 0x99B5, 0x455B, [8]byte{0x84, 0x1C, 0xAB, 0x7C, 0x74, 0xE4, 0xDD, 0xFC}}
)

func knownFolderByID(id *guid) string {
	var p *uint16
	r, _, _ := procSHGetKnownFolderPath.Call(uintptr(unsafe.Pointer(id)), 0, 0, uintptr(unsafe.Pointer(&p)))
	if r != 0 || p == nil {
		return ""
	}
	defer procCoTaskMemFree.Call(uintptr(unsafe.Pointer(p)))
	var buf []uint16
	for ptr := unsafe.Pointer(p); ; ptr = unsafe.Add(ptr, 2) {
		c := *(*uint16)(ptr)
		if c == 0 {
			break
		}
		buf = append(buf, c)
	}
	return syscall.UTF16ToString(buf)
}

// knownFolder returns the real Pictures/Videos folder (respects OneDrive redirection).
func knownFolder(kind string) string {
	id, fallback := &folderPictures, "Pictures"
	if kind == "videos" {
		id, fallback = &folderVideos, "Videos"
	}
	if d := knownFolderByID(id); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, fallback)
}

// openPath opens a file with its default app, or shows it selected in Explorer.
func openPath(p string, reveal bool) {
	if reveal {
		cmd := exec.Command("explorer.exe")
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `explorer.exe /select,"` + p + `"`}
		cmd.Start()
		return
	}
	exec.Command("rundll32", "url.dll,FileProtocolHandler", p).Start()
}

var (
	procSHFileOperationW = shell32.NewProc("SHFileOperationW")
	procOpenClipboard    = user32.NewProc("OpenClipboard")
	procEmptyClipboard   = user32.NewProc("EmptyClipboard")
	procSetClipboardData = user32.NewProc("SetClipboardData")
	procCloseClipboard   = user32.NewProc("CloseClipboard")
	procGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	procGlobalLock       = kernel32.NewProc("GlobalLock")
	procGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
	procRegisterCF       = user32.NewProc("RegisterClipboardFormatW")
	procMoveMemory       = kernel32.NewProc("RtlMoveMemory")
)

type shFileOp struct {
	Hwnd                  uintptr
	WFunc                 uint32
	PFrom                 *uint16
	PTo                   *uint16
	FFlags                uint16
	FAnyOperationsAborted int32
	HNameMappings         uintptr
	LpszProgressTitle     *uint16
}

// trashFile moves a file to the Recycle Bin (so a mistaken delete can be undone).
func trashFile(p string) error {
	from, _ := syscall.UTF16FromString(p)
	from = append(from, 0)                                                       // double-null terminated list
	op := shFileOp{WFunc: 3, PFrom: &from[0], FFlags: 0x40 | 0x10 | 0x400 | 0x4} // FO_DELETE; ALLOWUNDO|NOCONFIRMATION|NOERRORUI|SILENT
	if r, _, _ := procSHFileOperationW.Call(uintptr(unsafe.Pointer(&op))); r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

// copyFileToClipboard puts the file on the clipboard like Explorer's "Copy":
// then Ctrl+V in Telegram/Discord attaches it.
func copyFileToClipboard(p string) error {
	w, _ := syscall.UTF16FromString(p)
	w = append(w, 0)
	const dropfilesSize = 20 // DROPFILES: pFiles(4) pt(8) fNC(4) fWide(4)
	size := uintptr(dropfilesSize + len(w)*2)
	h, _, _ := procGlobalAlloc.Call(0x0042, size) // GHND
	if h == 0 {
		return syscall.EINVAL
	}
	buf := make([]byte, size)
	binary.LittleEndian.PutUint32(buf[0:], dropfilesSize)
	binary.LittleEndian.PutUint32(buf[16:], 1) // fWide
	for i, c := range w {
		binary.LittleEndian.PutUint16(buf[dropfilesSize+i*2:], c)
	}
	ptr, _, _ := procGlobalLock.Call(h)
	procMoveMemory.Call(ptr, uintptr(unsafe.Pointer(&buf[0])), size)
	procGlobalUnlock.Call(h)
	if r, _, _ := procOpenClipboard.Call(0); r == 0 {
		return syscall.EACCES
	}
	defer procCloseClipboard.Call()
	procEmptyClipboard.Call()
	procSetClipboardData.Call(15, h) // CF_HDROP
	// "Preferred DropEffect" = copy, so pasting copies rather than moves.
	name, _ := syscall.UTF16PtrFromString("Preferred DropEffect")
	if cf, _, _ := procRegisterCF.Call(uintptr(unsafe.Pointer(name))); cf != 0 {
		h2, _, _ := procGlobalAlloc.Call(0x0042, 4)
		if p2, _, _ := procGlobalLock.Call(h2); p2 != 0 {
			eff := []byte{1, 0, 0, 0} // DROPEFFECT_COPY
			procMoveMemory.Call(p2, uintptr(unsafe.Pointer(&eff[0])), 4)
			procGlobalUnlock.Call(h2)
			procSetClipboardData.Call(cf, h2)
		}
	}
	return nil
}
