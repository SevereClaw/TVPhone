package main

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const phoneDownloads = "/sdcard/Download"

func safeFileName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		name = "file"
	}
	return name
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// pushFile streams an uploaded file to a temp folder, then installs it (.apk)
// or copies it to the phone's Download folder.
func (a *App) pushFile(serial, name string, body io.Reader) (string, error) {
	a.mu.Lock()
	dev := a.findDeviceLocked(serial)
	if dev == nil || dev.State != "device" {
		a.mu.Unlock()
		return "", fmt.Errorf("телефон не найден")
	}
	d := *dev
	a.mu.Unlock()

	name = safeFileName(name)
	tmpDir, err := os.MkdirTemp("", "phone-push-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)
	local := filepath.Join(tmpDir, name)
	f, err := os.Create(local)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, body)
	f.Close()
	if err != nil {
		return "", fmt.Errorf("файл не загрузился: %v", err)
	}
	return a.pushLocal(d, local, name)
}

// pushLocalPath sends a file that already exists on the PC (used by the window toolbar).
func (a *App) pushLocalPath(serial, local string) (string, error) {
	a.mu.Lock()
	dev := a.findDeviceLocked(serial)
	if dev == nil || dev.State != "device" {
		a.mu.Unlock()
		return "", fmt.Errorf("телефон не найден")
	}
	d := *dev
	a.mu.Unlock()
	return a.pushLocal(d, local, safeFileName(filepath.Base(local)))
}

func (a *App) pushLocal(d DeviceView, local, name string) (string, error) {
	var n int64
	if fi, err := os.Stat(local); err == nil {
		n = fi.Size()
	}

	if strings.EqualFold(filepath.Ext(name), ".apk") {
		a.logf("📦 Устанавливаю %s на %s… (на телефоне может появиться запрос — подтвердите)", name, d.Name)
		out, _ := a.adb(5*time.Minute, "-s", d.Serial, "install", "-r", local)
		out = strings.TrimSpace(out)
		a.logf("adb install → %s", lastLine(out))
		if !strings.Contains(out, "Success") {
			return "", fmt.Errorf("приложение не установилось: %s", lastLine(out))
		}
		return "Приложение " + strings.TrimSuffix(name, filepath.Ext(name)) + " установлено", nil
	}

	remote := path.Join(phoneDownloads, name)
	ext := path.Ext(name)
	for i := 2; i < 100; i++ {
		out, _ := a.adb(6*time.Second, "-s", d.Serial, "shell", "[ -e "+shQuote(remote)+" ] && echo EXISTS")
		if !strings.Contains(out, "EXISTS") {
			break
		}
		remote = path.Join(phoneDownloads, fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, ext), i, ext))
	}
	a.logf("⇪ Отправляю %s (%s) на %s…", name, humanSize(n), d.Name)
	out, err := a.adb(60*time.Minute, "-s", d.Serial, "push", local, remote)
	out = strings.TrimSpace(out)
	if err != nil || strings.Contains(out, "error") || strings.Contains(out, "failed") {
		a.logf("adb push → %s", lastLine(out))
		return "", fmt.Errorf("не удалось отправить: %s", lastLine(out))
	}
	// Let the gallery / music apps see the new file right away.
	a.adb(6*time.Second, "-s", d.Serial, "shell",
		"am broadcast -a android.intent.action.MEDIA_SCANNER_SCAN_FILE -d "+shQuote("file://"+remote))
	a.logf("✔ %s → %s", name, remote)
	return "Отправлено в «Загрузки»: " + path.Base(remote), nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r", ""))
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f ГБ", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f МБ", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d КБ", n>>10)
	}
	return fmt.Sprintf("%d Б", n)
}
