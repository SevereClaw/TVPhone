package main

// Profiles: named sets of quality settings ("Лента / видео", "Игры", ...) switched in one click.

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Profile struct {
	Name       string `json:"name"`
	Hint       string `json:"hint,omitempty"`
	MaxSize    int    `json:"maxSize"`
	BitRate    int    `json:"bitRate"`
	MaxFps     int    `json:"maxFps"`
	VideoCodec string `json:"videoCodec"`
	Audio      bool   `json:"audio"`
	VideoBuf   int    `json:"videoBuffer"`
	AudioBuf   int    `json:"audioBuffer"`
	Builtin    bool   `json:"builtin,omitempty"`
}

// Buffers (ms) are applied only over Wi-Fi: they smooth out network jitter.
var builtinProfiles = []Profile{
	{Name: "Лента / видео", Hint: "1600 · 12 Мбит · H.265 · сглаживание", MaxSize: 1600, BitRate: 12, MaxFps: 60,
		VideoCodec: "h265", Audio: true, VideoBuf: 50, AudioBuf: 120, Builtin: true},
	{Name: "Игры", Hint: "1280 · 10 Мбит · мин. задержка", MaxSize: 1280, BitRate: 10, MaxFps: 0,
		VideoCodec: "h264", Audio: true, Builtin: true},
	{Name: "Чётко", Hint: "оригинал · 20 Мбит · для кабеля", MaxSize: 0, BitRate: 20, MaxFps: 60,
		VideoCodec: "h265", Audio: true, Builtin: true},
	{Name: "Плавно", Hint: "1280 · 4 Мбит · 60 к/с", MaxSize: 1280, BitRate: 4, MaxFps: 60, Audio: true,
		VideoBuf: 30, AudioBuf: 100, Builtin: true},
	{Name: "Экономно", Hint: "1024 · 2 Мбит · 30 к/с", MaxSize: 1024, BitRate: 2, MaxFps: 30, Audio: true,
		VideoBuf: 60, AudioBuf: 140, Builtin: true},
}

func (p Profile) matches(s Settings) bool {
	return p.MaxSize == s.MaxSize && p.BitRate == s.BitRate && p.MaxFps == s.MaxFps &&
		p.VideoCodec == s.VideoCodec && p.Audio == s.Audio && p.VideoBuf == s.VideoBuffer && p.AudioBuf == s.AudioBuffer
}

func (s *Settings) setFrom(p Profile) {
	s.MaxSize, s.BitRate, s.MaxFps, s.VideoCodec, s.Audio = p.MaxSize, p.BitRate, p.MaxFps, p.VideoCodec, p.Audio
	s.VideoBuffer, s.AudioBuffer = p.VideoBuf, p.AudioBuf
}

func (a *App) setProfileNoRestart(name string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range a.profilesLocked() {
		if p.Name == name {
			a.cfg.Settings.setFrom(p)
			a.saveLocked()
			return nil
		}
	}
	return fmt.Errorf("профиль не найден")
}

func (a *App) profilesLocked() []Profile {
	return append(append([]Profile{}, builtinProfiles...), a.cfg.Profiles...)
}

func (a *App) currentProfileLocked() string {
	for _, p := range a.profilesLocked() {
		if p.matches(a.cfg.Settings) {
			return p.Name
		}
	}
	return ""
}

// applyProfile switches settings and restarts open phone screens (unless recording).
func (a *App) applyProfile(name string) (restarted int, err error) {
	a.mu.Lock()
	var prof *Profile
	for _, p := range a.profilesLocked() {
		if p.Name == name {
			p := p
			prof = &p
		}
	}
	if prof == nil {
		a.mu.Unlock()
		return 0, fmt.Errorf("профиль не найден")
	}
	a.cfg.Settings.setFrom(*prof)
	a.saveLocked()
	type re struct {
		id, serial string
		fs         bool
	}
	var list []re
	for id, ss := range a.sessions {
		if ss.camera == "" && ss.app == "" && ss.record == "" {
			if d := a.findDeviceLocked(id); d != nil {
				list = append(list, re{id, d.Serial, ss.fullscreen})
			}
		}
	}
	a.mu.Unlock()
	a.logf("Профиль: %s", name)
	for _, r := range list {
		a.stopWait(r.id)
		fs := r.fs
		a.launch(r.serial, &fs)
	}
	return len(list), nil
}

func (a *App) saveProfile(name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 30 {
		return fmt.Errorf("введите название до 30 символов")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range builtinProfiles {
		if strings.EqualFold(p.Name, name) {
			return fmt.Errorf("такое название уже есть")
		}
	}
	s := a.cfg.Settings
	p := Profile{Name: name, MaxSize: s.MaxSize, BitRate: s.BitRate, MaxFps: s.MaxFps, VideoCodec: s.VideoCodec, Audio: s.Audio,
		VideoBuf: s.VideoBuffer, AudioBuf: s.AudioBuffer}
	size := "оригинал"
	if s.MaxSize > 0 {
		size = strconv.Itoa(s.MaxSize)
	}
	p.Hint = size + " · " + strconv.Itoa(s.BitRate) + " Мбит"
	for i, x := range a.cfg.Profiles {
		if strings.EqualFold(x.Name, name) {
			a.cfg.Profiles[i] = p
			a.saveLocked()
			return nil
		}
	}
	if len(a.cfg.Profiles) >= 8 {
		return fmt.Errorf("можно сохранить до 8 своих профилей")
	}
	a.cfg.Profiles = append(a.cfg.Profiles, p)
	a.saveLocked()
	return nil
}

func (a *App) deleteProfile(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var keep []Profile
	for _, p := range a.cfg.Profiles {
		if p.Name != name {
			keep = append(keep, p)
		}
	}
	a.cfg.Profiles = keep
	a.saveLocked()
}

// ---------------------------------------------------------------- battery

var (
	reBatLevel = regexp.MustCompile(`(?m)^\s*level:\s*(\d+)`)
	reBatStat  = regexp.MustCompile(`(?m)^\s*status:\s*(\d+)`)
	reBatPower = regexp.MustCompile(`(?m)^\s*(AC|USB|Wireless) powered:\s*true`)
)

type battery struct {
	Level    int
	Charging bool
	At       time.Time
	warned   bool
}

func parseBattery(out string) (int, bool, bool) {
	m := reBatLevel.FindStringSubmatch(out)
	if m == nil {
		return 0, false, false
	}
	lvl, _ := strconv.Atoi(m[1])
	charging := reBatPower.MatchString(out)
	if st := reBatStat.FindStringSubmatch(out); st != nil && (st[1] == "2" || st[1] == "5") {
		charging = true
	}
	return lvl, charging, true
}

// batteryLoop polls the battery of online phones once a minute and warns at ≤15%
// while the phone screen is open on the PC.
func (a *App) batteryLoop() {
	for {
		a.mu.Lock()
		var devs []DeviceView
		for _, d := range a.devices {
			if d.State == "device" {
				devs = append(devs, d)
			}
		}
		a.mu.Unlock()
		for _, d := range devs {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			out, err := a.adbCtx(ctx, "-s", d.Serial, "shell", "dumpsys battery")
			cancel()
			if err != nil {
				continue
			}
			a.readDND(d.Serial, d.ID)
			lvl, ch, ok := parseBattery(out)
			if !ok {
				continue
			}
			a.mu.Lock()
			b := a.battery[d.ID]
			if b == nil {
				b = &battery{}
				a.battery[d.ID] = b
			}
			b.Level, b.Charging, b.At = lvl, ch, time.Now()
			_, screenOpen := a.sessions[d.ID]
			warn := false
			if lvl <= 15 && !ch && screenOpen && !b.warned {
				b.warned, warn = true, true
			}
			if lvl > 20 || ch {
				b.warned = false
			}
			for i := range a.devices {
				if a.devices[i].ID == d.ID {
					a.devices[i].Battery, a.devices[i].Charging = lvl, ch
				}
			}
			a.mu.Unlock()
			if warn {
				msg := fmt.Sprintf("Заряд %s — %d%%. Поставьте телефон на зарядку.", d.Name, lvl)
				a.event("error", "", msg)
				trayNotify("Телефон скоро разрядится", msg)
				a.logf("🔋 %s", msg)
			}
		}
		select {
		case <-a.batWake:
		case <-time.After(batInterval()):
		}
	}
}

func batInterval() time.Duration {
	if v, err := strconv.Atoi(os.Getenv("LAUNCHER_BAT_INTERVAL")); err == nil && v > 0 {
		return time.Duration(v) * time.Second
	}
	return 60 * time.Second
}
