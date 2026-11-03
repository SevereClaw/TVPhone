package main

// "Photos from the phone": recent pictures/videos via MediaStore, tiny thumbnails
// (EXIF preview from the first 64 KB of a JPEG), and one-click download to the PC.

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type PhoneMedia struct {
	Path  string `json:"path"`
	Name  string `json:"name"`
	Mime  string `json:"mime"`
	Size  int64  `json:"size"`
	Date  int64  `json:"date"`
	Kind  string `json:"kind"`  // photo | video | screenshot
	Local bool   `json:"local"` // already on the PC
}

var reMediaRow = regexp.MustCompile(`_data=(.*?), date_added=(\d+), mime_type=(\S*), _size=(\d+)\s*$`)

func parseMediaRows(out string) []PhoneMedia {
	var res []PhoneMedia
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		m := reMediaRow.FindStringSubmatch(line)
		if m == nil || !strings.HasPrefix(m[1], "/storage/") {
			continue
		}
		size, _ := strconv.ParseInt(m[4], 10, 64)
		date, _ := strconv.ParseInt(m[2], 10, 64)
		pm := PhoneMedia{Path: m[1], Name: path.Base(m[1]), Mime: m[3], Size: size, Date: date}
		low := strings.ToLower(m[1])
		switch {
		case strings.HasPrefix(m[3], "video/"):
			pm.Kind = "video"
		case strings.Contains(low, "screenshot"):
			pm.Kind = "screenshot"
		default:
			pm.Kind = "photo"
		}
		res = append(res, pm)
	}
	return res
}

func parseLsMedia(out string) []PhoneMedia {
	// fallback: `ls -l --full-time`-less toybox: "stat -c '%n|%s|%Y'" output
	var res []PhoneMedia
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		f := strings.Split(strings.TrimSpace(line), "|")
		if len(f) != 3 || !strings.HasPrefix(f[0], "/") {
			continue
		}
		size, _ := strconv.ParseInt(f[1], 10, 64)
		date, _ := strconv.ParseInt(f[2], 10, 64)
		low := strings.ToLower(f[0])
		pm := PhoneMedia{Path: f[0], Name: path.Base(f[0]), Size: size, Date: date, Kind: "photo", Mime: "image/jpeg"}
		switch {
		case strings.HasSuffix(low, ".mp4") || strings.HasSuffix(low, ".mov") || strings.HasSuffix(low, ".3gp") || strings.HasSuffix(low, ".mkv"):
			pm.Kind, pm.Mime = "video", "video/mp4"
		case strings.Contains(low, "screenshot"):
			pm.Kind = "screenshot"
		}
		if strings.HasSuffix(low, ".png") {
			pm.Mime = "image/png"
		}
		res = append(res, pm)
	}
	return res
}

func (a *App) photoDirLocked() string {
	if d := strings.TrimSpace(a.cfg.Settings.PhotoDir); d != "" {
		return d
	}
	return filepath.Join(knownFolder("pictures"), "Телефон на ПК", "С телефона")
}

func (a *App) listPhoneMedia(serial string) ([]PhoneMedia, error) {
	a.mu.Lock()
	d := a.findDeviceLocked(serial)
	if d == nil || d.State != "device" {
		a.mu.Unlock()
		return nil, fmt.Errorf("телефон не найден")
	}
	ser := d.Serial
	dir := a.photoDirLocked()
	a.mu.Unlock()
	q := `content query --uri content://media/external/file --projection _data:date_added:mime_type:_size ` +
		`--where "media_type=1 OR media_type=3" --sort "date_added DESC"`
	out, _ := a.adb(20*time.Second, "-s", ser, "shell", q)
	items := parseMediaRows(out)
	if len(items) == 0 {
		// Older/locked-down phones: scan the usual folders.
		cmd := `for d in /sdcard/DCIM/Camera /sdcard/DCIM/Screenshots /sdcard/Pictures/Screenshots /sdcard/Pictures /sdcard/Movies; do ` +
			`[ -d "$d" ] && find "$d" -maxdepth 1 -type f \( -iname '*.jpg' -o -iname '*.jpeg' -o -iname '*.png' -o -iname '*.heic' -o -iname '*.mp4' -o -iname '*.mov' \) -exec stat -c '%n|%s|%Y' {} +; done`
		out, _ = a.adb(20*time.Second, "-s", ser, "shell", cmd)
		items = parseLsMedia(out)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Date > items[j].Date })
	if len(items) > 240 {
		items = items[:240]
	}
	for i := range items {
		if fi, err := os.Stat(filepath.Join(dir, items[i].Name)); err == nil && fi.Size() == items[i].Size {
			items[i].Local = true
		}
	}
	return items, nil
}

// ---------------------------------------------------------------- thumbnails

var phoneThumbs sync.Map // serial|path -> []byte (jpeg)

// exifThumb finds the embedded JPEG preview inside the EXIF block of a JPEG header.
func exifThumb(head []byte) []byte {
	if len(head) < 4 || head[0] != 0xFF || head[1] != 0xD8 {
		return nil
	}
	// The preview is a complete JPEG (FFD8…FFD9) nested inside APP1.
	for i := 2; i+1 < len(head); i++ {
		if head[i] == 0xFF && head[i+1] == 0xD8 {
			if end := bytes.Index(head[i:], []byte{0xFF, 0xD9}); end > 0 {
				return head[i : i+end+2]
			}
			return nil
		}
		if head[i] == 0xFF && head[i+1] == 0xDA { // start of the main image: no preview
			return nil
		}
	}
	return nil
}

func shellPath(p string) string { return shQuote(p) }

func (a *App) phoneThumb(serial, p string) ([]byte, error) {
	if !strings.HasPrefix(p, "/storage/") && !strings.HasPrefix(p, "/sdcard/") {
		return nil, fmt.Errorf("bad path")
	}
	key := serial + "|" + p
	if v, ok := phoneThumbs.Load(key); ok {
		return v.([]byte), nil
	}
	low := strings.ToLower(p)
	isJpeg := strings.HasSuffix(low, ".jpg") || strings.HasSuffix(low, ".jpeg")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	if isJpeg {
		cmd = exec.CommandContext(ctx, a.adbPath, "-s", serial, "exec-out", "head -c 98304 "+shellPath(p))
	} else {
		cmd = exec.CommandContext(ctx, a.adbPath, "-s", serial, "exec-out", "cat "+shellPath(p))
	}
	cmd.Dir = a.toolDir
	hideWindow(cmd)
	cmd.WaitDelay = time.Second
	data, err := cmd.Output()
	if err != nil && len(data) == 0 {
		return nil, err
	}
	var thumb []byte
	if isJpeg {
		thumb = exifThumb(data)
		if thumb == nil {
			// no preview: fetch the whole photo once and shrink it
			ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel2()
			c2 := exec.CommandContext(ctx2, a.adbPath, "-s", serial, "exec-out", "cat "+shellPath(p))
			c2.Dir = a.toolDir
			hideWindow(c2)
			c2.WaitDelay = time.Second
			if full, err := c2.Output(); err == nil {
				data = full
			}
		}
	}
	if thumb == nil {
		img, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("не картинка")
		}
		var buf bytes.Buffer
		jpeg.Encode(&buf, downscale(img, 240), &jpeg.Options{Quality: 75})
		thumb = buf.Bytes()
	}
	phoneThumbs.Store(key, thumb)
	return thumb, nil
}

// ---------------------------------------------------------------- download

type pullJob struct {
	Active  bool   `json:"active"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Current string `json:"current"`
	Error   string `json:"error"`
	Dir     string `json:"dir"`
	Last    string `json:"last"`
}

func (a *App) pullMedia(serial string, paths []string) error {
	a.mu.Lock()
	if a.pull.Active {
		a.mu.Unlock()
		return fmt.Errorf("уже загружаю")
	}
	d := a.findDeviceLocked(serial)
	if d == nil || d.State != "device" {
		a.mu.Unlock()
		return fmt.Errorf("телефон не найден")
	}
	ser := d.Serial
	dir := a.photoDirLocked()
	a.pull = pullJob{Active: true, Total: len(paths), Dir: dir}
	a.mu.Unlock()
	if err := os.MkdirAll(dir, 0755); err != nil {
		a.mu.Lock()
		a.pull.Active, a.pull.Error = false, err.Error()
		a.mu.Unlock()
		return err
	}
	go func() {
		ok := 0
		var last string
		for i, p := range paths {
			if !strings.HasPrefix(p, "/storage/") && !strings.HasPrefix(p, "/sdcard/") {
				continue
			}
			name := safeFileName(path.Base(p))
			a.mu.Lock()
			a.pull.Current, a.pull.Done = name, i
			a.mu.Unlock()
			dst := uniquePath(filepath.Join(dir, name))
			out, err := a.adb(30*time.Minute, "-s", ser, "pull", "-a", p, dst)
			if err != nil {
				a.logf("Не скачалось %s: %s", name, lastLine(out))
				continue
			}
			ok++
			last = dst
		}
		a.mu.Lock()
		a.pull.Active, a.pull.Done, a.pull.Current, a.pull.Last = false, len(paths), "", last
		a.mu.Unlock()
		a.logf("⬇ С телефона скачано файлов: %d → %s", ok, dir)
		if ok > 0 {
			a.event("video", last, fmt.Sprintf("Скачано с телефона: %d", ok))
		} else {
			a.event("error", "", "Не удалось скачать файлы с телефона")
		}
	}()
	return nil
}
