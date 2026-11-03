package main

import (
	"encoding/binary"
	"math"
	"os"
	"strings"
	"testing"
)

func name(labels ...string) []byte {
	var b []byte
	for _, l := range labels {
		b = append(b, byte(len(l)))
		b = append(b, l...)
	}
	return append(b, 0)
}

func rr(n []byte, typ uint16, rdata []byte) []byte {
	b := append([]byte{}, n...)
	h := make([]byte, 10)
	binary.BigEndian.PutUint16(h[0:], typ)
	binary.BigEndian.PutUint16(h[2:], 0x8001)
	binary.BigEndian.PutUint32(h[4:], 120)
	binary.BigEndian.PutUint16(h[8:], uint16(len(rdata)))
	return append(append(b, h...), rdata...)
}

func TestParseAndroidResponse(t *testing.T) {
	// Android-style answer: PTR (instance label + pointer to service), SRV + A additional records.
	inst := append([]byte{byte(len("adb-ABC123XYZ-q1w2e3"))}, "adb-ABC123XYZ-q1w2e3"...)
	var ans []byte
	var svcOff int
	msg := make([]byte, 12)
	binary.BigEndian.PutUint16(msg[6:], 1)
	binary.BigEndian.PutUint16(msg[10:], 2)
	svcOff = 12
	full := name("_adb-tls-connect", "_tcp", "local")
	instFull := append(append([]byte{}, inst...), 0xC0, byte(svcOff))
	ans = rr(full, 12, instFull)
	msg = append(msg, ans...)
	instOff := 12 + len(full) + 10
	srvData := []byte{0, 0, 0, 0, 0x9D, 0xD4} // port 40404
	srvData = append(srvData, name("Android-3", "local")...)
	msg = append(msg, rr([]byte{0xC0, byte(instOff)}, 33, srvData)...)
	msg = append(msg, rr(name("Android-3", "local"), 1, []byte{192, 168, 1, 50})...)

	got := collectServices([]packet{{msg, "10.0.0.9"}})
	if len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
	g := got[0]
	if g.Instance != "adb-ABC123XYZ-q1w2e3" || g.IP != "192.168.1.50" || g.Port != 40404 || g.Service != svcConnect {
		t.Fatalf("bad %+v", g)
	}
	if serialFromInstance(g.Instance) != "ABC123XYZ" {
		t.Fatal("serial")
	}
}

func TestAdbParsers(t *testing.T) {
	s := parseAdbMdns("List of discovered mdns services\nadb-ABC-x1\t_adb-tls-connect._tcp.\t192.168.1.50:33321\nscrcpy-abc\t_adb-tls-pairing._tcp\t192.168.1.50:41000\n")
	if len(s) != 2 || s[0].Port != 33321 || s[1].Service != svcPairing || s[1].Instance != "scrcpy-abc" {
		t.Fatalf("%+v", s)
	}
	d := parseAdbDevices("List of devices attached\n192.168.1.50:33321 device product:RMX model:RMX3636 device:x transport_id:3\nABC unauthorized usb:1-1 transport_id:4\n")
	if len(d) != 2 || d[0].Model != "RMX3636" || d[1].State != "unauthorized" {
		t.Fatalf("%+v", d)
	}
	a := splitArgs(`--crop=1:2:0:0 --window-title="My phone" -f`)
	if len(a) != 3 || a[1] != "--window-title=My phone" {
		t.Fatalf("%q", a)
	}
}

func TestParseAppList(t *testing.T) {
	out := "scrcpy 5.0.1 <https://github.com/Genymobile/scrcpy>\nINFO: ADB device found:\n[server] INFO: List of apps:\n" +
		" * Android System WebView      com.google.android.webview\n" +
		" - TikTok                      com.zhiliaoapp.musically\n" +
		" - Telegram                    org.telegram.messenger\n" +
		" - Мой МТС                     ru.mts.mymts\n"
	apps := parseAppList(out)
	if len(apps) != 4 || apps[0].Label != "Telegram" || apps[1].Label != "TikTok" || apps[2].Label != "Мой МТС" || !apps[3].System {
		t.Fatalf("%+v", apps)
	}
}

func TestHotkeysAndBattery(t *testing.T) {
	h, err := parseHotkey("Ctrl+Shift+S")
	if err != nil || h.Mods != 6 || h.VK != 'S' {
		t.Fatal(h, err)
	}
	if h, err := parseHotkey("Alt+F9"); err != nil || h.VK != 0x78 || h.Mods != 1 {
		t.Fatal(h, err)
	}
	if _, err := parseHotkey("S"); err == nil {
		t.Fatal("bare letter must be rejected")
	}
	if h, _ := parseHotkey(""); h.VK != 0 {
		t.Fatal("empty = disabled")
	}
	lvl, ch, ok := parseBattery("  AC powered: false\n  USB powered: true\n  status: 2\n  level: 14\n")
	if !ok || lvl != 14 || !ch {
		t.Fatal(lvl, ch, ok)
	}
}

func TestAudioArgs(t *testing.T) {
	s := defaultSettings()
	if j := strings.Join(s.args(), " "); strings.Contains(j, "audio") {
		t.Fatal(j)
	}
	s.AudioDup = true
	if j := strings.Join(s.args(), " "); !strings.Contains(j, "--audio-source=playback --audio-dup") {
		t.Fatal(j)
	}
	s.Audio = false
	if j := strings.Join(s.args(), " "); !strings.Contains(j, "--no-audio") || strings.Contains(j, "dup") {
		t.Fatal(j)
	}
}

func TestConnParsers(t *testing.T) {
	var r ConnResult
	ok := parseWifiInfo(`mWifiInfo SSID: "Home_5G", BSSID: aa:bb, MAC: x, IP: /192.168.1.50, Security type: 2, Supplicant state: COMPLETED, Wi-Fi standard: 11ac, RSSI: -52, Link speed: 433Mbps, Tx Link speed: 433Mbps, Max Supported Tx Link speed: 866Mbps, Rx Link speed: 390Mbps, Max Supported Rx Link speed: 866Mbps, Frequency: 5180MHz, Net ID: 3`, &r)
	if !ok || r.FreqMHz != 5180 || r.LinkMbps != 433 || r.RSSI != -52 || r.SSID != "Home_5G" {
		t.Fatalf("%+v", r)
	}
	r.Transport, r.RttMs, r.RttMaxMs = "wifi", 5, 12
	judge(&r)
	if r.Profile != "Лента / видео" || !r.GamesOK {
		t.Fatalf("%+v", r)
	}
	w := ConnResult{Transport: "wifi", FreqMHz: 2437, LinkMbps: 72, RttMs: 45, RttMaxMs: 300, RSSI: -75}
	judge(&w)
	if w.Profile != "Экономно" || len(w.Tips) < 3 {
		t.Fatalf("%+v", w)
	}
	hevc, name := parseEncoders("[server] INFO: List of video encoders:\n    --video-codec=h264 --video-encoder=c2.mtk.avc.encoder\n    --video-codec=h265 --video-encoder=c2.android.hevc.encoder   (sw)\n    --video-codec=h265 --video-encoder=c2.mtk.hevc.encoder\n")
	if !hevc || name != "c2.mtk.hevc.encoder" {
		t.Fatal(hevc, name)
	}
	if h, _ := parseEncoders("    --video-codec=h264 --video-encoder=c2.mtk.avc.encoder\n    --video-codec=h265 --video-encoder=c2.android.hevc.encoder   (sw)\n"); h {
		t.Fatal("sw only")
	}
}

func TestAudioPatterns(t *testing.T) {
	for _, l := range []string{"[server] WARN: Could not capture audio playback", "[server] ERROR: Failed to start audio capture",
		"WARN: Demuxer 'audio': stream explicitly disabled by the device", "[server] WARN: Audio disabled: audio playback capture source not supported before Android 13"} {
		if !reAudioDevFail.MatchString(l) && !reAudioNo13.MatchString(l) {
			t.Fatal("not matched:", l)
		}
	}
	if reAudioDevFail.MatchString("[server] WARN: Failed to start audio capture, retrying...") {
		t.Fatal("retry notice must not count as failure")
	}
	a := applyAudioSource([]string{"-s", "x", "--audio-source=playback", "--audio-dup", "--audio-buffer=100"}, "")
	if strings.Join(a, " ") != "-s x --audio-buffer=100" {
		t.Fatal(a)
	}
	if b := applyAudioSource([]string{"-s", "x"}, "playback"); strings.Join(b, " ") != "-s x --audio-source=playback" {
		t.Fatal(b)
	}
}

const sampleNotifDump = `  NotificationRecord(0x0d0a1b2c: pkg=org.telegram.messenger user=UserHandle{0} id=7 tag=null importance=4 key=0|org.telegram.messenger|7|null|10233: Notification(channel=private_combined shortcut=12 contentView=null vibrate=null sound=null defaults=0x0 flags=0x19 color=0xff2ca5e0 category=msg groupKey=messages vis=PRIVATE))
        android.title=String (Мама)
        android.text=String (Ты где?)
  NotificationRecord(0x0d0a1b2d: pkg=org.telegram.messenger user=UserHandle{0} id=8 tag=null importance=4 key=0|org.telegram.messenger|8|null|10233: Notification(channel=x flags=0x218 category=msg vis=PRIVATE))
        android.title=String (Telegram)
  NotificationRecord(0x1a: pkg=com.coloros.phonemanager user=UserHandle{0} id=1 tag=null importance=2 key=0|com.coloros.phonemanager|1|null|1000: Notification(channel=c flags=0x0 vis=PRIVATE))
        android.title=String (Очистка)
  NotificationRecord(0x1b: pkg=com.spotify.music user=UserHandle{0} id=2 tag=null importance=2 key=0|com.spotify.music|2|null|10111: Notification(channel=m flags=0x62 category=transport vis=PUBLIC))
        android.title=String (Song)
  NotificationRecord(0x1c: pkg=com.android.incallui user=UserHandle{0} id=3 tag=null importance=5 key=0|com.android.incallui|3|null|1001: Notification(channel=call flags=0x62 category=call vis=PUBLIC))
        android.title=String (Брат)
        android.text=SpannableString (Входящий вызов)
  NotificationRecord(0x0d0a1b2c: pkg=org.telegram.messenger user=UserHandle{0} id=7 tag=null importance=4 key=0|org.telegram.messenger|7|null|10233: Notification(channel=private_combined flags=0x19 category=msg vis=PRIVATE))
`

func TestNotifParse(t *testing.T) {
	ns := parseNotifications(sampleNotifDump)
	var show []PhoneNotif
	for _, n := range ns {
		if n.showable() {
			show = append(show, n)
		}
	}
	if len(show) != 1 || show[0].Title != "Мама" || show[0].Text != "Ты где?" || show[0].Pkg != "org.telegram.messenger" {
		t.Fatalf("%+v", show)
	}
	var call *PhoneNotif
	for i := range ns {
		if ns[i].Category == "call" {
			call = &ns[i]
		}
	}
	if call == nil || call.Title != "Брат" {
		t.Fatalf("call %+v", ns)
	}
	if m := reCall.FindStringSubmatch("  mCallState=1\n  mCallIncomingNumber=+79990001122\n"); m == nil || m[1] != "1" {
		t.Fatal("callstate")
	}
}

func TestSfxSynth(t *testing.T) {
	for _, n := range sfxNames {
		b := synth(n)
		if len(b) == 0 {
			t.Fatalf("%s: empty", n)
		}
		peak := 0.0
		for _, v := range b {
			if math.IsNaN(v) {
				t.Fatalf("%s: NaN", n)
			}
			peak = math.Max(peak, math.Abs(v))
		}
		if peak < 0.01 || peak > 1.2 {
			t.Fatalf("%s: peak %.3f", n, peak)
		}
		if w := sfxWav(n); len(w) < 44 || string(w[:4]) != "RIFF" {
			t.Fatalf("%s: bad wav", n)
		}
		t.Logf("%-12s %.2fs peak %.3f", n, float64(len(b))/sfxRate, peak)
	}
}

func TestSfxDump(t *testing.T) {
	out := os.Getenv("SFX_DUMP")
	if out == "" {
		t.Skip()
	}
	var all sfxBuf
	for _, n := range []string{"crt", "hiss", "whoosh_up", "chime", "whoosh_down", "thump", "shutter", "rec_start", "rec_stop", "notify", "lost", "tick", "pop"} {
		all = append(all, synth(n)...)
		all = append(all, make(sfxBuf, sfxRate/3)...)
	}
	os.WriteFile(out, all.wav(), 0644)
}
