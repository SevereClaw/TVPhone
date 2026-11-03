package main

// "Open app in its own window": scrcpy --new-display --flex-display --start-app=<pkg>.
// The app runs on a separate virtual display, so the phone itself stays free.

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type AppEntry struct {
	Label  string `json:"label"`
	Pkg    string `json:"pkg"`
	System bool   `json:"system,omitempty"`
}

const appKeySep = "|app|"

var reAppLine = regexp.MustCompile(`^\s*([*-])\s+(.+?)\s+([A-Za-z0-9_]+(?:\.[A-Za-z0-9_]+)+)\s*$`)

func parseAppList(out string) []AppEntry {
	var res []AppEntry
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if i := strings.Index(line, "INFO:"); i >= 0 && strings.Contains(line, "List of apps") {
			continue
		}
		m := reAppLine.FindStringSubmatch(line)
		if m == nil || seen[m[3]] {
			continue
		}
		seen[m[3]] = true
		res = append(res, AppEntry{Label: strings.TrimSpace(m[2]), Pkg: m[3], System: m[1] == "*"})
	}
	sort.SliceStable(res, func(i, j int) bool {
		if res[i].System != res[j].System {
			return !res[i].System
		}
		return strings.ToLower(res[i].Label) < strings.ToLower(res[j].Label)
	})
	return res
}

func (a *App) listApps(serial string, refresh bool) ([]AppEntry, error) {
	a.mu.Lock()
	d := a.findDeviceLocked(serial)
	if d == nil || d.State != "device" {
		a.mu.Unlock()
		return nil, fmt.Errorf("телефон не найден")
	}
	dev := *d
	cached := a.appCache[dev.ID]
	a.mu.Unlock()
	if cached != nil && !refresh {
		return cached, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.scrcpy, "-s", dev.Serial, "--list-apps")
	cmd.Dir = a.toolDir
	cmd.Env = append(cmd.Environ(), "ADB="+a.adbPath)
	hideWindow(cmd)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	apps := parseAppList(string(out))
	if len(apps) == 0 {
		msg := lastLine(string(out))
		if msg == "" && err != nil {
			msg = err.Error()
		}
		return nil, fmt.Errorf("не удалось получить список приложений: %s", msg)
	}
	a.mu.Lock()
	a.appCache[dev.ID] = apps
	a.mu.Unlock()
	return apps, nil
}

func (a *App) recentApps(id string) []AppEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AppEntry{}, a.cfg.Recent[id]...)
}

func (a *App) rememberApp(id string, e AppEntry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cfg.Recent == nil {
		a.cfg.Recent = map[string][]AppEntry{}
	}
	list := []AppEntry{{Label: e.Label, Pkg: e.Pkg}}
	for _, x := range a.cfg.Recent[id] {
		if x.Pkg != e.Pkg && len(list) < 8 {
			list = append(list, x)
		}
	}
	a.cfg.Recent[id] = list
	a.saveLocked()
}

// appArgs: a resizable window with one app; IME shows inside the window, not on the phone.
func (s Settings) appArgs(pkg string, withAudio bool) []string {
	r := []string{"--new-display", "--flex-display", "--start-app=" + pkg, "--display-ime-policy=local"}
	r = append(r, s.keyboardArgs()...)
	if s.MaxSize > 0 {
		r = append(r, "--max-size="+strconv.Itoa(s.MaxSize))
	}
	if s.BitRate > 0 {
		r = append(r, "--video-bit-rate="+strconv.Itoa(s.BitRate)+"M")
	}
	if s.MaxFps > 0 {
		r = append(r, "--max-fps="+strconv.Itoa(s.MaxFps))
	}
	if s.VideoCodec != "" {
		r = append(r, "--video-codec="+s.VideoCodec)
	}
	if !withAudio {
		r = append(r, "--no-audio")
	}
	if s.AlwaysOnTop {
		r = append(r, "--always-on-top")
	}
	return r
}

func (a *App) openApp(serial, pkg, label string) error {
	if !reAppLine.MatchString("- x " + pkg) {
		return fmt.Errorf("неверное имя приложения")
	}
	a.mu.Lock()
	d := a.findDeviceLocked(serial)
	if d == nil || d.State != "device" {
		a.mu.Unlock()
		return fmt.Errorf("телефон не найден")
	}
	dev := *d
	if label == "" {
		label = pkg
		for _, e := range a.appCache[dev.ID] {
			if e.Pkg == pkg {
				label = e.Label
			}
		}
	}
	if s := a.sessions[dev.ID+appKeySep+pkg]; s != nil && s.cmd.Process != nil {
		pid := s.cmd.Process.Pid
		a.mu.Unlock()
		focusPid(pid) // already open: bring its window up
		return nil
	}
	a.mu.Unlock()
	err := a.launchOpts(dev.Serial, launchOpts{app: pkg, appLabel: label})
	if err == nil {
		a.rememberApp(dev.ID, AppEntry{Label: label, Pkg: pkg})
	} else {
		a.event("error", "", label+": "+err.Error())
	}
	return err
}
