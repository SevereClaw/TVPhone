package main

// Local gallery: your screenshots, recordings and photos taken from the phone.

import (
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type GalleryItem struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Kind string `json:"kind"` // shot | video | phone
	Size int64  `json:"size"`
	Date int64  `json:"date"`
}

var galleryExt = map[string]string{".png": "img", ".jpg": "img", ".jpeg": "img", ".webp": "img", ".gif": "img", ".heic": "img",
	".mp4": "video", ".mkv": "video", ".mov": "video", ".webm": "video", ".3gp": "video"}

func (a *App) gallery() []GalleryItem {
	a.mu.Lock()
	dirs := []struct{ dir, kind string }{{a.shotDirLocked(), "shot"}, {a.videoDirLocked(), "video"}, {a.photoDirLocked(), "phone"}}
	a.mu.Unlock()
	var res []GalleryItem
	seen := map[string]bool{}
	for _, d := range dirs {
		entries, _ := os.ReadDir(d.dir)
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			ext := strings.ToLower(filepath.Ext(e.Name()))
			if galleryExt[ext] == "" {
				continue
			}
			p := filepath.Join(d.dir, e.Name())
			if seen[strings.ToLower(p)] {
				continue
			}
			seen[strings.ToLower(p)] = true
			fi, err := e.Info()
			if err != nil {
				continue
			}
			res = append(res, GalleryItem{Path: p, Name: e.Name(), Kind: d.kind, Size: fi.Size(), Date: fi.ModTime().Unix()})
		}
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Date > res[j].Date })
	if len(res) > 400 {
		res = res[:400]
	}
	return res
}

func serveGalleryFile(w http.ResponseWriter, r *http.Request, p string) {
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	defer f.Close()
	fi, _ := f.Stat()
	ct := map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp", ".gif": "image/gif",
		".mp4": "video/mp4", ".webm": "video/webm", ".mkv": "video/x-matroska", ".mov": "video/quicktime", ".heic": "image/heic", ".3gp": "video/3gpp"}[strings.ToLower(filepath.Ext(p))]
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f) // supports seeking in videos
}
