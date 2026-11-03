package main

// Self-update from a zip the user picks: no more "file is in use" errors.
// A running .exe can't be overwritten on Windows, but it CAN be renamed — so the
// current exe is moved aside to *.old, the new one is written in its place, adb is
// stopped (it locks adb.exe), and the new version is started on the same port/token
// so the open window just carries on.

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const selfExeName = "Телефон на ПК.exe"

// cleanupOld removes *.old files left by a previous self-update (they may still be
// locked for a moment while the old process exits).
func (a *App) cleanupOld() {
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	for i := 0; i < 20; i++ {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.old"))
		left := 0
		for _, m := range matches {
			if os.Remove(m) != nil {
				left++
			}
		}
		if left == 0 {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// stopAdbIfIdle stops the adb server on exit so the folder can be overwritten,
// unless scrcpy windows are still open (they need it).
func (a *App) stopAdbIfIdle() {
	a.mu.Lock()
	busy := len(a.sessions) > 0
	a.mu.Unlock()
	if !busy {
		a.adb(5*time.Second, "kill-server")
	}
}

type updateResult struct {
	Version string
}

// selfUpdate installs the app from a zip (or a bare exe) and returns once the new
// version has been started; the caller then quits.
func (a *App) selfUpdate(body io.Reader, filename string, port int, token string) (*updateResult, error) {
	tmp, err := os.MkdirTemp("", "phone-on-pc-update-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	pkg := filepath.Join(tmp, "pkg")
	f, err := os.Create(pkg)
	if err != nil {
		return nil, err
	}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		return nil, fmt.Errorf("файл не загрузился: %v", err)
	}
	f.Close()

	exe, _ := os.Executable()
	appDir := filepath.Dir(exe)
	stage := filepath.Join(tmp, "new")
	os.MkdirAll(stage, 0755)
	newExeName := filepath.Base(exe)

	if strings.EqualFold(filepath.Ext(filename), ".exe") {
		if err := copyFile(pkg, filepath.Join(stage, newExeName)); err != nil {
			return nil, err
		}
	} else {
		zr, err := zip.OpenReader(pkg)
		if err != nil {
			return nil, fmt.Errorf("это не zip-архив программы")
		}
		defer zr.Close()
		// Find the folder inside the zip that holds our exe.
		root := ""
		found := false
		for _, zf := range zr.File {
			n := strings.ReplaceAll(zf.Name, "\\", "/")
			if strings.EqualFold(pathBase(n), selfExeName) || strings.EqualFold(pathBase(n), newExeName) {
				root, found = n[:len(n)-len(pathBase(n))], true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("в архиве нет «%s»", selfExeName)
		}
		for _, zf := range zr.File {
			n := strings.ReplaceAll(zf.Name, "\\", "/")
			if !strings.HasPrefix(n, root) || strings.HasSuffix(n, "/") {
				continue
			}
			rel := n[len(root):]
			if rel == "" || strings.Contains(rel, "..") || strings.Contains(rel, "/") {
				continue // top-level files only
			}
			low := strings.ToLower(rel)
			if strings.HasSuffix(low, ".json") {
				continue // never touch settings
			}
			if strings.HasSuffix(low, ".bat") || strings.HasSuffix(low, ".txt") || strings.HasSuffix(low, ".vbs") {
				if _, err := os.Stat(filepath.Join(appDir, rel)); err == nil && !strings.HasPrefix(low, "как ") {
					continue // keep the user's own scripts as they are
				}
			}
			if strings.EqualFold(rel, selfExeName) {
				rel = newExeName
			}
			if err := extractZipFile(zf, filepath.Join(stage, rel)); err != nil {
				return nil, fmt.Errorf("распаковка: %v", err)
			}
		}
	}

	// Everything that could hold a file open: scrcpy windows and the adb server.
	a.mu.Lock()
	var ids []string
	for id := range a.sessions {
		ids = append(ids, id)
	}
	a.mu.Unlock()
	for _, id := range ids {
		a.stopWait(id)
	}
	a.adb(8*time.Second, "kill-server")
	time.Sleep(800 * time.Millisecond)

	entries, _ := os.ReadDir(stage)
	var moved []string // files renamed to .old (for rollback)
	rollback := func() {
		for _, name := range moved {
			dst := filepath.Join(appDir, name)
			os.Remove(dst)
			os.Rename(dst+".old", dst)
		}
	}
	for _, e := range entries {
		name := e.Name()
		dst := filepath.Join(appDir, name)
		os.Remove(dst + ".old")
		if _, err := os.Stat(dst); err == nil {
			// Renaming works even for the running exe and for DLLs in use.
			if err := os.Rename(dst, dst+".old"); err != nil {
				rollback()
				a.startServer()
				return nil, fmt.Errorf("файл «%s» занят другой программой", name)
			}
			moved = append(moved, name)
		}
		if err := copyFile(filepath.Join(stage, name), dst); err != nil {
			rollback()
			a.startServer()
			return nil, fmt.Errorf("не удалось записать «%s»: %v", name, err)
		}
	}

	// Ask the new exe its version, then start it on our port/token.
	ver := ""
	if out, err := exec.Command(exe, "--version").Output(); err == nil {
		ver = strings.TrimSpace(string(out))
	}
	cmd := exec.Command(exe, "--after-update", fmt.Sprintf("--port=%d", port), "--token="+token)
	cmd.Dir = appDir
	if err := cmd.Start(); err != nil {
		rollback()
		a.startServer()
		return nil, fmt.Errorf("новая версия не запустилась: %v", err)
	}
	a.logf("✔ Программа обновлена%s, перезапускаю", map[bool]string{true: " до " + ver, false: ""}[ver != ""])
	return &updateResult{Version: ver}, nil
}

func pathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}
