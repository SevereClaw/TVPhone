package main

// Automatic updates of the app itself from GitHub releases (SevereClaw/TVPhone).
// A newer release is downloaded and installed with the same self-update that the
// "update from archive" button uses; it waits while a phone window is open.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const appRepo = "SevereClaw/TVPhone"

type AppUpdState struct {
	Current    string `json:"current"`
	Latest     string `json:"latest"`
	Available  bool   `json:"available"`
	PageURL    string `json:"pageUrl"`
	Notes      string `json:"notes"`
	Checking   bool   `json:"checking"`
	Installing bool   `json:"installing"`
	Waiting    bool   `json:"waiting"` // auto-update postponed: a phone window is open
	Progress   int    `json:"progress"`
	Error      string `json:"error"`
	CheckedAt  int64  `json:"checkedAt"`
	assetURL   string
	assetName  string
	assetSize  int64
}

var reAppAsset = regexp.MustCompile(`(?i)^TVPhone.*\.zip$`)

func appReleasesURL() string {
	if u := os.Getenv("LAUNCHER_APP_UPDATE_URL"); u != "" {
		return u
	}
	return "https://api.github.com/repos/" + appRepo + "/releases/latest"
}

func (a *App) checkAppUpdate() error {
	a.mu.Lock()
	if a.appUpd.Checking || a.appUpd.Installing {
		a.mu.Unlock()
		return nil
	}
	a.appUpd.Checking, a.appUpd.Error = true, ""
	a.mu.Unlock()

	var rel struct {
		Tag    string `json:"tag_name"`
		URL    string `json:"html_url"`
		Body   string `json:"body"`
		Draft  bool   `json:"draft"`
		Pre    bool   `json:"prerelease"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	err := func() error {
		req, _ := http.NewRequest("GET", appReleasesURL(), nil)
		req.Header.Set("User-Agent", "TVPhone-updater")
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
		if err != nil {
			return fmt.Errorf("нет связи с GitHub")
		}
		defer resp.Body.Close()
		if resp.StatusCode == 404 {
			return fmt.Errorf("релизов пока нет")
		}
		if resp.StatusCode != 200 {
			return fmt.Errorf("GitHub ответил %d", resp.StatusCode)
		}
		return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel)
	}()

	a.mu.Lock()
	defer a.mu.Unlock()
	u := &a.appUpd
	u.Checking = false
	u.Current = appVersion
	if err != nil {
		u.Error = err.Error()
		return err
	}
	u.CheckedAt = time.Now().Unix()
	u.Latest = strings.TrimPrefix(strings.TrimPrefix(rel.Tag, "v"), "V")
	u.PageURL = rel.URL
	u.Notes = rel.Body
	if len([]rune(u.Notes)) > 1500 {
		u.Notes = string([]rune(u.Notes)[:1500]) + "…"
	}
	u.assetURL = ""
	for _, as := range rel.Assets {
		if reAppAsset.MatchString(as.Name) {
			u.assetURL, u.assetName, u.assetSize = as.URL, as.Name, as.Size
		}
	}
	u.Available = !rel.Draft && !rel.Pre && u.assetURL != "" && newerVer(u.Latest, appVersion)
	return nil
}

// installAppUpdate downloads the release zip and hands it to selfUpdate (via the
// hook set up in main, which knows the port/token for a seamless restart).
func (a *App) installAppUpdate() error {
	a.mu.Lock()
	if a.appUpd.Installing {
		a.mu.Unlock()
		return fmt.Errorf("обновление уже идёт")
	}
	if !a.appUpd.Available || a.appUpd.assetURL == "" {
		a.mu.Unlock()
		return fmt.Errorf("новой версии нет")
	}
	url, name, size, ver := a.appUpd.assetURL, a.appUpd.assetName, a.appUpd.assetSize, a.appUpd.Latest
	apply := a.applySelfUpdate
	a.appUpd.Installing, a.appUpd.Error, a.appUpd.Progress = true, "", 0
	a.mu.Unlock()
	fail := func(err error) error {
		a.mu.Lock()
		a.appUpd.Installing, a.appUpd.Error = false, err.Error()
		a.mu.Unlock()
		a.logf("Автообновление не удалось: %v", err)
		return err
	}
	if apply == nil {
		return fail(fmt.Errorf("обновление сейчас недоступно"))
	}
	a.logf("⬇ Скачиваю TVPhone %s…", ver)
	tmp, err := os.MkdirTemp("", "tvphone-dl-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(tmp)
	zipPath := filepath.Join(tmp, name)
	if err := downloadFile(url, zipPath, size, func(p int) {
		a.mu.Lock()
		a.appUpd.Progress = p
		a.mu.Unlock()
	}); err != nil {
		return fail(fmt.Errorf("не скачалось: %v", err))
	}
	f, err := os.Open(zipPath)
	if err != nil {
		return fail(err)
	}
	defer f.Close()
	if err := apply(f, name); err != nil {
		return fail(err)
	}
	return nil // the new version takes over from here
}

// appUpdateLoop: check soon after start and every 3 hours; install automatically
// (if enabled) as soon as no phone window is open.
func (a *App) appUpdateLoop() {
	first := 20 * time.Second
	if d, err := time.ParseDuration(os.Getenv("LAUNCHER_APP_UPDATE_DELAY")); err == nil {
		first = d
	}
	time.Sleep(first)
	notified := ""
	for {
		a.checkAppUpdate()
		a.mu.Lock()
		avail, latest := a.appUpd.Available, a.appUpd.Latest
		auto := a.cfg.Settings.AutoUpdateApp
		a.mu.Unlock()
		if avail && !auto && notified != latest {
			notified = latest
			trayNotify("Вышла новая версия TVPhone "+latest, "Откройте программу и нажмите «Обновить».")
		}
		if avail && auto {
			// don't cut off a stream or a recording: wait until windows are closed
			for {
				a.mu.Lock()
				busy := len(a.sessions) > 0
				a.appUpd.Waiting = busy
				a.mu.Unlock()
				if !busy {
					break
				}
				time.Sleep(time.Minute)
			}
			a.installAppUpdate()
		}
		time.Sleep(3 * time.Hour)
	}
}
