package main

// scrcpy update check (GitHub releases) and one-click install of the win64 build.

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const releasesAPI = "https://api.github.com/repos/Genymobile/scrcpy/releases/latest"
const releasesPage = "https://github.com/Genymobile/scrcpy/releases"

type UpdateState struct {
	Local      string `json:"local"`
	Latest     string `json:"latest"`
	Available  bool   `json:"available"`
	PageURL    string `json:"pageUrl"`
	assetURL   string
	assetSize  int64
	Checking   bool   `json:"checking"`
	Installing bool   `json:"installing"`
	Progress   int    `json:"progress"` // 0..100 while downloading
	Stage      string `json:"stage"`
	Error      string `json:"error"`
	CheckedAt  int64  `json:"checkedAt"`
}

var reVer = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

func parseVer(s string) [3]int {
	var v [3]int
	m := reVer.FindStringSubmatch(s)
	if m == nil {
		return v
	}
	for i := 0; i < 3; i++ {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	return v
}

func newerVer(a, b string) bool { // a > b
	va, vb := parseVer(a), parseVer(b)
	for i := 0; i < 3; i++ {
		if va[i] != vb[i] {
			return va[i] > vb[i]
		}
	}
	return false
}

func (a *App) localScrcpyVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.scrcpy, "--version")
	cmd.Dir = a.toolDir
	hideWindow(cmd)
	cmd.WaitDelay = time.Second
	out, _ := cmd.CombinedOutput()
	if m := regexp.MustCompile(`scrcpy (\d+\.\d+(?:\.\d+)?)`).FindStringSubmatch(string(out)); m != nil {
		return m[1]
	}
	return ""
}

func updateURL() string {
	if u := os.Getenv("LAUNCHER_UPDATE_URL"); u != "" {
		return u
	}
	return releasesAPI
}

func (a *App) checkUpdate(manual bool) {
	a.mu.Lock()
	if a.upd.Checking || a.upd.Installing {
		a.mu.Unlock()
		return
	}
	a.upd.Checking = true
	a.upd.Error = ""
	a.mu.Unlock()

	local := a.localScrcpyVersion()
	var rel struct {
		Tag    string `json:"tag_name"`
		URL    string `json:"html_url"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			Size int64  `json:"size"`
		} `json:"assets"`
	}
	err := func() error {
		req, _ := http.NewRequest("GET", updateURL(), nil)
		req.Header.Set("User-Agent", "phone-on-pc-launcher")
		req.Header.Set("Accept", "application/vnd.github+json")
		c := http.Client{Timeout: 20 * time.Second}
		resp, err := c.Do(req)
		if err != nil {
			return fmt.Errorf("нет связи с GitHub")
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("GitHub ответил %d", resp.StatusCode)
		}
		return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel)
	}()

	a.mu.Lock()
	defer a.mu.Unlock()
	a.upd.Checking = false
	a.upd.Local = local
	if err != nil {
		a.upd.Error = err.Error()
		return
	}
	a.upd.CheckedAt = time.Now().Unix()
	a.cfg.UpdateCheckedAt = a.upd.CheckedAt
	a.upd.Latest = strings.TrimPrefix(rel.Tag, "v")
	a.upd.PageURL = rel.URL
	if !strings.HasPrefix(a.upd.PageURL, releasesPage) {
		a.upd.PageURL = releasesPage
	}
	a.upd.assetURL = ""
	for _, as := range rel.Assets {
		if regexp.MustCompile(`^scrcpy-win64-.*\.zip$`).MatchString(as.Name) {
			a.upd.assetURL, a.upd.assetSize = as.URL, as.Size
		}
	}
	wasAvail := a.upd.Available
	a.upd.Available = local != "" && a.upd.Latest != "" && newerVer(a.upd.Latest, local)
	a.saveLocked()
	if a.upd.Available && !wasAvail && !manual && a.cfg.NotifiedVersion != a.upd.Latest {
		a.cfg.NotifiedVersion = a.upd.Latest
		a.saveLocked()
		go trayNotify("Вышла scrcpy "+a.upd.Latest, "Откройте программу, чтобы обновить в один клик.")
	}
}

func (a *App) updateLoop() {
	time.Sleep(15 * time.Second)
	for {
		a.mu.Lock()
		last := a.cfg.UpdateCheckedAt
		known := a.upd.Latest != ""
		a.mu.Unlock()
		if time.Since(time.Unix(last, 0)) > 20*time.Hour || !known {
			a.checkUpdate(false)
		}
		time.Sleep(6 * time.Hour)
	}
}

func (a *App) setUpdStage(stage string, progress int) {
	a.mu.Lock()
	a.upd.Stage, a.upd.Progress = stage, progress
	a.mu.Unlock()
}

// installUpdate downloads the win64 zip and replaces scrcpy files in place.
// User files (.bat, this launcher, settings) are never touched.
func (a *App) installUpdate() error {
	a.mu.Lock()
	if a.upd.Installing {
		a.mu.Unlock()
		return fmt.Errorf("обновление уже идёт")
	}
	if a.upd.assetURL == "" {
		a.mu.Unlock()
		return fmt.Errorf("сначала проверьте обновления")
	}
	url, size, ver := a.upd.assetURL, a.upd.assetSize, a.upd.Latest
	ids := []string{}
	var reopen []string // phones whose screen was open: reopen after the update
	for id := range a.sessions {
		ids = append(ids, id)
		if !strings.HasSuffix(id, camSuffix) && !strings.Contains(id, appKeySep) {
			reopen = append(reopen, id)
		}
	}
	a.upd.Installing, a.upd.Error = true, ""
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.upd.Installing = false
		a.mu.Unlock()
	}()
	fail := func(err error) error {
		a.mu.Lock()
		a.upd.Error = err.Error()
		a.mu.Unlock()
		a.logf("Обновление не удалось: %v", err)
		return err
	}

	a.logf("Скачиваю scrcpy %s…", ver)
	tmp, err := os.MkdirTemp("", "scrcpy-update-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(tmp)
	zipPath := filepath.Join(tmp, "scrcpy.zip")
	if err := downloadFile(url, zipPath, size, func(p int) { a.setUpdStage("download", p) }); err != nil {
		return fail(fmt.Errorf("не скачалось: %v", err))
	}

	a.setUpdStage("install", 100)
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fail(fmt.Errorf("архив повреждён: %v", err))
	}
	defer zr.Close()
	// Strip the top folder ("scrcpy-win64-vX.Y/").
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		name := strings.ReplaceAll(f.Name, "\\", "/")
		if i := strings.Index(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		if name == "" || strings.HasSuffix(name, "/") || strings.Contains(name, "..") || strings.Contains(name, "/") {
			continue // only top-level files, nothing outside the folder
		}
		files[name] = f
	}
	if files["scrcpy"+exeSuffix] == nil || files["scrcpy-server"] == nil {
		return fail(fmt.Errorf("в архиве нет scrcpy.exe"))
	}
	self, _ := os.Executable()
	selfName := strings.ToLower(filepath.Base(self))

	// Extract to staging first, so a broken download never half-replaces anything.
	stage := filepath.Join(tmp, "new")
	os.MkdirAll(stage, 0755)
	for name, f := range files {
		if strings.ToLower(name) == selfName || strings.HasSuffix(strings.ToLower(name), ".bat") {
			continue
		}
		if err := extractZipFile(f, filepath.Join(stage, name)); err != nil {
			return fail(fmt.Errorf("распаковка: %v", err))
		}
	}

	// Close scrcpy windows and stop the adb server: they lock their files.
	for _, id := range ids {
		a.stopWait(id)
	}
	a.adb(10*time.Second, "kill-server")
	time.Sleep(1500 * time.Millisecond)

	entries, _ := os.ReadDir(stage)
	var done []string
	rollback := func() {
		for _, name := range done {
			dst := filepath.Join(a.toolDir, name)
			os.Remove(dst)
			os.Rename(dst+".old", dst)
		}
	}
	for _, e := range entries {
		name := e.Name()
		dst := filepath.Join(a.toolDir, name)
		os.Remove(dst + ".old")
		if _, err := os.Stat(dst); err == nil {
			if err := os.Rename(dst, dst+".old"); err != nil {
				rollback()
				a.startServer()
				return fail(fmt.Errorf("файл %s занят — закройте scrcpy и попробуйте ещё раз", name))
			}
		}
		if err := copyFile(filepath.Join(stage, name), dst); err != nil {
			os.Remove(dst)
			os.Rename(dst+".old", dst)
			rollback()
			a.startServer()
			return fail(fmt.Errorf("не удалось записать %s: %v", name, err))
		}
		done = append(done, name)
	}
	for _, name := range done {
		os.Remove(filepath.Join(a.toolDir, name+".old"))
	}
	a.startServer()
	a.mu.Lock()
	a.props = map[string]devProps{}
	a.mu.Unlock()
	local := a.localScrcpyVersion()
	a.mu.Lock()
	a.upd.Local = local
	a.upd.Available = newerVer(a.upd.Latest, local)
	a.upd.Stage = ""
	a.mu.Unlock()
	a.logf("✔ scrcpy обновлён до %s", local)
	a.event("info", "", "scrcpy обновлён до "+local)
	a.poke()
	// adb kill-server dropped Wi-Fi connections; the watcher reconnects them.
	go func() {
		for _, id := range reopen {
			for i := 0; i < 30; i++ {
				time.Sleep(time.Second)
				a.mu.Lock()
				d := a.findDeviceLocked(id)
				ready := d != nil && d.State == "device"
				_, running := a.sessions[id]
				a.mu.Unlock()
				if running {
					break
				}
				if ready {
					a.launch(id, nil)
					break
				}
			}
		}
	}()
	return nil
}

func downloadFile(url, dst string, size int64, progress func(int)) error {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "phone-on-pc-launcher")
	c := http.Client{Timeout: 15 * time.Minute}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > 0 {
		size = resp.ContentLength
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 256<<10)
	var got int64
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			got += int64(n)
			if size > 0 {
				progress(int(got * 100 / size))
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if size > 0 && got != size {
		return fmt.Errorf("скачано %d из %d байт", got, size)
	}
	return nil
}

func extractZipFile(f *zip.File, dst string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, rc)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0755)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
