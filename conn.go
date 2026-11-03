package main

// Connection check (Wi-Fi band, link speed, latency, real throughput), auto-profile,
// and phone encoder detection (hardware H.265).

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type ConnResult struct {
	Transport  string   `json:"transport"` // "usb" | "wifi"
	SSID       string   `json:"ssid,omitempty"`
	FreqMHz    int      `json:"freqMhz,omitempty"`
	LinkMbps   int      `json:"linkMbps,omitempty"`
	RSSI       int      `json:"rssi,omitempty"`
	RttMs      float64  `json:"rttMs,omitempty"`
	RttMaxMs   float64  `json:"rttMaxMs,omitempty"`
	Lost       int      `json:"lost,omitempty"`
	ThruMbps   float64  `json:"thruMbps,omitempty"`
	Quality    string   `json:"quality"` // good | ok | weak
	Profile    string   `json:"profile"`
	Verdict    string   `json:"verdict"`
	GamesOK    bool     `json:"gamesOk"`
	Tips       []string `json:"tips"`
	CheckedAt  int64    `json:"checkedAt"`
	HEVC       string   `json:"hevc"` // "yes" | "no" | ""
	Encoder    string   `json:"encoder,omitempty"`
	SummaryTxt string   `json:"summary"`
}

var (
	reLink  = regexp.MustCompile(`(?i)(?:^|[ ,])Link speed:\s*(\d+)\s*Mbps`)
	reTx    = regexp.MustCompile(`(?i)Tx Link speed:\s*(\d+)`)
	reRx    = regexp.MustCompile(`(?i)Rx Link speed:\s*(\d+)`)
	reFreq  = regexp.MustCompile(`(?i)Frequency:\s*(\d+)\s*MHz`)
	reRSSI  = regexp.MustCompile(`(?i)RSSI:\s*(-?\d+)`)
	reSSID  = regexp.MustCompile(`SSID:\s*"?([^",]*?)"?,`)
	reEncHW = regexp.MustCompile(`--video-codec=(h264|h265|av1)\s+--video-encoder=(\S+)(.*)`)
)

func parseWifiInfo(out string, r *ConnResult) bool {
	atoi := func(re *regexp.Regexp) int {
		if m := re.FindStringSubmatch(out); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n
		}
		return 0
	}
	r.FreqMHz = atoi(reFreq)
	r.LinkMbps = atoi(reLink)
	if tx := atoi(reTx); tx > r.LinkMbps {
		r.LinkMbps = tx
	}
	if rx := atoi(reRx); rx > r.LinkMbps {
		r.LinkMbps = rx
	}
	r.RSSI = atoi(reRSSI)
	if m := reSSID.FindStringSubmatch(out); m != nil && m[1] != "<unknown ssid>" {
		r.SSID = m[1]
	}
	return r.FreqMHz > 0 || r.LinkMbps > 0
}

func (a *App) deviceAddr(d DeviceView) string {
	if h, p := splitHostPort(d.Serial); h != "" && p > 0 {
		return d.Serial
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if k := a.knownLocked(d.ID); k != nil && k.IP != "" && k.Port > 0 {
		return net.JoinHostPort(k.IP, strconv.Itoa(k.Port))
	}
	return ""
}

// measureRTT: TCP handshake time to the phone's wireless-debugging port = one network round trip.
func measureRTT(addr string, n int) (median, max float64, lost int) {
	var ms []float64
	fails := 0
	for i := 0; i < n; i++ {
		t := time.Now()
		c, err := net.DialTimeout("tcp", addr, 700*time.Millisecond)
		if err != nil {
			lost++
			if fails++; fails >= 2 && len(ms) == 0 {
				break // unreachable: don't hold up the connection
			}
			continue
		}
		ms = append(ms, float64(time.Since(t).Microseconds())/1000)
		c.Close()
		time.Sleep(60 * time.Millisecond)
	}
	if len(ms) == 0 {
		return 0, 0, lost
	}
	sort.Float64s(ms)
	return ms[len(ms)/2], ms[len(ms)-1], lost
}

// measureThroughput pushes 4 MB of random data to the phone and times it.
func (a *App) measureThroughput(serial string) float64 {
	dir, err := os.MkdirTemp("", "phone-speed-")
	if err != nil {
		return 0
	}
	defer os.RemoveAll(dir)
	f := filepath.Join(dir, "speed.bin")
	buf := make([]byte, 4<<20)
	rand.Read(buf) // incompressible, so adb compression can't cheat
	if os.WriteFile(f, buf, 0644) != nil {
		return 0
	}
	remote := "/data/local/tmp/.phone-on-pc-speed"
	t := time.Now()
	if _, err := a.adb(30*time.Second, "-s", serial, "push", f, remote); err != nil {
		return 0
	}
	sec := time.Since(t).Seconds()
	a.adb(5*time.Second, "-s", serial, "shell", "rm -f "+remote)
	if sec <= 0 {
		return 0
	}
	return float64(len(buf)*8) / 1e6 / sec
}

func (a *App) connTest(serial string, full bool) (*ConnResult, error) {
	a.mu.Lock()
	dv := a.findDeviceLocked(serial)
	if dv == nil || dv.State != "device" {
		a.mu.Unlock()
		return nil, fmt.Errorf("телефон не найден")
	}
	d := *dv
	a.mu.Unlock()

	r := &ConnResult{Transport: "wifi", CheckedAt: time.Now().Unix(), Tips: []string{}}
	if !isWifiSerial(d.Serial) {
		r.Transport = "usb"
	}
	if r.Transport == "wifi" {
		out, _ := a.adb(8*time.Second, "-s", d.Serial, "shell", "dumpsys wifi | grep -m1 -E 'mWifiInfo|WifiInfo:'")
		if !parseWifiInfo(out, r) {
			out, _ = a.adb(8*time.Second, "-s", d.Serial, "shell", "cmd wifi status")
			parseWifiInfo(out, r)
		}
		if addr := a.deviceAddr(d); addr != "" {
			n := 5
			if full {
				n = 10
			}
			r.RttMs, r.RttMaxMs, r.Lost = measureRTT(addr, n)
		}
	}
	if full {
		r.ThruMbps = a.measureThroughput(d.Serial)
		if hevc, enc, err := a.ensureEncoders(d.Serial); err == nil {
			r.HEVC, r.Encoder = map[bool]string{true: "yes", false: "no"}[hevc], enc
		}
	}
	judge(r)
	a.mu.Lock()
	a.conn[d.ID] = r
	a.mu.Unlock()
	a.logf("📶 Связь с %s: %s", d.Name, r.SummaryTxt)
	return r, nil
}

func judge(r *ConnResult) {
	if r.Transport == "usb" {
		r.Quality, r.Profile, r.GamesOK = "good", "Чётко", true
		r.Verdict = "Кабель USB — самая быстрая и стабильная связь."
		r.SummaryTxt = "кабель USB"
		if r.ThruMbps > 0 {
			r.SummaryTxt += fmt.Sprintf(", %.0f Мбит/с", r.ThruMbps)
		}
		return
	}
	jitter := r.RttMaxMs - r.RttMs
	thrOK := func(min float64) bool { return r.ThruMbps == 0 || r.ThruMbps >= min }
	band5 := r.FreqMHz >= 4900
	switch {
	case r.RttMs > 0 && r.RttMs <= 12 && jitter <= 40 && r.Lost == 0 && thrOK(35) && (band5 || r.LinkMbps >= 150):
		r.Quality, r.Profile = "good", "Лента / видео"
		r.Verdict = "Хорошая связь — можно смотреть в высоком качестве."
	case r.RttMs > 0 && r.RttMs <= 35 && jitter <= 120 && r.Lost <= 1 && thrOK(12):
		r.Quality, r.Profile = "ok", "Плавно"
		r.Verdict = "Нормальная связь — лучше средние настройки."
	default:
		r.Quality, r.Profile = "weak", "Экономно"
		r.Verdict = "Слабая связь — ставлю экономные настройки, чтобы не было рывков."
	}
	r.GamesOK = r.RttMs > 0 && r.RttMs <= 8 && jitter <= 25 && r.Lost == 0
	if r.FreqMHz > 0 && !band5 {
		r.Tips = append(r.Tips, "Телефон подключён к Wi-Fi 2,4 ГГц. Если у роутера есть сеть 5 ГГц — подключите телефон к ней: будет в разы быстрее.")
	}
	if r.RSSI != 0 && r.RSSI < -70 {
		r.Tips = append(r.Tips, "Слабый сигнал Wi-Fi — подойдите ближе к роутеру.")
	}
	if jitter > 40 || r.Lost > 0 {
		r.Tips = append(r.Tips, "Сеть нестабильна (задержка скачет). Помогает подключить ПК к роутеру кабелем.")
	}
	if !r.GamesOK {
		r.Tips = append(r.Tips, "Для игр лучше кабель USB — задержка будет минимальной.")
	}
	var parts []string
	if r.FreqMHz > 0 {
		parts = append(parts, map[bool]string{true: "5 ГГц", false: "2,4 ГГц"}[band5])
	}
	if r.LinkMbps > 0 {
		parts = append(parts, fmt.Sprintf("%d Мбит/с", r.LinkMbps))
	}
	if r.RttMs > 0 {
		if r.RttMs < 1 {
			parts = append(parts, "<1 мс")
		} else {
			parts = append(parts, fmt.Sprintf("%.0f мс", r.RttMs))
		}
	}
	r.SummaryTxt = strings.Join(parts, " · ")
	if r.SummaryTxt == "" {
		r.SummaryTxt = "Wi-Fi"
	}
}

// autoProfile picks a quality profile on connect. It only switches between the
// quality-tier profiles, so a manual choice like «Игры» or a custom one is kept.
func (a *App) autoProfile(serial string) {
	a.mu.Lock()
	on := a.cfg.Settings.AutoProfile
	cur := a.currentProfileLocked()
	a.mu.Unlock()
	if !on {
		return
	}
	tier := map[string]bool{"Лента / видео": true, "Плавно": true, "Экономно": true, "Чётко": true}
	if !tier[cur] {
		return
	}
	r, err := a.connTest(serial, false)
	if err != nil || r.Profile == cur {
		return
	}
	if err := a.setProfileNoRestart(r.Profile); err == nil {
		a.logf("Профиль «%s» подобран автоматически (%s)", r.Profile, r.SummaryTxt)
		a.event("info", "", "Профиль «"+r.Profile+"» подобран по связи: "+r.SummaryTxt)
	}
}

// ---------------------------------------------------------------- encoders

type encInfo struct {
	checked bool
	hevc    bool
	name    string
}

func parseEncoders(out string) (hevc bool, name string) {
	for _, line := range strings.Split(out, "\n") {
		m := reEncHW.FindStringSubmatch(line)
		if m == nil || m[1] != "h265" {
			continue
		}
		tail := strings.ToLower(m[3])
		if strings.Contains(tail, "(sw)") || strings.Contains(m[2], "c2.android.") || strings.Contains(strings.ToLower(m[2]), "omx.google.") {
			continue // software encoder
		}
		return true, m[2]
	}
	return false, ""
}

// ensureEncoders asks the phone (once) whether it has a hardware H.265 encoder.
func (a *App) ensureEncoders(serial string) (bool, string, error) {
	a.mu.Lock()
	d := a.findDeviceLocked(serial)
	if d == nil {
		a.mu.Unlock()
		return false, "", fmt.Errorf("телефон не найден")
	}
	id, ser := d.ID, d.Serial
	if e := a.enc[id]; e != nil && e.checked {
		a.mu.Unlock()
		return e.hevc, e.name, nil
	}
	if name, ok := a.cfg.HEVC[id]; ok { // remembered from an earlier run
		a.enc[id] = &encInfo{checked: true, hevc: name != "", name: name}
		a.mu.Unlock()
		return name != "", name, nil
	}
	a.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.scrcpy, "-s", ser, "--list-encoders")
	cmd.Dir = a.toolDir
	cmd.Env = append(os.Environ(), "ADB="+a.adbPath)
	hideWindow(cmd)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if !strings.Contains(string(out), "--video-codec=") {
		if err == nil {
			err = fmt.Errorf("нет списка кодировщиков")
		}
		return false, "", err
	}
	hevc, name := parseEncoders(string(out))
	a.mu.Lock()
	a.enc[id] = &encInfo{checked: true, hevc: hevc, name: name}
	if a.cfg.HEVC == nil {
		a.cfg.HEVC = map[string]string{}
	}
	a.cfg.HEVC[id] = name
	a.saveLocked()
	a.mu.Unlock()
	if hevc {
		a.logf("Телефон умеет H.265 аппаратно (%s)", name)
	} else {
		a.logf("Аппаратного H.265 на телефоне нет — профили с H.265 будут использовать H.264")
	}
	return hevc, name, nil
}
