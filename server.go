package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed ui
var uiFS embed.FS

var appVersion = "3.0"

type instanceInfo struct {
	Port    int    `json:"port"`
	Token   string `json:"token"`
	Pid     int    `json:"pid"`
	Version string `json:"version"`
}

func argValue(name string) string {
	for _, x := range os.Args[1:] {
		if strings.HasPrefix(x, name+"=") {
			return strings.TrimPrefix(x, name+"=")
		}
	}
	return ""
}

// legacyInstanceRunning: our server answers 403 "forbidden" without a token.
func legacyInstanceRunning() bool {
	c := http.Client{Timeout: 700 * time.Millisecond}
	for p := 47321; p < 47331; p++ {
		resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ping", p))
		if err != nil {
			continue
		}
		body := make([]byte, 64)
		n, _ := resp.Body.Read(body)
		resp.Body.Close()
		if resp.StatusCode == 403 && strings.HasPrefix(string(body[:n]), "forbidden") {
			return true
		}
	}
	return false
}

func pingInstance(inf instanceInfo) (version string, ok bool) {
	c := http.Client{Timeout: 1500 * time.Millisecond}
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/api/ping", inf.Port), nil)
	req.Header.Set("X-Token", inf.Token)
	resp, err := c.Do(req)
	if err != nil || resp.StatusCode != 200 {
		return "", false
	}
	defer resp.Body.Close()
	var r struct {
		Version string `json:"version"`
	}
	json.NewDecoder(resp.Body).Decode(&r)
	return r.Version, true
}

func main() {
	if hasArg("--version") {
		fmt.Print(appVersion)
		return
	}
	a := NewApp()
	profileDir := filepath.Join(a.cfgDir, ".launcher-window")
	// One lock for the whole user (not per folder), so a copy started from another
	// folder still finds the running one.
	lockDir := a.cfgDir
	if d, err := os.UserCacheDir(); err == nil {
		profileDir = filepath.Join(d, "ScrcpyLauncher", "window")
		lockDir = filepath.Join(d, "ScrcpyLauncher")
		os.MkdirAll(lockDir, 0755)
	}
	if d := os.Getenv("LAUNCHER_CONFIG_DIR"); d != "" {
		lockDir = d
	}
	lockPath := filepath.Join(lockDir, "instance.json")
	afterUpdate := hasArg("--after-update")
	go a.cleanupOld()
	if _, err := os.Stat(lockPath); err != nil && !afterUpdate {
		// Versions before 2.4 kept their lock in their own folder: find one by its
		// port and close it, so this version can take over cleanly.
		if legacyInstanceRunning() {
			killOtherInstances()
			a.adb(6*time.Second, "kill-server")
			time.Sleep(time.Second)
		}
	}
	fixAutostartPath()

	// Single instance: if already running, just bring up its window. If the running
	// one is a different version, it's asked to step aside.
	if b, err := os.ReadFile(lockPath); err == nil && !afterUpdate {
		var inf instanceInfo
		if json.Unmarshal(b, &inf) == nil && inf.Port > 0 {
			c := http.Client{Timeout: 1500 * time.Millisecond}
			if ver, alive := pingInstance(inf); alive && ver != appVersion {
				req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/api/quit", inf.Port), strings.NewReader("{}"))
				req.Header.Set("X-Token", inf.Token)
				if r, err := c.Do(req); err == nil {
					r.Body.Close()
				}
				for i := 0; i < 40; i++ {
					if _, still := pingInstance(inf); !still {
						break
					}
					time.Sleep(200 * time.Millisecond)
				}
				if _, still := pingInstance(inf); still && inf.Pid > 0 {
					if p, err := os.FindProcess(inf.Pid); err == nil {
						p.Kill()
					}
				}
			} else if alive {
				if !hasArg("--tray") {
					// Let the running copy bring its window to the front (or open it).
					allowOtherForeground()
					req, _ := http.NewRequest("POST", fmt.Sprintf("http://127.0.0.1:%d/api/show", inf.Port), strings.NewReader("{}"))
					req.Header.Set("X-Token", inf.Token)
					if r2, err := c.Do(req); err == nil {
						r2.Body.Close()
					} else {
						openAppWindow(fmt.Sprintf("http://127.0.0.1:%d/?t=%s", inf.Port, inf.Token), profileDir)
					}
				}
				return
			}
		}
	}

	var ln net.Listener
	var err error
	token := randString(24, "abcdefghijklmnopqrstuvwxyz0123456789")
	if afterUpdate {
		// Take over the old version's port and token, so its open window keeps working.
		if t := argValue("--token"); len(t) >= 16 {
			token = t
		}
		if p, _ := strconv.Atoi(argValue("--port")); p > 0 {
			for i := 0; i < 60 && ln == nil; i++ {
				if ln, err = net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p)); err != nil {
					ln = nil
					time.Sleep(200 * time.Millisecond)
				}
			}
		}
	}
	for p := 47321; p < 47331 && ln == nil; p++ {
		if ln, err = net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p)); err != nil {
			ln = nil
		}
	}
	if ln == nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			panic(err)
		}
	}
	port := ln.Addr().(*net.TCPAddr).Port
	b, _ := json.Marshal(instanceInfo{port, token, os.Getpid(), appVersion})
	os.WriteFile(lockPath, b, 0600)
	var updating atomic.Bool
	defer func() {
		// Only remove the lock if it's still ours (a new version may own it already).
		if cur, err := os.ReadFile(lockPath); err == nil {
			var inf instanceInfo
			if json.Unmarshal(cur, &inf) == nil && inf.Pid == os.Getpid() {
				os.Remove(lockPath)
			}
		}
		// Free adb.exe so the folder can be overwritten — unless a new version
		// has just taken over and is using it.
		if !updating.Load() {
			a.stopAdbIfIdle()
		}
	}()

	var lastHit atomic.Int64
	lastHit.Store(time.Now().Unix())
	var byeAt atomic.Int64
	var hits atomic.Int64
	var trayActive atomic.Bool
	quit := make(chan struct{})
	var quitOnce sync.Once
	doQuit := func() { quitOnce.Do(func() { close(quit) }) }
	// Installs a new version from a zip/exe and hands over to it (same port/token,
	// so an open window just reloads).
	a.applySelfUpdate = func(r io.Reader, name string) error {
		if _, err := a.selfUpdate(r, name, port, token); err != nil {
			return err
		}
		updating.Store(true)
		go func() { time.Sleep(300 * time.Millisecond); trayQuit(); time.Sleep(700 * time.Millisecond); doQuit() }()
		return nil
	}

	url := fmt.Sprintf("http://127.0.0.1:%d/?t=%s", port, token)
	var lastOpen atomic.Int64
	showWindow := func() {
		if focusAppWindow(windowTitle) {
			return
		}
		if time.Now().Unix()-lastOpen.Load() < 4 {
			return // window is still starting
		}
		lastOpen.Store(time.Now().Unix())
		openAppWindow(url, profileDir)
	}

	mux := http.NewServeMux()
	sub, _ := fs.Sub(uiFS, "ui")
	static := http.FileServer(http.FS(sub))
	mux.Handle("/", static)

	api := func(path string, h func(w http.ResponseWriter, r *http.Request)) {
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Token")), []byte(token)) != 1 &&
				subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("t")), []byte(token)) != 1 {
				http.Error(w, "forbidden", 403)
				return
			}
			lastHit.Store(time.Now().Unix())
			hits.Add(1)
			byeAt.Store(0)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			h(w, r)
		})
	}
	writeJSON := func(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(v) }
	readJSON := func(r *http.Request, v any) error {
		defer r.Body.Close()
		return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<16)).Decode(v)
	}
	okErr := func(w http.ResponseWriter, err error) {
		if err != nil {
			writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		} else {
			writeJSON(w, map[string]any{"ok": true})
		}
	}

	api("/api/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ok": true, "version": appVersion})
	})
	api("/api/quit", func(w http.ResponseWriter, r *http.Request) {
		// Another version is taking over: close our scrcpy windows (it reopens them).
		okErr(w, nil)
		go func() {
			a.mu.Lock()
			var ids []string
			for id := range a.sessions {
				ids = append(ids, id)
			}
			a.mu.Unlock()
			for _, id := range ids {
				a.stopWait(id)
			}
			trayQuit()
			time.Sleep(700 * time.Millisecond)
			doQuit()
		}()
	})
	api("/api/selfupdate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		defer r.Body.Close()
		if err := a.applySelfUpdate(r.Body, r.URL.Query().Get("name")); err != nil {
			okErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	})
	api("/api/appupdate/check", func(w http.ResponseWriter, r *http.Request) {
		okErr(w, a.checkAppUpdate())
	})
	api("/api/appupdate/install", func(w http.ResponseWriter, r *http.Request) {
		go a.installAppUpdate()
		okErr(w, nil)
	})

	api("/api/state", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		since, _ := strconv.Atoi(r.URL.Query().Get("log"))
		if since < 0 || since > len(a.logs) {
			since = 0
		}
		known := []KnownDevice{}
		for _, k := range a.cfg.Known {
			known = append(known, *k)
		}
		pair := a.pair
		devs := a.devices
		if devs == nil {
			devs = []DeviceView{}
		}
		scan := a.lastScan
		if scan == nil {
			scan = []MdnsService{}
		}
		writeJSON(w, map[string]any{
			"devices":  devs,
			"known":    known,
			"settings": a.cfg.Settings,
			"pair":     pair,
			"logs":     append([]string{}, a.logs[since:]...),
			"logLen":   len(a.logs),
			"adbError": a.adbErr,
			"scan":     scan,
			"version":  appVersion,
			"toolsOk":  fileExists(a.scrcpy) && fileExists(a.adbPath),
			"toolDir":  a.toolDir,
			"events":   append([]Event{}, a.events...),
			"update":   a.upd,
			"caps": map[string]bool{"tray": trayActive.Load(), "autostart": autostartSupported(),
				"shortcuts": shortcutsSupported(), "sfx": nativeSfx},
			"autostart": autostartEnabled(),
			"uiReq":     a.uiReq,
			"profiles":  a.profilesLocked(),
			"profile":   a.currentProfileLocked(),
			"hotkeyErr": a.hkErr,
			"pull":      a.pull,
			"appUpdate": a.appUpd,
			"shotDir":   a.shotDirLocked(),
			"videoDir":  a.videoDirLocked(),
		})
	})

	api("/api/settings", func(w http.ResponseWriter, r *http.Request) {
		var s Settings
		if err := readJSON(r, &s); err != nil {
			okErr(w, err)
			return
		}
		a.mu.Lock()
		toolbarChanged := a.cfg.Settings.Toolbar != s.Toolbar
		hkChanged := a.cfg.Settings.HotkeyShot != s.HotkeyShot || a.cfg.Settings.HotkeyRec != s.HotkeyRec
		a.cfg.Settings = s
		a.saveLocked()
		a.mu.Unlock()
		if toolbarChanged {
			a.applyToolbar()
		}
		if hkChanged {
			a.applyHotkeys()
		}
		okErr(w, nil)
	})

	api("/api/launch", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Serial     string `json:"serial"`
			Fullscreen *bool  `json:"fullscreen"`
			Place      *Place `json:"place"`
			RevealMs   int    `json:"revealMs"`
		}
		readJSON(r, &q)
		o := launchOpts{fullscreen: q.Fullscreen}
		if q.Place != nil && q.Place.W > 0.05 && q.Place.H > 0.05 && q.Place.W <= 1 && q.Place.H <= 1 {
			o.place = q.Place
		}
		if q.RevealMs > 0 && q.RevealMs < 5000 {
			o.revealAt = time.Now().Add(time.Duration(q.RevealMs) * time.Millisecond)
		}
		okErr(w, a.launchOpts(q.Serial, o))
	})

	api("/api/stop", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ ID string }
		readJSON(r, &q)
		a.stop(q.ID)
		okErr(w, nil)
	})

	api("/api/push", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		defer r.Body.Close()
		msg, err := a.pushFile(r.URL.Query().Get("serial"), r.URL.Query().Get("name"), r.Body)
		if err != nil {
			okErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "message": msg})
	})

	api("/api/camera", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Serial, Facing string }
		readJSON(r, &q)
		if q.Facing != "front" {
			q.Facing = "back"
		}
		okErr(w, a.cameraOn(q.Serial, q.Facing))
	})

	api("/api/show", func(w http.ResponseWriter, r *http.Request) {
		go showWindow()
		okErr(w, nil)
	})

	api("/api/update/check", func(w http.ResponseWriter, r *http.Request) {
		a.checkUpdate(true)
		okErr(w, nil)
	})
	api("/api/update/install", func(w http.ResponseWriter, r *http.Request) {
		go a.installUpdate()
		okErr(w, nil)
	})
	api("/api/openurl", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ URL string }
		readJSON(r, &q)
		if !strings.HasPrefix(q.URL, "https://github.com/Genymobile/scrcpy") {
			okErr(w, fmt.Errorf("недопустимая ссылка"))
			return
		}
		openPath(q.URL, false)
		okErr(w, nil)
	})
	api("/api/autostart", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ On bool }
		readJSON(r, &q)
		okErr(w, setAutostart(q.On))
	})
	api("/api/shortcut", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Where string }
		readJSON(r, &q)
		p, err := createShortcut(q.Where)
		if err != nil {
			okErr(w, fmt.Errorf("ярлык не создан: %v", err))
			return
		}
		a.logf("Ярлык создан: %s", p)
		writeJSON(w, map[string]any{"ok": true, "path": p})
	})

	api("/api/apps", func(w http.ResponseWriter, r *http.Request) {
		serial := r.URL.Query().Get("serial")
		apps, err := a.listApps(serial, r.URL.Query().Get("refresh") == "1")
		if err != nil {
			okErr(w, err)
			return
		}
		a.mu.Lock()
		id := serial
		if d := a.findDeviceLocked(serial); d != nil {
			id = d.ID
		}
		a.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "apps": apps, "recent": a.recentApps(id)})
	})
	api("/api/app/open", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Serial, Pkg, Label string }
		readJSON(r, &q)
		okErr(w, a.openApp(q.Serial, q.Pkg, q.Label))
	})

	api("/api/profile/apply", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Name string }
		readJSON(r, &q)
		n, err := a.applyProfile(q.Name)
		if err != nil {
			okErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "restarted": n})
	})
	api("/api/profile/save", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Name string }
		readJSON(r, &q)
		okErr(w, a.saveProfile(q.Name))
	})
	api("/api/profile/delete", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Name string }
		readJSON(r, &q)
		a.deleteProfile(q.Name)
		okErr(w, nil)
	})

	api("/api/conntest", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Serial string }
		readJSON(r, &q)
		res, err := a.connTest(q.Serial, true)
		if err != nil {
			okErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "result": res})
	})

	api("/api/thumb", func(w http.ResponseWriter, r *http.Request) {
		jpg, pw, ph := a.thumbnail(r.URL.Query().Get("serial"))
		if jpg == nil {
			w.WriteHeader(204)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("X-Screen-Size", strconv.Itoa(pw)+"x"+strconv.Itoa(ph))
		w.Write(jpg)
	})

	api("/api/call", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Serial, Action string }
		readJSON(r, &q)
		if q.Action != "answer" && q.Action != "decline" && q.Action != "hangup" {
			okErr(w, fmt.Errorf("неизвестное действие"))
			return
		}
		okErr(w, a.callAction(q.Serial, q.Action))
	})

	api("/api/phone/media", func(w http.ResponseWriter, r *http.Request) {
		items, err := a.listPhoneMedia(r.URL.Query().Get("serial"))
		if err != nil {
			okErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "items": items})
	})
	api("/api/phone/thumb", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		serial := r.URL.Query().Get("serial")
		if d := a.findDeviceLocked(serial); d != nil {
			serial = d.Serial
		}
		a.mu.Unlock()
		jpg, err := a.phoneThumb(serial, r.URL.Query().Get("path"))
		if err != nil {
			w.WriteHeader(204)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "private, max-age=86400")
		w.Write(jpg)
	})
	api("/api/phone/pull", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Serial string
			Paths  []string
		}
		readJSON(r, &q)
		okErr(w, a.pullMedia(q.Serial, q.Paths))
	})
	api("/api/gallery", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		dirs := map[string]string{"shot": a.shotDirLocked(), "video": a.videoDirLocked(), "phone": a.photoDirLocked()}
		a.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "items": a.gallery(), "dirs": dirs})
	})
	api("/api/gallery/file", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		if !a.allowedPath(p) {
			http.Error(w, "forbidden", 403)
			return
		}
		w.Header().Del("Content-Type")
		w.Header().Del("Cache-Control")
		serveGalleryFile(w, r, p)
	})
	api("/api/gallery/delete", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Path string }
		readJSON(r, &q)
		if !a.allowedPath(q.Path) {
			okErr(w, fmt.Errorf("недопустимый путь"))
			return
		}
		okErr(w, trashFile(q.Path))
	})
	api("/api/gallery/copy", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Path string }
		readJSON(r, &q)
		if !a.allowedPath(q.Path) {
			okErr(w, fmt.Errorf("недопустимый путь"))
			return
		}
		okErr(w, copyFileToClipboard(q.Path))
	})

	api("/api/remote", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Serial, Action string }
		readJSON(r, &q)
		st, err := a.remote(q.Serial, q.Action)
		if err != nil {
			okErr(w, err)
			return
		}
		a.mu.Lock()
		a.syncRunningLocked()
		a.mu.Unlock()
		writeJSON(w, map[string]any{"ok": true, "state": st})
	})

	api("/api/sfx", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Name string }
		readJSON(r, &q)
		go a.sfx(q.Name)
		okErr(w, nil)
	})

	api("/api/screenshot", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Serial string }
		readJSON(r, &q)
		p, err := a.screenshot(q.Serial)
		if err != nil {
			okErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "path": p})
	})

	api("/api/record", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Serial string
			On     bool
		}
		readJSON(r, &q)
		okErr(w, a.setRecording(q.Serial, q.On))
	})

	api("/api/open", func(w http.ResponseWriter, r *http.Request) {
		var q struct {
			Path   string
			Reveal bool
			Folder string
		}
		readJSON(r, &q)
		a.mu.Lock()
		switch q.Folder {
		case "shots":
			q.Path = a.shotDirLocked()
		case "videos":
			q.Path = a.videoDirLocked()
		}
		a.mu.Unlock()
		if !a.allowedPath(q.Path) {
			okErr(w, fmt.Errorf("недопустимый путь"))
			return
		}
		if q.Folder != "" {
			os.MkdirAll(q.Path, 0755)
		}
		openPath(q.Path, q.Reveal)
		okErr(w, nil)
	})

	api("/api/pair/qr", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, a.startQRPairing())
	})
	api("/api/pair/cancel", func(w http.ResponseWriter, r *http.Request) {
		a.cancelPair()
		okErr(w, nil)
	})
	api("/api/pair/code", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Addr, Code string }
		readJSON(r, &q)
		q.Addr, q.Code = strings.TrimSpace(q.Addr), strings.TrimSpace(q.Code)
		if h, p := splitHostPort(q.Addr); h == "" || p == 0 {
			okErr(w, fmt.Errorf("укажите адрес в виде IP:порт, например 192.168.1.50:41234"))
			return
		}
		if q.Code == "" {
			okErr(w, fmt.Errorf("введите код с телефона"))
			return
		}
		a.pairWithCode(q.Addr, q.Code)
		okErr(w, nil)
	})

	api("/api/connect", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Addr string }
		readJSON(r, &q)
		addr := strings.TrimSpace(q.Addr)
		if !strings.Contains(addr, ":") {
			addr += ":5555"
		}
		ok, msg := a.connect(addr, false)
		if !ok {
			okErr(w, fmt.Errorf("%s", msg))
			return
		}
		okErr(w, nil)
	})

	api("/api/disconnect", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Serial string }
		readJSON(r, &q)
		out, _ := a.adb(5*time.Second, "disconnect", q.Serial)
		a.logf("adb disconnect %s → %s", q.Serial, strings.TrimSpace(out))
		a.poke()
		okErr(w, nil)
	})

	api("/api/tcpip", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ Serial string }
		readJSON(r, &q)
		addr, err := a.usbToWifi(q.Serial)
		if err != nil {
			okErr(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "addr": addr})
	})

	api("/api/forget", func(w http.ResponseWriter, r *http.Request) {
		var q struct{ ID string }
		readJSON(r, &q)
		a.mu.Lock()
		var keep []*KnownDevice
		for _, k := range a.cfg.Known {
			if k.ID != q.ID {
				keep = append(keep, k)
			}
		}
		a.cfg.Known = keep
		a.saveLocked()
		a.mu.Unlock()
		okErr(w, nil)
	})

	api("/api/rescan", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.tried = map[string]time.Time{}
		a.mu.Unlock()
		go func() { a.reconnectKnown(a.discover(2 * time.Second)); a.poke() }()
		okErr(w, nil)
	})

	api("/api/bye", func(w http.ResponseWriter, r *http.Request) {
		byeAt.Store(time.Now().Unix())
		okErr(w, nil)
	})

	srv := &http.Server{Handler: hostGuard(mux, port), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	go a.watcher()

	if os.Getenv("LAUNCHER_URL_FILE") != "" {
		os.WriteFile(os.Getenv("LAUNCHER_URL_FILE"), []byte(url), 0644)
	}
	go a.updateLoop()
	go a.appUpdateLoop()
	go a.batteryLoop()
	go a.phoneEventsLoop()
	a.applyHotkeys()
	a.showUI = showWindow

	trayMenu := func() []TrayItem {
		items := []TrayItem{{Text: "Открыть окно", Action: showWindow}}
		a.mu.Lock()
		var devs []DeviceView
		for _, d := range a.devices {
			if d.State == "device" {
				devs = append(devs, d)
			}
		}
		auto := a.cfg.Settings.AutoLaunch
		a.mu.Unlock()
		if len(devs) > 0 {
			items = append(items, TrayItem{Separator: true})
		}
		for _, d := range devs {
			d := d
			items = append(items, TrayItem{Text: "Показать экран — " + d.Name, Checked: d.Running, Disabled: d.Running,
				Action: func() { a.launch(d.Serial, nil) }})
		}
		a.mu.Lock()
		profs, curProf := a.profilesLocked(), a.currentProfileLocked()
		a.mu.Unlock()
		items = append(items, TrayItem{Separator: true})
		for _, p := range profs {
			name := p.Name
			items = append(items, TrayItem{Text: "Профиль: " + name, Checked: name == curProf,
				Action: func() { a.applyProfile(name) }})
		}
		items = append(items, TrayItem{Separator: true},
			TrayItem{Text: "Открывать экран сразу при подключении", Checked: auto, Action: func() {
				a.mu.Lock()
				a.cfg.Settings.AutoLaunch = !a.cfg.Settings.AutoLaunch
				a.saveLocked()
				a.mu.Unlock()
			}},
			TrayItem{Text: "Запускать вместе с Windows", Checked: autostartEnabled(), Action: func() {
				setAutostart(!autostartEnabled())
			}},
			TrayItem{Separator: true},
			TrayItem{Text: "Выход", Action: func() { trayQuit(); time.Sleep(2 * time.Second); doQuit() }},
		)
		return items
	}
	ok, trayDone := startTray("Телефон на ПК", showWindow, trayMenu)
	trayActive.Store(ok)
	if ok {
		go func() { <-trayDone; doQuit() }()
	}
	if afterUpdate {
		go func() {
			time.Sleep(1500 * time.Millisecond)
			trayNotify("TVPhone обновлён до версии "+appVersion, "Всё работает как раньше — можно пользоваться.")
		}()
		// The old version's window reloads itself onto us; open one only if it doesn't.
		go func() {
			time.Sleep(10 * time.Second)
			if hits.Load() == 0 {
				showWindow()
			}
		}()
	} else if !hasArg("--tray") || !ok {
		lastOpen.Store(time.Now().Unix())
		openAppWindow(url, profileDir)
	}

	// Without the tray: quit when the window is closed (beacon) or nobody has polled
	// for a long time. With the tray: keep waiting for the phone in the background.
	lastTip := ""
	for {
		select {
		case <-quit:
			return
		case <-time.After(time.Second):
		}
		now := time.Now().Unix()
		a.mu.Lock()
		stay := ok && a.cfg.Settings.StayInTray
		tip := trayTipText(a.devices)
		a.mu.Unlock()
		if ok && tip != lastTip {
			lastTip = tip
			traySetTip(tip)
		}
		closed := byeAt.Load() != 0 && now-byeAt.Load() >= 5 && now-lastHit.Load() >= 5
		if stay {
			if closed {
				byeAt.Store(0)
				a.mu.Lock()
				first := !a.cfg.TrayHintShown
				a.cfg.TrayHintShown = true
				a.saveLocked()
				a.mu.Unlock()
				if first {
					trayNotify("Телефон на ПК работает в трее",
						"Включите на телефоне «Отладку по Wi-Fi» — экран откроется сам. Выход — правой кнопкой по значку.")
				}
			}
			continue
		}
		if closed || now-lastHit.Load() > 240 {
			if ok {
				trayQuit()
				select {
				case <-trayDone:
				case <-time.After(2 * time.Second):
				}
			}
			return
		}
	}
}

const windowTitle = "scrcpy — телефон на ПК"

func hasArg(a string) bool {
	for _, x := range os.Args[1:] {
		if x == a {
			return true
		}
	}
	return false
}

func trayTipText(devs []DeviceView) string {
	var names []string
	for _, d := range devs {
		if d.State == "device" {
			n := d.Name
			if d.Battery >= 0 {
				n += " " + strconv.Itoa(d.Battery) + "%"
				if d.Charging {
					n += "⚡"
				}
			}
			if d.Running {
				n += " (экран открыт)"
			}
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return "Телефон на ПК — жду телефон"
	}
	return "Телефон на ПК — " + strings.Join(names, ", ")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// hostGuard blocks DNS-rebinding: only accept our own loopback host names.
func hostGuard(h http.Handler, port int) http.Handler {
	ok := map[string]bool{
		"127.0.0.1:" + strconv.Itoa(port): true,
		"localhost:" + strconv.Itoa(port): true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ok[r.Host] {
			http.Error(w, "bad host", 403)
			return
		}
		h.ServeHTTP(w, r)
	})
}
