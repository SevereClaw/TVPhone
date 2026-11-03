package main

// Global hotkeys (Windows): screenshot and record from any window, even a game.

import (
	"fmt"
	"strings"
)

type hotkeySpec struct {
	Mods uint32 // MOD_ALT=1 CONTROL=2 SHIFT=4 WIN=8
	VK   uint32
}

var namedKeys = map[string]uint32{
	"PrintScreen": 0x2C, "Pause": 0x13, "Insert": 0x2D, "Delete": 0x2E, "Home": 0x24, "End": 0x23,
	"PageUp": 0x21, "PageDown": 0x22, "Space": 0x20,
}

// parseHotkey: "Ctrl+Shift+S" → spec. Empty string = disabled.
func parseHotkey(s string) (hotkeySpec, error) {
	var h hotkeySpec
	s = strings.TrimSpace(s)
	if s == "" {
		return h, nil
	}
	parts := strings.Split(s, "+")
	key := strings.TrimSpace(parts[len(parts)-1])
	for _, m := range parts[:len(parts)-1] {
		switch strings.ToLower(strings.TrimSpace(m)) {
		case "ctrl", "control":
			h.Mods |= 2
		case "shift":
			h.Mods |= 4
		case "alt":
			h.Mods |= 1
		case "win":
			h.Mods |= 8
		default:
			return h, fmt.Errorf("непонятная клавиша %q", m)
		}
	}
	switch {
	case len(key) == 1 && ((key[0] >= 'A' && key[0] <= 'Z') || (key[0] >= '0' && key[0] <= '9')):
		h.VK = uint32(key[0])
	case len(key) == 1 && key[0] >= 'a' && key[0] <= 'z':
		h.VK = uint32(key[0] - 32)
	case len(key) >= 2 && (key[0] == 'F' || key[0] == 'f'):
		var n int
		if _, err := fmt.Sscanf(key[1:], "%d", &n); err == nil && n >= 1 && n <= 24 {
			h.VK = uint32(0x70 + n - 1)
		}
	default:
		h.VK = namedKeys[key]
	}
	if h.VK == 0 {
		return h, fmt.Errorf("клавиша %q не поддерживается", key)
	}
	isF := h.VK >= 0x70 && h.VK <= 0x87
	if h.Mods == 0 && !isF && h.VK != 0x2C && h.VK != 0x13 {
		return h, fmt.Errorf("добавьте Ctrl, Alt или Shift — иначе клавиша перестанет работать везде")
	}
	return h, nil
}

// hotkeyTarget picks the phone a hotkey applies to: the one whose window is in front,
// else the one with an open screen, else the first connected phone.
func (a *App) hotkeyTarget(fgPid int) (serial, id string, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for key, s := range a.sessions {
		if s.cmd.Process != nil && s.cmd.Process.Pid == fgPid {
			devID := strings.SplitN(strings.SplitN(key, appKeySep, 2)[0], camSuffix, 2)[0]
			if d := a.findDeviceLocked(devID); d != nil {
				return d.Serial, d.ID, true
			}
		}
	}
	for _, d := range a.devices {
		if d.State == "device" && a.sessions[d.ID] != nil {
			return d.Serial, d.ID, true
		}
	}
	for _, d := range a.devices {
		if d.State == "device" {
			return d.Serial, d.ID, true
		}
	}
	return "", "", false
}

func (a *App) onHotkey(which int, fgPid int) {
	serial, id, ok := a.hotkeyTarget(fgPid)
	if !ok {
		beep(false)
		a.event("error", "", "Горячая клавиша: телефон не подключён")
		return
	}
	switch which {
	case 1:
		if _, err := a.screenshot(serial); err != nil {
			beep(false)
			a.event("error", "", err.Error())
			tbDone(id, btnShot, "Ошибка", false)
			return
		}
		tbDone(id, btnShot, "Готово", true)
	case 2:
		rec, _, _ := a.toolbarInfo(id)
		if err := a.setRecording(serial, !rec); err != nil {
			beep(false)
			a.event("error", "", err.Error())
			return
		}
		if rec {
			a.logf("⌨ Запись остановлена горячей клавишей")
		}
	}
}
