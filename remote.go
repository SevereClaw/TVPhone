package main

// Phone remote: volume, brightness, Do Not Disturb, flashlight, lock, rotate.

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const torchSuffix = "|torch"

type RemoteState struct {
	DND   bool `json:"dnd"`
	Torch bool `json:"torch"`
}

func (a *App) remoteState(id string) RemoteState {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, torch := a.sessions[id+torchSuffix]
	return RemoteState{DND: a.dnd[id], Torch: torch}
}

func (a *App) readDND(serial, id string) bool {
	out, _ := a.adb(5*time.Second, "-s", serial, "shell", "settings get global zen_mode")
	on := strings.TrimSpace(out) != "0" && strings.TrimSpace(out) != "" && !strings.Contains(out, "null")
	a.mu.Lock()
	a.dnd[id] = on
	a.mu.Unlock()
	return on
}

func (a *App) remote(serial, action string) (RemoteState, error) {
	a.mu.Lock()
	d := a.findDeviceLocked(serial)
	if d == nil || d.State != "device" {
		a.mu.Unlock()
		return RemoteState{}, fmt.Errorf("телефон не найден")
	}
	ser, id := d.Serial, d.ID
	a.mu.Unlock()
	key := func(code int) error {
		_, err := a.adb(5*time.Second, "-s", ser, "shell", "input keyevent "+strconv.Itoa(code))
		return err
	}
	var err error
	switch action {
	case "back":
		err = key(4)
	case "home":
		err = key(3)
	case "recents":
		err = key(187) // KEYCODE_APP_SWITCH
	case "shade":
		// toggle the notification shade
		out, _ := a.adb(5*time.Second, "-s", ser, "shell", "dumpsys window | grep -m1 -E 'mCurrentFocus|NotificationShade'")
		if strings.Contains(out, "NotificationShade") {
			_, err = a.adb(5*time.Second, "-s", ser, "shell", "cmd statusbar collapse")
		} else {
			_, err = a.adb(5*time.Second, "-s", ser, "shell", "cmd statusbar expand-notifications")
		}
	case "vol_up":
		err = key(24)
	case "vol_down":
		err = key(25)
	case "mute":
		err = key(164) // KEYCODE_VOLUME_MUTE
	case "bright_up":
		err = key(221)
	case "bright_down":
		err = key(220)
	case "lock":
		err = key(223) // KEYCODE_SLEEP: screen off + lock
	case "wake":
		err = key(224)
	case "rotate":
		// turn auto-rotate off and flip between portrait and landscape
		out, _ := a.adb(5*time.Second, "-s", ser, "shell", "settings get system user_rotation")
		next := "1"
		if strings.TrimSpace(out) == "1" || strings.TrimSpace(out) == "3" {
			next = "0"
		}
		_, err = a.adb(5*time.Second, "-s", ser, "shell", "settings put system accelerometer_rotation 0; settings put system user_rotation "+next)
	case "autorotate":
		_, err = a.adb(5*time.Second, "-s", ser, "shell", "settings put system accelerometer_rotation 1")
	case "dnd":
		on := a.readDND(ser, id)
		mode := "on"
		if on {
			mode = "off"
		}
		out, e := a.adb(5*time.Second, "-s", ser, "shell", "cmd notification set_dnd "+mode)
		if e != nil || strings.Contains(strings.ToLower(out), "error") || strings.Contains(out, "Unknown") {
			// older Android: fall back to the zen_mode setting
			v := "1"
			if on {
				v = "0"
			}
			_, e = a.adb(5*time.Second, "-s", ser, "shell", "settings put global zen_mode "+v)
		}
		err = e
		a.readDND(ser, id)
	case "torch":
		err = a.toggleTorch(ser, id)
	case "kblayout":
		_, err = a.adb(5*time.Second, "-s", ser, "shell", "am start -a android.settings.HARD_KEYBOARD_SETTINGS")
	default:
		return RemoteState{}, fmt.Errorf("неизвестная команда")
	}
	return a.remoteState(id), err
}

// Flashlight: Android has no shell command for it, so a tiny hidden scrcpy camera
// session with --camera-torch keeps it on; stopping that session turns it off.
func (a *App) toggleTorch(serial, id string) error {
	a.mu.Lock()
	s := a.sessions[id+torchSuffix]
	a.mu.Unlock()
	if s != nil {
		a.stopWait(id + torchSuffix)
		return nil
	}
	args := []string{"-s", serial, "--video-source=camera", "--camera-facing=back", "--camera-torch",
		"--no-audio", "--max-size=96", "--video-bit-rate=100K", "--max-fps=5", "--window-title=Фонарик",
		"--window-x=-32000", "--window-y=-32000", "--window-width=64", "--window-height=64"}
	cmd := exec.Command(a.scrcpy, args...)
	cmd.Dir = a.toolDir
	cmd.Env = append(os.Environ(), "ADB="+a.adbPath)
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	sess := &session{cmd: cmd, since: time.Now().Unix(), done: make(chan struct{}), camera: "torch"}
	a.mu.Lock()
	a.sessions[id+torchSuffix] = sess
	a.mu.Unlock()
	a.logf("🔦 Фонарик включён")
	go hideWindowOfPid(cmd.Process.Pid, 10*time.Second)
	go func() {
		cmd.Wait()
		a.mu.Lock()
		if a.sessions[id+torchSuffix] == sess {
			delete(a.sessions, id+torchSuffix)
		}
		a.mu.Unlock()
		close(sess.done)
		a.logf("🔦 Фонарик выключен")
	}()
	return nil
}
