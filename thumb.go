package main

// Small live thumbnails of the phone's screen for the cards (and the "dive" animation).

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os/exec"
	"sync"
	"time"
)

type thumbEntry struct {
	mu   sync.Mutex
	jpg  []byte
	w, h int
	at   time.Time
	dark bool
}

var thumbs sync.Map // device id -> *thumbEntry

// downscale with box filtering to the given width.
func downscale(src image.Image, outW int) *image.RGBA {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return image.NewRGBA(image.Rect(0, 0, 1, 1))
	}
	outH := sh * outW / sw
	if outH < 1 {
		outH = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, outW, outH))
	for y := 0; y < outH; y++ {
		y0, y1 := y*sh/outH, (y+1)*sh/outH
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < outW; x++ {
			x0, x1 := x*sw/outW, (x+1)*sw/outW
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, bl, n uint32
			// sample a 3x3 grid inside the box: fast and smooth enough
			for sy := 0; sy < 3; sy++ {
				py := y0 + (y1-y0)*sy/3
				for sx := 0; sx < 3; sx++ {
					px := x0 + (x1-x0)*sx/3
					cr, cg, cb, _ := src.At(b.Min.X+px, b.Min.Y+py).RGBA()
					r, g, bl, n = r+cr, g+cg, bl+cb, n+1
				}
			}
			dst.SetRGBA(x, y, color.RGBA{uint8(r / n >> 8), uint8(g / n >> 8), uint8(bl / n >> 8), 255})
		}
	}
	return dst
}

// thumbnail returns a cached (≤3 s old) JPEG of the phone screen, or nil if the
// screen is off/black or unavailable.
func (a *App) thumbnail(serial string) ([]byte, int, int) {
	a.mu.Lock()
	d := a.findDeviceLocked(serial)
	if d == nil || d.State != "device" {
		a.mu.Unlock()
		return nil, 0, 0
	}
	id, ser := d.ID, d.Serial
	a.mu.Unlock()
	v, _ := thumbs.LoadOrStore(id, &thumbEntry{})
	e := v.(*thumbEntry)
	e.mu.Lock() // one capture per phone at a time
	defer e.mu.Unlock()
	if time.Since(e.at) < 3*time.Second {
		if e.dark {
			return nil, 0, 0
		}
		return e.jpg, e.w, e.h
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.adbPath, "-s", ser, "exec-out", "screencap", "-p")
	cmd.Dir = a.toolDir
	hideWindow(cmd)
	cmd.WaitDelay = time.Second
	raw, _ := cmd.Output()
	e.at = time.Now()
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		e.dark, e.jpg = true, nil
		return nil, 0, 0
	}
	small := downscale(img, 270)
	// Screen off / secure content → black: keep the yellow placeholder instead.
	var sum uint64
	px := small.Pix
	for i := 0; i < len(px); i += 4 {
		sum += uint64(px[i]) + uint64(px[i+1]) + uint64(px[i+2])
	}
	if sum/uint64(len(px)/4*3) < 4 {
		e.dark, e.jpg = true, nil
		return nil, 0, 0
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, small, &jpeg.Options{Quality: 72})
	e.dark, e.jpg = false, buf.Bytes()
	e.w, e.h = img.Bounds().Dx(), img.Bounds().Dy()
	return e.jpg, e.w, e.h
}
