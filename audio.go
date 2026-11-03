package main

// Phone audio on the PC: notice when scrcpy can't capture it (scrcpy then silently
// mirrors video only), retry with another capture source, and remember what works.

import (
	"regexp"
	"strings"
	"time"
)

var (
	reAudioDevFail = regexp.MustCompile(`(?i)(Failed to start audio capture\s*$|Could not capture audio playback|Cannot create AudioRecord|Audio capture error|Audio recording error|Audio encoding error|Could not create (default )?audio encoder|Demuxer 'audio': stream explicitly disabled|Audio disabled)`)
	reAudioOld     = regexp.MustCompile(`(?i)not supported before Android 11`)
	reAudioNo13    = regexp.MustCompile(`(?i)not supported before Android 13`)
	reAudioPCFail  = regexp.MustCompile(`(?i)(Could not open audio device|Could not initialize SDL audio|Could not resume audio device)`)
)

// Capture sources tried in order. "" = scrcpy's default (output: phone goes quiet,
// sound plays on the PC).
var audioChain = []string{"", "playback"}

func (a *App) audioSourceFor(id string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.AudioSrc[id]
}

// audioLine inspects one line of scrcpy output for a session.
func (a *App) audioLine(key, devID, serial, line string, sess *session) {
	if sess.camera != "" || sess.app != "" || sess.audioHandled {
		return
	}
	switch {
	case reAudioPCFail.MatchString(line):
		sess.audioHandled = true
		a.setAudioNote(devID, "звук: нет устройства вывода на ПК")
		a.event("error", "", "Звук телефона не играет: Windows не дала устройство вывода. Проверьте, что колонки/наушники подключены и выбраны по умолчанию.")
	case reAudioOld.MatchString(line):
		sess.audioHandled = true
		a.setAudioNote(devID, "звук: нужен Android 11+")
		a.event("error", "", "Передача звука работает только с Android 11 и новее.")
	case reAudioDevFail.MatchString(line) || reAudioNo13.MatchString(line):
		sess.audioHandled = true
		go a.audioFallback(key, devID, serial, sess, line)
	}
}

func (a *App) setAudioNote(id, note string) {
	a.mu.Lock()
	if a.audioNote == nil {
		a.audioNote = map[string]string{}
	}
	a.audioNote[id] = note
	a.syncRunningLocked()
	a.mu.Unlock()
}

func (a *App) audioFallback(key, devID, serial string, sess *session, why string) {
	time.Sleep(400 * time.Millisecond)
	a.mu.Lock()
	cur := sess.audioSrc
	if sess.dup {
		cur = "dup"
	}
	recording := sess.record != ""
	fs, off := sess.fullscreen, sess.screenOff
	alive := a.sessions[key] == sess
	a.mu.Unlock()
	if !alive {
		return
	}
	a.logf("🔇 Звук с телефона не захватился (%s, способ «%s»)", strings.TrimSpace(why), map[string]string{"": "основной", "playback": "playback", "dup": "ПК+телефон"}[cur])

	next, found := "", false
	if cur == "dup" {
		next, found = "", true // drop "both", try the plain default first
	} else {
		for i, s := range audioChain {
			if s == cur && i+1 < len(audioChain) {
				next, found = audioChain[i+1], true
			}
		}
	}
	if !found {
		a.setAudioNote(devID, "звук: не передаётся")
		a.event("error", "", "Звук с телефона не передаётся. Разблокируйте телефон и откройте экран заново; если не поможет — пришлите строки из «Журнала».")
		trayNotify("Нет звука с телефона", "Разблокируйте телефон и откройте экран заново.")
		return
	}
	if recording {
		a.setAudioNote(devID, "звук: не идёт (идёт запись)")
		a.event("error", "", "Звук с телефона не захватился. Остановите запись и откройте экран заново.")
		return
	}
	a.mu.Lock()
	if a.cfg.AudioSrc == nil {
		a.cfg.AudioSrc = map[string]string{}
	}
	a.cfg.AudioSrc[devID] = next
	if cur == "dup" {
		a.cfg.Settings.AudioDup = false
	}
	a.saveLocked()
	a.mu.Unlock()
	a.setAudioNote(devID, "звук: пробую другой способ…")
	a.logf("🔁 Перезапускаю экран с другим способом захвата звука")
	a.stopWait(key)
	src := next
	a.launchOpts(serial, launchOpts{fullscreen: &fs, screenOff: &off, audioSource: &src})
}

// applyAudioSource rewrites the audio flags for a chosen capture source.
func applyAudioSource(args []string, src string) []string {
	out := args[:0:0]
	for _, x := range args {
		if strings.HasPrefix(x, "--audio-source=") || x == "--audio-dup" {
			continue
		}
		out = append(out, x)
	}
	if src != "" {
		out = append(out, "--audio-source="+src)
	}
	return out
}
