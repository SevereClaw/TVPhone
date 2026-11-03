package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Settings struct {
	MaxSize      int    `json:"maxSize"`
	BitRate      int    `json:"bitRate"`
	MaxFps       int    `json:"maxFps"`
	VideoCodec   string `json:"videoCodec"`
	KeyboardUHID bool   `json:"keyboardUhid"` // legacy, replaced by KeyboardMode
	// KeyboardMode: "text" — letters sent as text (any language works as-is, default);
	// "uhid" — a real HID keyboard (best for games; set the layout on the phone);
	// "keys" — plain key events.
	KeyboardMode    string `json:"keyboardMode"`
	StayAwake       bool   `json:"stayAwake"`
	TurnScreenOff   bool   `json:"turnScreenOff"`
	Audio           bool   `json:"audio"`
	Fullscreen      bool   `json:"fullscreen"`
	AlwaysOnTop     bool   `json:"alwaysOnTop"`
	PowerOffOnClose bool   `json:"powerOffOnClose"`
	ExtraArgs       string `json:"extraArgs"`
	AutoLaunch      bool   `json:"autoLaunch"`
	LaunchAfterPair bool   `json:"launchAfterPair"`
	CameraMic       bool   `json:"cameraMic"`
	StayInTray      bool   `json:"stayInTray"`
	Toolbar         bool   `json:"toolbar"`
	UISounds        bool   `json:"uiSounds"`
	AutoUpdateApp   bool   `json:"autoUpdateApp"`
	PhoneNotifs     bool   `json:"phoneNotifs"`
	NotifText       bool   `json:"notifText"`
	PhoneCalls      bool   `json:"phoneCalls"`
	AudioDup        bool   `json:"audioDup"`
	VideoBuffer     int    `json:"videoBuffer"` // ms, Wi-Fi only
	AudioBuffer     int    `json:"audioBuffer"` // ms, Wi-Fi only
	AutoProfile     bool   `json:"autoProfile"`
	HotkeyShot      string `json:"hotkeyShot"`
	HotkeyRec       string `json:"hotkeyRec"`
	ShotDir         string `json:"shotDir"`
	VideoDir        string `json:"videoDir"`
	PhotoDir        string `json:"photoDir"`
}

func defaultSettings() Settings {
	// Matches the user's previous .bat files.
	return Settings{MaxSize: 1280, BitRate: 4, MaxFps: 60, KeyboardMode: "text", StayAwake: true,
		Audio: true, AutoLaunch: true, LaunchAfterPair: true, StayInTray: true, Toolbar: true, AutoProfile: true, UISounds: true, AutoUpdateApp: true, PhoneNotifs: true, NotifText: true, PhoneCalls: true,
		VideoBuffer: 30, AudioBuffer: 100,
		HotkeyShot: "Ctrl+Shift+S", HotkeyRec: "Ctrl+Shift+R"}
}

type KnownDevice struct {
	ID       string `json:"id"` // ro.serialno
	Name     string `json:"name"`
	IP       string `json:"ip"`
	Port     int    `json:"port"`
	LastSeen int64  `json:"lastSeen"`
}

type Config struct {
	Settings        Settings              `json:"settings"`
	Known           []*KnownDevice        `json:"known"`
	UpdateCheckedAt int64                 `json:"updateCheckedAt"`
	NotifiedVersion string                `json:"notifiedVersion"`
	TrayHintShown   bool                  `json:"trayHintShown"`
	Recent          map[string][]AppEntry `json:"recentApps"`
	Profiles        []Profile             `json:"profiles"`
	HEVC            map[string]string     `json:"hevc"`        // device id -> hardware H.265 encoder ("" = none)
	AudioSrc        map[string]string     `json:"audioSource"` // device id -> audio capture source that works
}

type devProps struct {
	ID   string
	Name string
}

type DeviceView struct {
	Serial    string    `json:"serial"`
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Transport string    `json:"transport"`
	State     string    `json:"state"`
	Addr      string    `json:"addr"`
	Running   bool      `json:"running"`
	Recording bool      `json:"recording"`
	Shown     bool      `json:"shown"`
	Fps       int       `json:"fps"`
	DND       bool      `json:"dnd"`
	Torch     bool      `json:"torch"`
	AudioNote string    `json:"audioNote,omitempty"`
	Camera    string    `json:"camera"`
	Apps      []AppRun  `json:"apps"`
	Battery   int       `json:"battery"` // -1 = unknown
	Link      string    `json:"link"`
	Call      *CallInfo `json:"call"`
	Charging  bool      `json:"charging"`
	// unix seconds when recording started
	RecordSince int64 `json:"recordSince"`
}

type AppRun struct {
	Pkg   string `json:"pkg"`
	Label string `json:"label"`
	Key   string `json:"key"`
}

type PairState struct {
	Active  bool   `json:"active"`
	Stage   string `json:"stage"`
	Message string `json:"message"`
	QR      string `json:"qr,omitempty"`
	Name    string `json:"name,omitempty"`
	Pass    string `json:"pass,omitempty"`
	gen     int
}

type App struct {
	mu              sync.Mutex
	toolDir         string
	adbPath         string
	scrcpy          string
	cfgPath         string
	cfgDir          string
	cfg             Config
	devices         []DeviceView
	props           map[string]devProps // serial -> props
	sessions        map[string]*session
	events          []Event
	evSeq           int
	online          map[string]bool // id -> seen online during this run
	tried           map[string]time.Time
	logs            []string
	pair            PairState
	lastScan        []MdnsService
	adbErr          string
	wake            chan struct{}
	upd             UpdateState
	appCache        map[string][]AppEntry
	battery         map[string]*battery
	calls           map[string]*CallInfo
	dnd             map[string]bool
	appUpd          AppUpdState
	applySelfUpdate func(r io.Reader, name string) error
	pull            pullJob
	conn            map[string]*ConnResult
	enc             map[string]*encInfo
	batWake         chan struct{}
	hkErr           string
	uiReq           UIRequest
	audioNote       map[string]string
	showUI          func()
}

func NewApp() *App {
	exe, _ := os.Executable()
	exeDir := filepath.Dir(exe)
	toolDir := exeDir
	if d := os.Getenv("SCRCPY_DIR"); d != "" {
		toolDir = d
	}
	a := &App{toolDir: toolDir, props: map[string]devProps{}, sessions: map[string]*session{},
		online: map[string]bool{}, tried: map[string]time.Time{}, wake: make(chan struct{}, 1),
		appCache: map[string][]AppEntry{}, battery: map[string]*battery{}, calls: map[string]*CallInfo{}, dnd: map[string]bool{}, conn: map[string]*ConnResult{}, enc: map[string]*encInfo{}, batWake: make(chan struct{}, 1)}
	a.adbPath = findTool(toolDir, "adb")
	a.scrcpy = findTool(toolDir, "scrcpy")

	// Portable config next to the exe; fall back to %APPDATA%.
	a.cfgDir = exeDir
	if !dirWritable(exeDir) {
		if d, err := os.UserConfigDir(); err == nil {
			a.cfgDir = filepath.Join(d, "ScrcpyLauncher")
			os.MkdirAll(a.cfgDir, 0755)
		}
	}
	if d := os.Getenv("LAUNCHER_CONFIG_DIR"); d != "" {
		a.cfgDir = d
		os.MkdirAll(d, 0755)
	}
	a.cfgPath = filepath.Join(a.cfgDir, "scrcpy-launcher.json")
	a.cfg.Settings = defaultSettings()
	if b, err := os.ReadFile(a.cfgPath); err == nil {
		json.Unmarshal(b, &a.cfg)
	}
	return a
}

func findTool(dir, name string) string {
	p := filepath.Join(dir, name+exeSuffix)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return filepath.Join(dir, name+exeSuffix)
}

func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".wtest")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

func (a *App) saveLocked() {
	b, _ := json.MarshalIndent(a.cfg, "", "  ")
	tmp := a.cfgPath + ".tmp"
	if os.WriteFile(tmp, b, 0644) == nil {
		os.Rename(tmp, a.cfgPath)
	}
}

func (a *App) logf(format string, args ...any) {
	line := time.Now().Format("15:04:05") + "  " + fmt.Sprintf(format, args...)
	a.mu.Lock()
	a.logs = append(a.logs, line)
	if len(a.logs) > 400 {
		a.logs = a.logs[len(a.logs)-400:]
	}
	a.mu.Unlock()
}

func (a *App) poke() {
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func randString(n int, alphabet string) string {
	b := make([]byte, n)
	for i := range b {
		v, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		b[i] = alphabet[v.Int64()]
	}
	return string(b)
}

func splitHostPort(s string) (string, int) {
	h, p, err := net.SplitHostPort(strings.TrimSpace(s))
	if err != nil {
		return "", 0
	}
	n, _ := strconv.Atoi(p)
	return h, n
}

// ---------------------------------------------------------------- discovery

func (a *App) discover(wait time.Duration) []MdnsService {
	var wg sync.WaitGroup
	var own, viaAdb []MdnsService
	wg.Add(2)
	go func() { defer wg.Done(); own = BrowseMdns(wait) }()
	go func() {
		defer wg.Done()
		if out, err := a.adb(wait+3*time.Second, "mdns", "services"); err == nil {
			viaAdb = parseAdbMdns(out)
		}
	}()
	wg.Wait()
	seen := map[string]bool{}
	var res []MdnsService
	for _, s := range append(own, viaAdb...) {
		k := s.Service + "|" + strings.ToLower(s.Instance) + "|" + s.Addr()
		if !seen[k] {
			seen[k] = true
			res = append(res, s)
		}
	}
	a.mu.Lock()
	a.lastScan = res
	a.mu.Unlock()
	return res
}

func (a *App) fetchProps(serial string) devProps {
	a.mu.Lock()
	p, ok := a.props[serial]
	a.mu.Unlock()
	if ok {
		return p
	}
	out, err := a.adb(6*time.Second, "-s", serial, "shell",
		"getprop ro.serialno; getprop ro.product.marketname; getprop ro.vendor.oplus.market.name; getprop ro.product.model; getprop ro.product.manufacturer")
	if err != nil {
		return devProps{}
	}
	lines := strings.Split(strings.ReplaceAll(out, "\r", ""), "\n")
	get := func(i int) string {
		if i < len(lines) {
			return strings.TrimSpace(lines[i])
		}
		return ""
	}
	p.ID = get(0)
	p.Name = get(1)
	if p.Name == "" {
		p.Name = get(2)
	}
	if p.Name == "" {
		m, man := get(3), get(4)
		if man != "" && !strings.HasPrefix(strings.ToLower(m), strings.ToLower(man)) {
			m = man + " " + m
		}
		p.Name = m
	}
	if p.ID == "" {
		p.ID = serial
	}
	a.mu.Lock()
	a.props[serial] = p
	a.mu.Unlock()
	return p
}

func (a *App) refreshDevices() {
	list, err := a.adbDevices()
	if err != nil {
		a.mu.Lock()
		a.adbErr = "adb не отвечает: " + err.Error()
		a.mu.Unlock()
		return
	}
	views := []DeviceView{}
	byID := map[string]int{}
	for _, d := range list {
		v := DeviceView{Serial: d.Serial, State: d.State, Name: d.Model, Transport: "usb"}
		if isWifiSerial(d.Serial) {
			v.Transport = "wifi"
			if h, _ := splitHostPort(d.Serial); h != "" {
				v.Addr = d.Serial
			}
		}
		if d.State == "device" {
			p := a.fetchProps(d.Serial)
			v.ID = p.ID
			if p.Name != "" {
				v.Name = p.Name
			}
		} else if d.State == "offline" && v.Transport == "wifi" {
			// Stale Wi-Fi entry (phone changed port or went away) – clean it up.
			a.adb(4*time.Second, "disconnect", d.Serial)
			continue
		}
		if v.ID == "" {
			v.ID = d.Serial
		}
		if v.Name == "" {
			v.Name = d.Serial
		}
		// Same phone over USB and Wi-Fi: keep one card, prefer Wi-Fi "ip:port".
		if i, ok := byID[v.ID]; ok && v.State == "device" {
			if views[i].Transport == "usb" && v.Transport == "wifi" {
				views[i].Transport = "usb+wifi"
			} else if views[i].Transport == "wifi" && v.Transport == "usb" {
				views[i].Transport = "usb+wifi"
			}
			continue
		}
		byID[v.ID] = len(views)
		views = append(views, v)
	}

	a.mu.Lock()
	a.adbErr = ""
	now := time.Now().Unix()
	var toLaunch, justOnline []DeviceView
	current := map[string]bool{}
	for i := range views {
		v := &views[i]
		if c := a.sessions[v.ID+camSuffix]; c != nil {
			v.Camera = c.camera
		}
		v.Apps = a.appRunsLocked(v.ID)
		if c := a.conn[v.ID]; c != nil {
			v.Link = c.SummaryTxt
		}
		v.Call = a.calls[v.ID]
		_, v.Torch = a.sessions[v.ID+torchSuffix]
		v.DND = a.dnd[v.ID]
		v.Battery = -1
		if b := a.battery[v.ID]; b != nil && time.Since(b.At) < 5*time.Minute {
			v.Battery, v.Charging = b.Level, b.Charging
		}
		if s := a.sessions[v.ID]; s != nil {
			v.Running = true
			v.Shown = s.shown
			if time.Since(s.fpsAt) < 4*time.Second {
				v.Fps = s.fps
			}
			v.AudioNote = a.audioNote[v.ID]
			v.Recording = s.record != ""
			if v.Recording {
				v.RecordSince = s.since
			}
		}
		if v.State != "device" {
			continue
		}
		current[v.ID] = true
		k := a.knownLocked(v.ID)
		if k == nil {
			k = &KnownDevice{ID: v.ID}
			a.cfg.Known = append(a.cfg.Known, k)
		}
		k.Name = v.Name
		k.LastSeen = now
		if h, p := splitHostPort(v.Serial); h != "" {
			k.IP, k.Port = h, p
		}
		if !a.online[v.ID] {
			a.online[v.ID] = true
			go a.sfx("crt")
			if !a.cfg.Settings.AutoLaunch {
				justOnline = append(justOnline, *v)
			}
			select {
			case a.batWake <- struct{}{}:
			default:
			}
			if a.cfg.Settings.AutoLaunch && !v.Running {
				toLaunch = append(toLaunch, *v)
			}
		}
	}
	for id := range a.online {
		if !current[id] {
			delete(a.online, id)
			go a.sfx("lost")
		}
	}
	a.devices = views
	a.saveLocked()
	a.mu.Unlock()

	for _, v := range toLaunch {
		v := v
		go func() {
			a.autoProfile(v.Serial) // pick quality by connection first (~1 s)
			a.logf("Телефон %s подключён — запускаю автоматически", v.Name)
			a.launch(v.Serial, nil)
		}()
	}
	for _, v := range justOnline {
		go a.autoProfile(v.Serial)
	}
}

func (a *App) knownLocked(id string) *KnownDevice {
	for _, k := range a.cfg.Known {
		if strings.EqualFold(k.ID, id) {
			return k
		}
	}
	return nil
}

// reconnectKnown finds remembered phones on the network (the port changes every time
// wireless debugging is toggled) and connects to them.
func (a *App) reconnectKnown(services []MdnsService) {
	a.mu.Lock()
	onlineIDs := map[string]bool{}
	onlineAddr := map[string]bool{}
	for _, d := range a.devices {
		if d.State == "device" {
			onlineIDs[strings.ToLower(d.ID)] = true
			if d.Addr != "" {
				onlineAddr[d.Addr] = true
			}
		}
	}
	known := make([]KnownDevice, 0, len(a.cfg.Known))
	for _, k := range a.cfg.Known {
		known = append(known, *k)
	}
	a.mu.Unlock()

	for _, s := range services {
		if s.Service != svcConnect || onlineAddr[s.Addr()] {
			continue
		}
		id := strings.ToLower(serialFromInstance(s.Instance))
		if id != "" && onlineIDs[id] {
			continue
		}
		// Try every advertised phone once per address: if it was ever paired with
		// this PC (e.g. via the old .bat files), it just connects.
		a.mu.Lock()
		last, tried := a.tried[s.Addr()]
		isKnown := false
		for _, k := range known {
			if strings.EqualFold(k.ID, id) || k.IP == s.IP {
				isKnown = true
			}
		}
		if tried && (!isKnown || time.Since(last) < 6*time.Second) {
			a.mu.Unlock()
			continue
		}
		a.tried[s.Addr()] = time.Now()
		a.mu.Unlock()
		a.connect(s.Addr(), true)
	}

	// Fallback: last known address (if mDNS is blocked by the router/firewall).
	for _, k := range known {
		if onlineIDs[strings.ToLower(k.ID)] || k.IP == "" || k.Port == 0 {
			continue
		}
		addr := net.JoinHostPort(k.IP, strconv.Itoa(k.Port))
		a.mu.Lock()
		last, tried := a.tried[addr]
		if tried && time.Since(last) < 15*time.Second {
			a.mu.Unlock()
			continue
		}
		a.tried[addr] = time.Now()
		a.mu.Unlock()
		a.connect(addr, true)
	}
}

func (a *App) connect(addr string, quiet bool) (bool, string) {
	out, _ := a.adb(8*time.Second, "connect", addr)
	out = strings.TrimSpace(out)
	ok := strings.Contains(out, "connected to") && !strings.Contains(out, "failed") && !strings.Contains(out, "unable")
	if ok || !quiet {
		a.logf("adb connect %s → %s", addr, out)
	}
	if ok {
		// Wait until the device is "device" (auth may take a moment).
		for i := 0; i < 10; i++ {
			if list, err := a.adbDevices(); err == nil {
				for _, d := range list {
					if d.Serial == addr && d.State == "device" {
						a.refreshDevices()
						return true, out
					}
				}
			}
			time.Sleep(500 * time.Millisecond)
		}
		a.refreshDevices()
	}
	return ok, out
}

func (a *App) watcher() {
	a.startServer()
	a.logf("Папка scrcpy: %s", a.toolDir)
	lastScan := time.Time{}
	for {
		a.refreshDevices()
		a.mu.Lock()
		needScan := false
		onl := map[string]bool{}
		for _, d := range a.devices {
			if d.State == "device" {
				onl[strings.ToLower(d.ID)] = true
			}
		}
		for _, k := range a.cfg.Known {
			if !onl[strings.ToLower(k.ID)] {
				needScan = true
			}
		}
		if len(a.cfg.Known) == 0 {
			needScan = true
		}
		pairing := a.pair.Active
		a.mu.Unlock()
		if needScan && !pairing && time.Since(lastScan) > 4*time.Second {
			lastScan = time.Now()
			a.reconnectKnown(a.discover(1500 * time.Millisecond))
		}
		select {
		case <-a.wake:
		case <-time.After(3 * time.Second):
		}
	}
}

// ---------------------------------------------------------------- scrcpy

func splitArgs(s string) []string {
	var res []string
	var cur strings.Builder
	inQ := false
	has := false
	for _, r := range s {
		switch {
		case r == '"':
			inQ = !inQ
			has = true
		case (r == ' ' || r == '\t' || r == '\n') && !inQ:
			if has {
				res = append(res, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	if has {
		res = append(res, cur.String())
	}
	return res
}

var reFps = regexp.MustCompile(`^(?:INFO:\s*)?(\d+(?:\.\d+)?) fps\b`)

// fpsOf: frames per second actually shown in the phone window (-1 = unknown yet).
func (a *App) fpsOf(id string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.sessions[id]; s != nil && time.Since(s.fpsAt) < 4*time.Second {
		return s.fps
	}
	return -1
}

func (s Settings) keyboardArgs() []string {
	switch s.KeyboardMode {
	case "uhid":
		return []string{"--keyboard=uhid"}
	case "keys":
		return []string{"--keyboard=sdk"}
	default:
		// Cyrillic (and any other script) typed on the PC arrives as the same letters.
		return []string{"--keyboard=sdk", "--prefer-text"}
	}
}

func (s Settings) args() []string {
	r := []string{"--print-fps"}
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
	r = append(r, s.keyboardArgs()...)
	if s.StayAwake {
		r = append(r, "--stay-awake")
	}
	if s.TurnScreenOff {
		r = append(r, "--turn-screen-off")
	}
	if !s.Audio {
		r = append(r, "--no-audio")
	} else if s.AudioDup {
		// Sound on both PC and phone (Android 13+).
		r = append(r, "--audio-source=playback", "--audio-dup")
	}
	if s.Fullscreen {
		r = append(r, "--fullscreen")
	}
	if s.AlwaysOnTop {
		r = append(r, "--always-on-top")
	}
	if s.PowerOffOnClose {
		r = append(r, "--power-off-on-close")
	}
	return append(r, splitArgs(s.ExtraArgs)...)
}

type session struct {
	cmd          *exec.Cmd
	record       string // output file while recording
	since        int64
	fullscreen   bool
	screenOff    bool // phone screen state as toggled from the toolbar
	shown        bool // scrcpy window is on screen
	fps          int
	fpsAt        time.Time
	audioSrc     string
	dup          bool
	audioHandled bool
	camera       string
	app          string
	appLabel     string
	done         chan struct{}
}

// Place: target client rect as fractions (0..1) of the monitor the app window is on.
type Place struct {
	X, Y, W, H float64
}

type launchOpts struct {
	fullscreen  *bool
	record      bool
	camera      string  // "", "back", "front"
	screenOff   *bool   // override "turn screen off" for this launch
	audioSource *string // override the audio capture source (fallback)
	place       *Place  // put the window exactly here (fractions of the UI's monitor)
	revealAt    time.Time
	app         string // package for an app window
	appLabel    string
}

const camSuffix = "|cam"

func (a *App) launch(serial string, fullscreen *bool) error {
	return a.launchOpts(serial, launchOpts{fullscreen: fullscreen})
}

func (a *App) findDeviceLocked(serial string) *DeviceView {
	for i := range a.devices {
		if a.devices[i].Serial == serial || a.devices[i].ID == serial {
			return &a.devices[i]
		}
	}
	return nil
}

func (a *App) syncRunningLocked() {
	for i := range a.devices {
		s := a.sessions[a.devices[i].ID]
		a.devices[i].Camera = ""
		if c := a.sessions[a.devices[i].ID+camSuffix]; c != nil {
			a.devices[i].Camera = c.camera
		}
		a.devices[i].Apps = a.appRunsLocked(a.devices[i].ID)
		a.devices[i].Running = s != nil
		a.devices[i].Shown = s != nil && s.shown
		_, a.devices[i].Torch = a.sessions[a.devices[i].ID+torchSuffix]
		a.devices[i].DND = a.dnd[a.devices[i].ID]
		a.devices[i].Fps = 0
		if s != nil && time.Since(s.fpsAt) < 4*time.Second {
			a.devices[i].Fps = s.fps
		}
		a.devices[i].AudioNote = ""
		if s != nil {
			a.devices[i].AudioNote = a.audioNote[a.devices[i].ID]
		}
		a.devices[i].Recording = s != nil && s.record != ""
		a.devices[i].RecordSince = 0
		if a.devices[i].Recording {
			a.devices[i].RecordSince = s.since
		}
	}
}

// cameraArgs: phone camera in a window (Android 12+). Screen-only options are left out.
func (s Settings) cameraArgs(facing string) []string {
	r := []string{"--video-source=camera", "--camera-facing=" + facing}
	if s.MaxSize > 0 {
		r = append(r, "--max-size="+strconv.Itoa(s.MaxSize))
	}
	if s.BitRate > 0 {
		r = append(r, "--video-bit-rate="+strconv.Itoa(s.BitRate)+"M")
	}
	if s.VideoCodec != "" {
		r = append(r, "--video-codec="+s.VideoCodec)
	}
	if s.CameraMic {
		r = append(r, "--audio-source=mic")
	} else {
		r = append(r, "--no-audio")
	}
	if s.AlwaysOnTop {
		r = append(r, "--always-on-top")
	}
	return r
}

// netArgs: jitter buffers over Wi-Fi only (over USB they would just add delay).
func (s Settings) netArgs(wifi bool) []string {
	if !wifi {
		return nil
	}
	var r []string
	if s.VideoBuffer > 0 {
		r = append(r, "--video-buffer="+strconv.Itoa(s.VideoBuffer))
	}
	if s.AudioBuffer > 0 && s.Audio {
		r = append(r, "--audio-buffer="+strconv.Itoa(s.AudioBuffer))
	}
	return r
}

func (a *App) launchOpts(serial string, o launchOpts) error {
	placeArgs := placementArgs(o.place) // before taking the lock (talks to the UI thread)
	// Profiles with H.265 fall back to H.264 if the phone has no hardware HEVC encoder.
	a.mu.Lock()
	wantHEVC := a.cfg.Settings.VideoCodec == "h265"
	a.mu.Unlock()
	hevcOK := true
	if wantHEVC {
		var err error
		if hevcOK, _, err = a.ensureEncoders(serial); err != nil {
			hevcOK = false
		}
	}
	a.mu.Lock()
	dev := a.findDeviceLocked(serial)
	if dev == nil || dev.State != "device" {
		a.mu.Unlock()
		return fmt.Errorf("телефон не найден")
	}
	d := *dev
	key := d.ID
	if o.camera != "" {
		key += camSuffix
	}
	if o.app != "" {
		key += appKeySep + o.app
	}
	if _, running := a.sessions[key]; running {
		a.mu.Unlock()
		return fmt.Errorf("уже запущено")
	}
	st := a.cfg.Settings
	if o.fullscreen != nil {
		st.Fullscreen = *o.fullscreen
	}
	if o.screenOff != nil {
		st.TurnScreenOff = *o.screenOff
	}
	if wantHEVC && !hevcOK {
		st.VideoCodec = ""
	}
	wifi := isWifiSerial(d.Serial)
	var args []string
	audioSrcUsed, audioDup := "", false
	if o.app != "" {
		_, screenOpen := a.sessions[d.ID]
		args = append([]string{"-s", d.Serial, "--window-title=" + o.appLabel}, st.appArgs(o.app, st.Audio && !screenOpen)...)
		args = append(args, st.netArgs(wifi)...)
	} else if o.camera != "" {
		args = append([]string{"-s", d.Serial, "--window-title=" + d.Name + " — камера"}, st.cameraArgs(o.camera)...)
	} else {
		args = append([]string{"-s", d.Serial, "--window-title=" + d.Name}, st.args()...)
		args = append(args, st.netArgs(wifi)...)
		args = append(args, placeArgs...)
		if st.Audio {
			// Use the capture source that is known to work on this phone.
			src := a.cfg.AudioSrc[d.ID]
			if o.audioSource != nil {
				src = *o.audioSource
			}
			if src != "" || o.audioSource != nil {
				args = applyAudioSource(args, src)
				audioSrcUsed = src
			} else {
				audioDup = st.AudioDup
			}
		}
	}
	sess := &session{fullscreen: st.Fullscreen, since: time.Now().Unix(), done: make(chan struct{}), camera: o.camera, app: o.app, appLabel: o.appLabel,
		screenOff: st.TurnScreenOff, audioSrc: audioSrcUsed, dup: audioDup}
	if a.audioNote != nil && o.camera == "" && o.app == "" {
		delete(a.audioNote, d.ID)
	}
	if o.record {
		dir := a.videoDirLocked()
		if err := os.MkdirAll(dir, 0755); err != nil {
			a.mu.Unlock()
			return fmt.Errorf("не удалось создать папку для видео: %v", err)
		}
		sess.record = uniquePath(filepath.Join(dir, "Запись "+time.Now().Format("2006-01-02 15-04-05")+".mp4"))
		args = append(args, "--record="+sess.record)
	}
	cmd := exec.Command(a.scrcpy, args...)
	cmd.Dir = a.toolDir
	cmd.Env = append(os.Environ(), "ADB="+a.adbPath)
	hideWindow(cmd)
	pr, pw, _ := os.Pipe()
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		a.mu.Unlock()
		pw.Close()
		pr.Close()
		a.logf("Не удалось запустить scrcpy: %v", err)
		return err
	}
	pw.Close()
	sess.cmd = cmd
	a.sessions[key] = sess
	if o.camera == "" && o.app == "" && st.Toolbar {
		overlayAttach(a, key, d.Serial, cmd.Process.Pid)
	}
	go func(pid int) {
		// Wait for scrcpy's window, bring it to the front, and tell the UI it's up
		// (the "dive into the TV" animation ends when this happens).
		if revealWindow(pid, o.place, o.revealAt, 15*time.Second) {
			a.mu.Lock()
			if s := a.sessions[key]; s == sess {
				s.shown = true
				a.syncRunningLocked()
			}
			a.mu.Unlock()
		}
	}(cmd.Process.Pid)
	a.syncRunningLocked()
	a.mu.Unlock()
	a.logf("▶ scrcpy %s", strings.Join(args, " "))
	if sess.record != "" {
		a.logf("● Запись: %s", sess.record)
	}

	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			if t := strings.TrimSpace(sc.Text()); t != "" {
				if m := reFps.FindStringSubmatch(t); m != nil {
					// "INFO: 58 fps" once a second — keep it out of the log
					f, _ := strconv.ParseFloat(m[1], 64)
					a.mu.Lock()
					sess.fps, sess.fpsAt = int(f+.5), time.Now()
					a.mu.Unlock()
					continue
				}
				a.logf("   %s", t)
				if st.Audio {
					a.audioLine(key, d.ID, d.Serial, t, sess)
				}
			}
		}
		pr.Close()
	}()
	go func() {
		err := cmd.Wait()
		a.mu.Lock()
		delete(a.sessions, key)
		overlayDetach(key)
		a.syncRunningLocked()
		a.mu.Unlock()
		close(sess.done)
		if err != nil {
			a.logf("■ scrcpy (%s) завершился: %v", d.Name, err)
		} else {
			a.logf("■ scrcpy (%s) закрыт", d.Name)
		}
		if sess.record != "" {
			if fi, e := os.Stat(sess.record); e == nil && fi.Size() > 0 {
				a.event("video", sess.record, "Видео сохранено")
				a.logf("✔ Видео сохранено: %s", sess.record)
			} else {
				os.Remove(sess.record)
				a.event("error", "", "Запись не сохранилась — смотрите журнал")
			}
		}
	}()
	return nil
}

// stopWait closes the scrcpy window gracefully (so the video file is finalized),
// and kills it only if it doesn't quit in time.
func (a *App) stopWait(id string) {
	a.mu.Lock()
	s := a.sessions[id]
	a.mu.Unlock()
	if s == nil || s.cmd.Process == nil {
		return
	}
	gracefulStop(s.cmd)
	select {
	case <-s.done:
	case <-time.After(8 * time.Second):
		s.cmd.Process.Kill()
		<-s.done
	}
}

func (a *App) stop(id string) { a.stopWait(id) }

type UIRequest struct {
	Seq    int    `json:"seq"`
	Kind   string `json:"kind"`
	Serial string `json:"serial"`
}

func (a *App) requestUI(kind, serial string) {
	a.mu.Lock()
	a.uiReq = UIRequest{a.uiReq.Seq + 1, kind, serial}
	a.mu.Unlock()
}

func (a *App) appRunsLocked(id string) []AppRun {
	runs := []AppRun{}
	for k, s := range a.sessions {
		if s.app != "" && strings.HasPrefix(k, id+appKeySep) {
			runs = append(runs, AppRun{Pkg: s.app, Label: s.appLabel, Key: k})
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Label < runs[j].Label })
	return runs
}

// toolbarInfo: live state for the toolbar next to a phone's scrcpy window.
func (a *App) toolbarInfo(id string) (recording bool, since int64, camera string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.sessions[id]; s != nil && s.record != "" {
		recording, since = true, s.since
	}
	if c := a.sessions[id+camSuffix]; c != nil {
		camera = c.camera
	}
	return
}

// screenState: is the phone's own screen turned off (toolbar toggle), and where sound goes.
func (a *App) screenState(id string) (screenOff bool, audio string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.sessions[id]; s != nil {
		screenOff = s.screenOff
	}
	switch {
	case !a.cfg.Settings.Audio:
		audio = "phone"
	case a.cfg.Settings.AudioDup:
		audio = "both"
	default:
		audio = "pc"
	}
	return
}

func (a *App) setScreenOffFlag(id string, off bool) {
	a.mu.Lock()
	if s := a.sessions[id]; s != nil {
		s.screenOff = off
	}
	a.mu.Unlock()
}

// setAudioMode: "pc" | "phone" | "both". Restarts the open phone screen to apply.
func (a *App) setAudioMode(serial, mode string) error {
	a.mu.Lock()
	d := a.findDeviceLocked(serial)
	if d == nil {
		a.mu.Unlock()
		return fmt.Errorf("телефон не найден")
	}
	id, ser := d.ID, d.Serial
	s := a.sessions[id]
	if s != nil && s.record != "" {
		a.mu.Unlock()
		return fmt.Errorf("сначала остановите запись")
	}
	st := &a.cfg.Settings
	st.Audio = mode != "phone"
	st.AudioDup = mode == "both"
	a.saveLocked()
	var fs, off bool
	if s != nil {
		fs, off = s.fullscreen, s.screenOff
	}
	a.mu.Unlock()
	a.logf("Звук: %s", map[string]string{"pc": "на ПК", "phone": "на телефоне", "both": "на ПК и телефоне"}[mode])
	if s == nil {
		return nil
	}
	a.stopWait(id)
	return a.launchOpts(ser, launchOpts{fullscreen: &fs, screenOff: &off})
}

func (a *App) cameraOn(serial, facing string) error {
	a.mu.Lock()
	var id string
	if d := a.findDeviceLocked(serial); d != nil {
		id = d.ID
	}
	a.mu.Unlock()
	if id != "" {
		a.stopWait(id + camSuffix) // switching front/back = restart
	}
	err := a.launchOpts(serial, launchOpts{camera: facing})
	if err != nil {
		a.event("error", "", "Камера: "+err.Error())
	}
	return err
}

// applyToolbar shows/hides toolbars on already open windows after a settings change.
func (a *App) applyToolbar() {
	a.mu.Lock()
	on := a.cfg.Settings.Toolbar
	type item struct {
		id, serial string
		pid        int
	}
	var list []item
	for id, s := range a.sessions {
		if s.camera == "" && s.app == "" && s.cmd.Process != nil {
			serial := id
			if d := a.findDeviceLocked(id); d != nil {
				serial = d.Serial
			}
			list = append(list, item{id, serial, s.cmd.Process.Pid})
		}
	}
	a.mu.Unlock()
	if !on {
		overlayDetachAll()
		return
	}
	for _, it := range list {
		overlayAttach(a, it.id, it.serial, it.pid)
	}
}

func (a *App) setRecording(serial string, on bool) error {
	a.mu.Lock()
	dev := a.findDeviceLocked(serial)
	if dev == nil || dev.State != "device" {
		a.mu.Unlock()
		return fmt.Errorf("телефон не найден")
	}
	d := *dev
	s := a.sessions[d.ID]
	fs := a.cfg.Settings.Fullscreen
	if s != nil {
		fs = s.fullscreen
	}
	a.mu.Unlock()
	if on {
		if s != nil && s.record != "" {
			return nil
		}
		if s != nil {
			a.stopWait(d.ID) // restart the mirror with recording on
		}
		err := a.launchOpts(d.Serial, launchOpts{fullscreen: &fs, record: true})
		if err == nil {
			go a.sfx("rec_start")
		}
		return err
	}
	if s == nil || s.record == "" {
		return nil
	}
	a.stopWait(d.ID)
	go a.sfx("rec_stop")
	return a.launchOpts(d.Serial, launchOpts{fullscreen: &fs}) // keep mirroring
}

func uniquePath(p string) string {
	if _, err := os.Stat(p); err != nil {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 2; ; i++ {
		q := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(q); err != nil {
			return q
		}
	}
}

// ---------------------------------------------------------------- screenshots

func (a *App) shotDirLocked() string {
	if d := strings.TrimSpace(a.cfg.Settings.ShotDir); d != "" {
		return d
	}
	return filepath.Join(knownFolder("pictures"), "Телефон на ПК")
}

func (a *App) videoDirLocked() string {
	if d := strings.TrimSpace(a.cfg.Settings.VideoDir); d != "" {
		return d
	}
	return filepath.Join(knownFolder("videos"), "Телефон на ПК")
}

func (a *App) screenshot(serial string) (string, error) {
	a.mu.Lock()
	dev := a.findDeviceLocked(serial)
	if dev == nil || dev.State != "device" {
		a.mu.Unlock()
		return "", fmt.Errorf("телефон не найден")
	}
	d := *dev
	dir := a.shotDirLocked()
	a.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.adbPath, "-s", d.Serial, "exec-out", "screencap", "-p")
	cmd.Dir = a.toolDir
	hideWindow(cmd)
	cmd.WaitDelay = 2 * time.Second
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	png, err := cmd.Output()
	if len(png) < 8 || string(png[1:4]) != "PNG" {
		msg := strings.TrimSpace(errBuf.String())
		if msg == "" && err != nil {
			msg = err.Error()
		}
		if msg == "" {
			msg = "телефон не отдал картинку (экран выключен или защищён?)"
		}
		return "", fmt.Errorf("скриншот не получился: %s", msg)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("не удалось создать папку: %v", err)
	}
	p := uniquePath(filepath.Join(dir, "Скриншот "+time.Now().Format("2006-01-02 15-04-05")+".png"))
	if err := os.WriteFile(p, png, 0644); err != nil {
		return "", err
	}
	a.logf("📷 Скриншот: %s", p)
	go a.sfx("shutter")
	return p, nil
}

type Event struct {
	ID   int    `json:"id"`
	Kind string `json:"kind"`
	Path string `json:"path"`
	Text string `json:"text"`
}

func (a *App) event(kind, path, text string) {
	a.mu.Lock()
	a.evSeq++
	a.events = append(a.events, Event{a.evSeq, kind, path, text})
	if len(a.events) > 20 {
		a.events = a.events[len(a.events)-20:]
	}
	a.mu.Unlock()
}

// allowedPath: only open files/folders inside our own output folders.
func (a *App) allowedPath(p string) bool {
	a.mu.Lock()
	dirs := []string{a.shotDirLocked(), a.videoDirLocked(), a.photoDirLocked()}
	a.mu.Unlock()
	p = filepath.Clean(p)
	for _, d := range dirs {
		d = filepath.Clean(d)
		if strings.EqualFold(p, d) || strings.HasPrefix(strings.ToLower(p), strings.ToLower(d)+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- pairing

const pairStageTimeout = 3 * time.Minute

func (a *App) startQRPairing() PairState {
	name := "scrcpy-" + randString(6, "abcdefghijkmnpqrstuvwxyz23456789")
	pass := randString(10, "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789")
	text := "WIFI:T:ADB;S:" + name + ";P:" + pass + ";;"
	q, _ := EncodeQR(text)
	a.mu.Lock()
	a.pair.gen++
	gen := a.pair.gen
	a.pair = PairState{Active: true, Stage: "waiting", Message: "Отсканируйте QR-код телефоном", QR: q.SVG(4), Name: name, Pass: pass, gen: gen}
	st := a.pair
	a.mu.Unlock()
	a.logf("Сопряжение по QR: ждём телефон (%s)", name)
	go a.qrPairLoop(gen, name, pass)
	return st
}

func (a *App) pairAlive(gen int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pair.Active && a.pair.gen == gen
}

func (a *App) setPair(gen int, stage, msg string) {
	a.mu.Lock()
	if a.pair.gen == gen {
		a.pair.Stage = stage
		a.pair.Message = msg
		if stage == "done" || stage == "error" || stage == "timeout" {
			a.pair.Active = false
		}
	}
	a.mu.Unlock()
}

func (a *App) cancelPair() {
	a.mu.Lock()
	a.pair.Active = false
	a.pair.gen++
	a.pair.Stage = "cancelled"
	a.mu.Unlock()
}

func (a *App) qrPairLoop(gen int, name, pass string) {
	deadline := time.Now().Add(pairStageTimeout)
	for time.Now().Before(deadline) {
		if !a.pairAlive(gen) {
			return
		}
		for _, s := range a.discover(1200 * time.Millisecond) {
			if s.Service == svcPairing && strings.EqualFold(s.Instance, name) {
				a.setPair(gen, "pairing", "Телефон найден ("+s.IP+"), выполняю сопряжение…")
				a.finishPairing(gen, s.Addr(), pass)
				return
			}
		}
	}
	a.setPair(gen, "timeout", "Телефон не появился за 3 минуты. Проверьте, что ПК и телефон в одной Wi-Fi сети, и попробуйте ещё раз.")
}

// finishPairing runs `adb pair` and then finds the (different!) connect port.
func (a *App) finishPairing(gen int, pairAddr, secret string) {
	out, _ := a.adb(25*time.Second, "pair", pairAddr, secret)
	out = strings.TrimSpace(out)
	a.logf("adb pair %s → %s", pairAddr, out)
	if !strings.Contains(strings.ToLower(out), "successfully paired") {
		a.setPair(gen, "error", "Сопряжение не удалось: "+out)
		return
	}
	ip, _ := splitHostPort(pairAddr)
	serial := ""
	if m := reGuid.FindStringSubmatch(out); m != nil {
		serial = serialFromInstance("adb-" + m[1])
	}
	a.setPair(gen, "connecting", "Сопряжено! Подключаюсь…")

	a.mu.Lock()
	launchAfter := a.cfg.Settings.LaunchAfterPair
	autoLaunch := a.cfg.Settings.AutoLaunch
	a.mu.Unlock()

	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		// adb may auto-connect by itself via mDNS.
		a.refreshDevices()
		if d := a.findOnline(ip, serial); d != nil {
			a.pairSuccess(gen, *d, launchAfter && !autoLaunch)
			return
		}
		for _, s := range a.discover(1200 * time.Millisecond) {
			if s.Service != svcConnect {
				continue
			}
			if s.IP == ip || (serial != "" && strings.EqualFold(serialFromInstance(s.Instance), serial)) {
				if ok, _ := a.connect(s.Addr(), false); ok {
					if d := a.findOnline(ip, serial); d != nil {
						a.pairSuccess(gen, *d, launchAfter && !autoLaunch)
						return
					}
				}
			}
		}
	}
	a.setPair(gen, "error", "Сопряжение прошло, но подключиться не удалось. Убедитесь, что «Отладка по Wi-Fi» на телефоне включена.")
}

func (a *App) findOnline(ip, serial string) *DeviceView {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.devices {
		d := a.devices[i]
		if d.State != "device" {
			continue
		}
		h, _ := splitHostPort(d.Serial)
		if (ip != "" && h == ip) || (serial != "" && (strings.EqualFold(d.ID, serial) || strings.Contains(strings.ToLower(d.Serial), strings.ToLower(serial)))) {
			return &d
		}
	}
	return nil
}

func (a *App) pairSuccess(gen int, d DeviceView, launch bool) {
	a.setPair(gen, "done", "Готово! "+d.Name+" подключён.")
	a.logf("✔ %s подключён по Wi-Fi", d.Name)
	if launch {
		a.launch(d.Serial, nil)
	}
}

// Manual pairing with a 6-digit code ("Pair device with pairing code").
func (a *App) pairWithCode(addr, code string) {
	a.mu.Lock()
	a.pair.gen++
	gen := a.pair.gen
	a.pair = PairState{Active: true, Stage: "pairing", Message: "Сопряжение с " + addr + "…", gen: gen}
	a.mu.Unlock()
	go a.finishPairing(gen, addr, code)
}

// USB → Wi-Fi for phones without "Wireless debugging" (Android ≤ 10) or as a shortcut.
func (a *App) usbToWifi(serial string) (string, error) {
	out, _ := a.adb(6*time.Second, "-s", serial, "shell", "ip -f inet addr show wlan0")
	m := reInet.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("не удалось узнать IP телефона в Wi-Fi (Wi-Fi включён?)")
	}
	ip := m[1]
	out, err := a.adb(10*time.Second, "-s", serial, "tcpip", "5555")
	a.logf("adb tcpip 5555 → %s", strings.TrimSpace(out))
	if err != nil {
		return "", fmt.Errorf("adb tcpip: %s", strings.TrimSpace(out))
	}
	time.Sleep(2 * time.Second)
	addr := ip + ":5555"
	if ok, msg := a.connect(addr, false); !ok {
		return "", fmt.Errorf("%s", msg)
	}
	return addr, nil
}
