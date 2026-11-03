package main

// Generates a COFF .syso with an RT_GROUP_ICON (PNG-compressed icons) for Go's linker.

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
)

func lerp(a, b uint8, t float64) uint8 { return uint8(float64(a) + (float64(b)-float64(a))*t) }

// signed distance to rounded rect centered at (cx,cy) with half sizes hw,hh, radius r
func sdRR(x, y, cx, cy, hw, hh, r float64) float64 {
	qx := math.Abs(x-cx) - (hw - r)
	qy := math.Abs(y-cy) - (hh - r)
	ox, oy := math.Max(qx, 0), math.Max(qy, 0)
	return math.Hypot(ox, oy) + math.Min(math.Max(qx, qy), 0) - r
}

func icon(n int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, n, n))
	s := float64(n) / 256
	body := color.NRGBA{0x13, 0x6f, 0x6a, 255}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			px, py := (float64(x)+.5)/s, (float64(y)+.5)/s
			aa := 1.2 / s
			// phone body
			d := sdRR(px, py, 128, 128, 74, 120, 30)
			a := math.Min(math.Max(.5-d/aa, 0), 1)
			if a <= 0 {
				continue
			}
			c := body
			// screen
			ds := sdRR(px, py, 128, 132, 58, 98, 16)
			sa := math.Min(math.Max(.5-ds/aa, 0), 1)
			if sa > 0 {
				t := (py - 34) / 196
				sc := color.NRGBA{lerp(0xc4, 0x5f, t), lerp(0xf7, 0xd8, t), lerp(0xee, 0xc8, t), 255}
				// "mirror" arrow cue: small dark bar at bottom of screen
				c = color.NRGBA{lerp(c.R, sc.R, sa), lerp(c.G, sc.G, sa), lerp(c.B, sc.B, sa), 255}
			}
			// camera dot
			if math.Hypot(px-128, py-22) < 6 {
				c = color.NRGBA{0x08, 0x2b, 0x29, 255}
			}
			c.A = uint8(a * 255)
			img.SetNRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func main() {
	sizes := []int{256, 48, 32, 16}
	var icons [][]byte
	for _, n := range sizes {
		icons = append(icons, icon(n))
	}
	if len(os.Args) > 2 {
		os.WriteFile(os.Args[2], icons[0], 0644)
	}
	// Group icon
	var grp bytes.Buffer
	binary.Write(&grp, binary.LittleEndian, []uint16{0, 1, uint16(len(sizes))})
	for i, n := range sizes {
		w := uint8(n)
		if n >= 256 {
			w = 0
		}
		grp.Write([]byte{w, w, 0, 0})
		binary.Write(&grp, binary.LittleEndian, uint16(1))
		binary.Write(&grp, binary.LittleEndian, uint16(32))
		binary.Write(&grp, binary.LittleEndian, uint32(len(icons[i])))
		binary.Write(&grp, binary.LittleEndian, uint16(i+1))
	}

	// Resource tree layout
	type leaf struct{ data []byte }
	var sec bytes.Buffer
	le := binary.LittleEndian
	dir := func(nId int) {
		binary.Write(&sec, le, []uint32{0, 0})
		binary.Write(&sec, le, []uint16{4, 0, 0, uint16(nId)})
	}
	// Precompute offsets
	nIcons := len(icons)
	rootSz := 16 + 2*8
	l2IconOff := rootSz
	l2IconSz := 16 + nIcons*8
	l2GrpOff := l2IconOff + l2IconSz
	l2GrpSz := 16 + 8
	l3Off := l2GrpOff + l2GrpSz
	l3Sz := 16 + 8 // each language dir
	nLeaves := nIcons + 1
	dataEntOff := l3Off + nLeaves*l3Sz
	dataStart := dataEntOff + nLeaves*16
	// root
	dir(2)
	binary.Write(&sec, le, []uint32{3, 0x80000000 | uint32(l2IconOff)})
	binary.Write(&sec, le, []uint32{14, 0x80000000 | uint32(l2GrpOff)})
	dir(nIcons)
	for i := 0; i < nIcons; i++ {
		binary.Write(&sec, le, []uint32{uint32(i + 1), 0x80000000 | uint32(l3Off+i*l3Sz)})
	}
	dir(1)
	binary.Write(&sec, le, []uint32{1, 0x80000000 | uint32(l3Off+nIcons*l3Sz)})
	for i := 0; i < nLeaves; i++ {
		dir(1)
		binary.Write(&sec, le, []uint32{0x409, uint32(dataEntOff + i*16)})
	}
	leaves := append(append([][]byte{}, icons...), grp.Bytes())
	off := dataStart
	var relocs []uint32
	var offs []int
	for _, d := range leaves {
		off = (off + 7) &^ 7
		offs = append(offs, off)
		off += len(d)
	}
	for i, d := range leaves {
		relocs = append(relocs, uint32(sec.Len()))
		binary.Write(&sec, le, []uint32{uint32(offs[i]), uint32(len(d)), 0, 0})
	}
	for i, d := range leaves {
		for sec.Len() < offs[i] {
			sec.WriteByte(0)
		}
		sec.Write(d)
	}
	for sec.Len()%8 != 0 {
		sec.WriteByte(0)
	}

	// COFF
	var out bytes.Buffer
	hdrSz := 20 + 40
	rawPtr := hdrSz
	relPtr := rawPtr + sec.Len()
	symPtr := relPtr + len(relocs)*10
	binary.Write(&out, le, uint16(0x8664))
	binary.Write(&out, le, uint16(1))
	binary.Write(&out, le, uint32(0))
	binary.Write(&out, le, uint32(symPtr))
	binary.Write(&out, le, uint32(1))
	binary.Write(&out, le, uint16(0))
	binary.Write(&out, le, uint16(0))
	out.WriteString(".rsrc\x00\x00\x00")
	binary.Write(&out, le, []uint32{0, 0, uint32(sec.Len()), uint32(rawPtr), uint32(relPtr), 0})
	binary.Write(&out, le, []uint16{uint16(len(relocs)), 0})
	binary.Write(&out, le, uint32(0x40000040))
	out.Write(sec.Bytes())
	for _, r := range relocs {
		binary.Write(&out, le, r)
		binary.Write(&out, le, uint32(0))
		binary.Write(&out, le, uint16(3)) // IMAGE_REL_AMD64_ADDR32NB
	}
	out.WriteString(".rsrc\x00\x00\x00")
	binary.Write(&out, le, uint32(0))
	binary.Write(&out, le, int16(1))
	binary.Write(&out, le, uint16(0))
	out.WriteByte(3)
	out.WriteByte(0)
	binary.Write(&out, le, uint32(4))
	os.WriteFile(os.Args[1], out.Bytes(), 0644)
}
