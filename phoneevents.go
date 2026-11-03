package main

// Phone notifications and incoming calls, read over adb (no app on the phone needed).

import (
	"context"
	"hash/fnv"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type PhoneNotif struct {
	Key, Pkg, Title, Text, Category string
	Flags                           int64
}

type CallInfo struct {
	State string `json:"state"` // "ringing" | "offhook"
	Name  string `json:"name"`
	Since int64  `json:"since"`
}

var (
	reNRec     = regexp.MustCompile(`NotificationRecord\(0x[0-9a-f]+: pkg=(\S+) .*? key=(\S+?): Notification\((.*)`)
	reNFlags   = regexp.MustCompile(`\bflags=0x([0-9a-f]+)`)
	reNCat     = regexp.MustCompile(`\bcategory=(\w+)`)
	reNExtra   = regexp.MustCompile(`^\s*android\.(title|text|bigText|conversationTitle)=\w+ \((.*)\)\s*$`)
	reCall     = regexp.MustCompile(`mCallState=(\d)`)
	reCallNum  = regexp.MustCompile(`mCallIncomingNumber=(\S*)`)
	sysPkgPref = []string{"android", "com.android.", "com.coloros.", "com.oplus.", "com.heytap.", "com.realme.", "com.google.android.gms",
		"com.google.android.apps.messaging.ui", "com.mediatek.", "com.qualcomm."}
)

// Dialer packages show the call notification; everything else from them is ignored.
var dialerPkgs = []string{"com.android.incallui", "com.android.dialer", "com.google.android.dialer", "com.android.server.telecom", "com.oplus.incallui", "com.coloros.incallui"}

func isSystemPkg(p string) bool {
	if p == "android" {
		return true
	}
	for _, x := range sysPkgPref {
		if x != "android" && strings.HasPrefix(p, x) {
			return true
		}
	}
	return false
}

func parseNotifications(out string) []PhoneNotif {
	var res []PhoneNotif
	seen := map[string]int{}
	var cur *PhoneNotif
	flush := func() {
		if cur == nil {
			return
		}
		if i, ok := seen[cur.Key]; ok {
			// the same record can be listed twice; keep the one with text
			if res[i].Title == "" && res[i].Text == "" {
				res[i] = *cur
			}
		} else {
			seen[cur.Key] = len(res)
			res = append(res, *cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if m := reNRec.FindStringSubmatch(line); m != nil {
			flush()
			cur = &PhoneNotif{Pkg: m[1], Key: m[2]}
			if f := reNFlags.FindStringSubmatch(m[3]); f != nil {
				cur.Flags, _ = strconv.ParseInt(f[1], 16, 64)
			}
			if c := reNCat.FindStringSubmatch(m[3]); c != nil {
				cur.Category = c[1]
			}
			continue
		}
		if cur == nil {
			continue
		}
		if m := reNExtra.FindStringSubmatch(line); m != nil {
			v := strings.TrimSpace(m[2])
			if v == "null" {
				continue
			}
			switch m[1] {
			case "title":
				cur.Title = v
			case "conversationTitle":
				if cur.Title == "" {
					cur.Title = v
				}
			case "text":
				cur.Text = v
			case "bigText":
				if cur.Text == "" {
					cur.Text = v
				}
			}
		}
	}
	flush()
	return res
}

// worth showing on the PC: no ongoing/foreground/group-summary/system ones.
func (n PhoneNotif) showable() bool {
	if n.Flags&(0x2|0x40|0x200) != 0 || n.Category == "call" || n.Category == "service" || n.Category == "progress" ||
		n.Category == "sys" || n.Category == "transport" {
		return false
	}
	if isSystemPkg(n.Pkg) {
		return false
	}
	return n.Title != "" || n.Text != ""
}

func hashStr(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

type phoneWatch struct {
	seen    map[string]uint64 // notification key -> text hash
	primed  bool
	call    *CallInfo
	lastToT time.Time
}

func (a *App) appLabel(id, pkg string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.appCache[id] {
		if e.Pkg == pkg {
			return e.Label
		}
	}
	// Fallback: last part of the package, capitalised ("org.telegram.messenger" → "Messenger")
	parts := strings.Split(pkg, ".")
	p := parts[len(parts)-1]
	if p == "android" && len(parts) > 1 {
		p = parts[len(parts)-2]
	}
	if p != "" {
		p = strings.ToUpper(p[:1]) + p[1:]
	}
	return p
}

// phoneEventsLoop polls each connected phone: calls every ~1.5 s, notifications every ~3 s.
func (a *App) phoneEventsLoop() {
	watches := map[string]*phoneWatch{}
	tick := 0
	for {
		time.Sleep(1500 * time.Millisecond)
		tick++
		a.mu.Lock()
		notifOn, callsOn := a.cfg.Settings.PhoneNotifs, a.cfg.Settings.PhoneCalls
		var devs []DeviceView
		for _, d := range a.devices {
			if d.State == "device" {
				devs = append(devs, d)
			}
		}
		a.mu.Unlock()
		if !notifOn && !callsOn {
			continue
		}
		online := map[string]bool{}
		for _, d := range devs {
			online[d.ID] = true
			w := watches[d.ID]
			if w == nil {
				w = &phoneWatch{seen: map[string]uint64{}}
				watches[d.ID] = w
				go a.listApps(d.Serial, false) // app names for nicer notifications
			}
			wantNotifs := notifOn && (tick%2 == 0 || !w.primed)
			cmd := "dumpsys telephony.registry | grep -m4 -E 'mCallState|mCallIncomingNumber'"
			if wantNotifs || w.call != nil { // during a call: the caller's name comes from its notification
				cmd += "; echo ===N===; dumpsys notification --noredact | grep -E 'NotificationRecord\\(|android\\.(title|text|bigText|conversationTitle)='"
			}
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			out, err := a.adbCtx(ctx, "-s", d.Serial, "shell", cmd)
			cancel()
			if err != nil && out == "" {
				continue
			}
			callPart, notifPart := out, ""
			if i := strings.Index(out, "===N==="); i >= 0 {
				callPart, notifPart = out[:i], out[i+7:]
			}
			notifs := parseNotifications(notifPart)
			if callsOn {
				a.handleCall(d, w, callPart, notifs)
			}
			if wantNotifs {
				a.handleNotifs(d, w, notifs)
			}
		}
		for id := range watches {
			if !online[id] {
				delete(watches, id)
				a.setCall(id, nil)
			}
		}
	}
}

func (a *App) handleNotifs(d DeviceView, w *phoneWatch, notifs []PhoneNotif) {
	var fresh []PhoneNotif
	keep := map[string]bool{}
	for _, n := range notifs {
		if !n.showable() {
			continue
		}
		keep[n.Key] = true
		h := hashStr(n.Title + "\x00" + n.Text)
		if old, ok := w.seen[n.Key]; !ok || old != h {
			w.seen[n.Key] = h
			if w.primed {
				fresh = append(fresh, n)
			}
		}
	}
	for k := range w.seen {
		if !keep[k] {
			delete(w.seen, k)
		}
	}
	w.primed = true
	if len(fresh) == 0 {
		return
	}
	// Don't pop up over the phone's own window: you're already looking at it.
	if fg := foregroundPid(); fg != 0 && a.isOurScrcpyPid(fg) {
		return
	}
	n := fresh[len(fresh)-1]
	label := a.appLabel(d.ID, n.Pkg)
	a.mu.Lock()
	showText := a.cfg.Settings.NotifText
	a.mu.Unlock()
	title := label
	if n.Title != "" && showText {
		title = label + " — " + n.Title
	}
	text := n.Text
	if !showText {
		text = "Новое уведомление"
	}
	if len(fresh) > 1 {
		text += "  (+" + strconv.Itoa(len(fresh)-1) + ")"
	}
	if len([]rune(text)) > 200 {
		text = string([]rune(text)[:199]) + "…"
	}
	serial, pkg := d.Serial, n.Pkg
	trayNotifyAction(title, text, func() { a.openApp(serial, pkg, label) })
	go a.sfx("notify")
	a.logf("🔔 %s: %s", label, strings.TrimSpace(n.Title+" "+map[bool]string{true: n.Text, false: ""}[showText]))
}

func (a *App) handleCall(d DeviceView, w *phoneWatch, callPart string, notifs []PhoneNotif) {
	state := "0"
	if m := reCall.FindStringSubmatch(callPart); m != nil {
		state = m[1]
	}
	switch state {
	case "1", "2":
		st := map[string]string{"1": "ringing", "2": "offhook"}[state]
		name := ""
		for _, n := range notifs {
			if n.Category == "call" || containsStr(dialerPkgs, n.Pkg) {
				if n.Title != "" {
					name = n.Title
					break
				}
			}
		}
		if name == "" {
			if m := reCallNum.FindStringSubmatch(callPart); m != nil && m[1] != "" {
				name = m[1]
			}
		}
		if name == "" {
			name = "Неизвестный номер"
		}
		if w.call == nil || w.call.State != st {
			since := time.Now().Unix()
			if w.call != nil && w.call.State == "offhook" {
				since = w.call.Since
			}
			w.call = &CallInfo{State: st, Name: name, Since: since}
			if st == "ringing" {
				a.logf("📞 Звонит: %s", name)
			}
		} else if name != "Неизвестный номер" {
			w.call.Name = name
		}
		c := *w.call
		a.setCall(d.ID, &c)
		serial := d.Serial
		callPopupShow(c, d.Name, func(act string) { a.callAction(serial, act) })
	default:
		if w.call != nil {
			w.call = nil
			a.setCall(d.ID, nil)
			callPopupHide()
		}
	}
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (a *App) setCall(id string, c *CallInfo) {
	a.mu.Lock()
	if c == nil {
		delete(a.calls, id)
	} else {
		a.calls[id] = c
	}
	for i := range a.devices {
		if a.devices[i].ID == id {
			a.devices[i].Call = c
		}
	}
	a.mu.Unlock()
}

// callAction: answer = headset-button press (answers a ringing call on any Android);
// decline/hang up = ENDCALL.
func (a *App) callAction(serial, act string) error {
	key := "KEYCODE_ENDCALL"
	if act == "answer" {
		key = "KEYCODE_HEADSETHOOK"
	}
	out, err := a.adb(5*time.Second, "-s", serial, "shell", "input keyevent "+key)
	a.logf("📞 %s → %s", map[string]string{"answer": "Ответить", "decline": "Сбросить", "hangup": "Завершить"}[act], strings.TrimSpace(out))
	return err
}

func (a *App) isOurScrcpyPid(pid int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.sessions {
		if s.cmd.Process != nil && s.cmd.Process.Pid == pid {
			return true
		}
	}
	return false
}
