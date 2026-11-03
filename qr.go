package main

// Minimal QR Code encoder (byte mode, ECC level M, versions 1–10).
// Enough for "WIFI:T:ADB;S:...;P:...;;" strings used by Android wireless pairing.

import (
	"errors"
	"fmt"
	"strings"
)

type qrVersion struct {
	ecPerBlock int
	g1Blocks   int
	g1Data     int
	g2Blocks   int
	g2Data     int
	align      []int
}

// ECC level M.
var qrVersionsM = []qrVersion{
	{},
	{10, 1, 16, 0, 0, nil},
	{16, 1, 28, 0, 0, []int{6, 18}},
	{26, 1, 44, 0, 0, []int{6, 22}},
	{18, 2, 32, 0, 0, []int{6, 26}},
	{24, 2, 43, 0, 0, []int{6, 30}},
	{16, 4, 27, 0, 0, []int{6, 34}},
	{18, 4, 31, 0, 0, []int{6, 22, 38}},
	{22, 2, 38, 2, 39, []int{6, 24, 42}},
	{22, 3, 36, 2, 37, []int{6, 26, 46}},
	{26, 4, 43, 1, 44, []int{6, 28, 50}},
}

type qrCode struct {
	size    int
	mods    [][]bool
	isFunc  [][]bool
	version int
}

func gfMul(x, y int) int {
	z := 0
	for i := 7; i >= 0; i-- {
		z = (z << 1) ^ ((z >> 7) * 0x11D)
		z ^= ((y >> uint(i)) & 1) * x
	}
	return z & 0xFF
}

func rsDivisor(degree int) []int {
	res := make([]int, degree)
	res[degree-1] = 1
	root := 1
	for i := 0; i < degree; i++ {
		for j := 0; j < degree; j++ {
			res[j] = gfMul(res[j], root)
			if j+1 < degree {
				res[j] ^= res[j+1]
			}
		}
		root = gfMul(root, 0x02)
	}
	return res
}

func rsRemainder(data []byte, div []int) []byte {
	res := make([]int, len(div))
	for _, b := range data {
		factor := int(b) ^ res[0]
		copy(res, res[1:])
		res[len(res)-1] = 0
		for i := range res {
			res[i] ^= gfMul(div[i], factor)
		}
	}
	out := make([]byte, len(res))
	for i, v := range res {
		out[i] = byte(v)
	}
	return out
}

func EncodeQR(text string) (*qrCode, error) {
	data := []byte(text)
	ver := 0
	for v := 1; v <= 10; v++ {
		vi := qrVersionsM[v]
		capBits := (vi.g1Blocks*vi.g1Data + vi.g2Blocks*vi.g2Data) * 8
		ccBits := 8
		if v >= 10 {
			ccBits = 16
		}
		if 4+ccBits+len(data)*8 <= capBits {
			ver = v
			break
		}
	}
	if ver == 0 {
		return nil, errors.New("text too long for QR")
	}
	vi := qrVersionsM[ver]
	dataCap := vi.g1Blocks*vi.g1Data + vi.g2Blocks*vi.g2Data

	// Bit stream.
	var bits []bool
	put := func(val, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, (val>>uint(i))&1 == 1)
		}
	}
	put(0x4, 4)
	if ver >= 10 {
		put(len(data), 16)
	} else {
		put(len(data), 8)
	}
	for _, b := range data {
		put(int(b), 8)
	}
	capBits := dataCap * 8
	term := capBits - len(bits)
	if term > 4 {
		term = 4
	}
	put(0, term)
	for len(bits)%8 != 0 {
		bits = append(bits, false)
	}
	cw := make([]byte, 0, dataCap)
	for i := 0; i < len(bits); i += 8 {
		var b byte
		for j := 0; j < 8; j++ {
			if bits[i+j] {
				b |= 1 << uint(7-j)
			}
		}
		cw = append(cw, b)
	}
	for pad := byte(0xEC); len(cw) < dataCap; {
		cw = append(cw, pad)
		pad ^= 0xEC ^ 0x11
	}

	// Blocks + EC.
	div := rsDivisor(vi.ecPerBlock)
	var dBlocks, eBlocks [][]byte
	off := 0
	for g := 0; g < 2; g++ {
		n, l := vi.g1Blocks, vi.g1Data
		if g == 1 {
			n, l = vi.g2Blocks, vi.g2Data
		}
		for i := 0; i < n; i++ {
			blk := cw[off : off+l]
			off += l
			dBlocks = append(dBlocks, blk)
			eBlocks = append(eBlocks, rsRemainder(blk, div))
		}
	}
	var final []byte
	maxD := vi.g1Data
	if vi.g2Data > maxD {
		maxD = vi.g2Data
	}
	for i := 0; i < maxD; i++ {
		for _, b := range dBlocks {
			if i < len(b) {
				final = append(final, b[i])
			}
		}
	}
	for i := 0; i < vi.ecPerBlock; i++ {
		for _, b := range eBlocks {
			final = append(final, b[i])
		}
	}

	q := &qrCode{size: ver*4 + 17, version: ver}
	q.mods = make([][]bool, q.size)
	q.isFunc = make([][]bool, q.size)
	for i := range q.mods {
		q.mods[i] = make([]bool, q.size)
		q.isFunc[i] = make([]bool, q.size)
	}
	q.drawFunctionPatterns()
	q.drawCodewords(final)

	best, bestPen := 0, -1
	for m := 0; m < 8; m++ {
		q.applyMask(m)
		q.drawFormat(m)
		p := q.penalty()
		if bestPen < 0 || p < bestPen {
			best, bestPen = m, p
		}
		q.applyMask(m) // undo (XOR)
	}
	q.applyMask(best)
	q.drawFormat(best)
	return q, nil
}

func (q *qrCode) set(x, y int, dark bool) {
	q.mods[y][x] = dark
	q.isFunc[y][x] = true
}

func abs(a int) int {
	if a < 0 {
		return -a
	}
	return a
}

func (q *qrCode) drawFunctionPatterns() {
	n := q.size
	for i := 0; i < n; i++ {
		q.set(6, i, i%2 == 0)
		q.set(i, 6, i%2 == 0)
	}
	finder := func(x, y int) {
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				d := abs(dx)
				if abs(dy) > d {
					d = abs(dy)
				}
				xx, yy := x+dx, y+dy
				if xx >= 0 && xx < n && yy >= 0 && yy < n {
					q.set(xx, yy, d != 2 && d != 4)
				}
			}
		}
	}
	finder(3, 3)
	finder(n-4, 3)
	finder(3, n-4)
	al := qrVersionsM[q.version].align
	for i := range al {
		for j := range al {
			last := len(al) - 1
			if (i == 0 && j == 0) || (i == 0 && j == last) || (i == last && j == 0) {
				continue
			}
			for dy := -2; dy <= 2; dy++ {
				for dx := -2; dx <= 2; dx++ {
					d := abs(dx)
					if abs(dy) > d {
						d = abs(dy)
					}
					q.set(al[i]+dx, al[j]+dy, d != 1)
				}
			}
		}
	}
	q.drawFormat(0) // reserve
	if q.version >= 7 {
		rem := q.version
		for i := 0; i < 12; i++ {
			rem = (rem << 1) ^ ((rem >> 11) * 0x1F25)
		}
		bits := q.version<<12 | rem
		for i := 0; i < 18; i++ {
			bit := (bits>>uint(i))&1 == 1
			a := n - 11 + i%3
			b := i / 3
			q.set(a, b, bit)
			q.set(b, a, bit)
		}
	}
}

func (q *qrCode) drawFormat(mask int) {
	data := 0<<3 | mask // ECC M = 00
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return (bits>>uint(i))&1 == 1 }
	n := q.size
	for i := 0; i <= 5; i++ {
		q.set(8, i, bit(i))
	}
	q.set(8, 7, bit(6))
	q.set(8, 8, bit(7))
	q.set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		q.set(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		q.set(n-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		q.set(8, n-15+i, bit(i))
	}
	q.set(8, n-8, true)
}

func (q *qrCode) drawCodewords(data []byte) {
	n := q.size
	i := 0
	for right := n - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < n; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				upward := ((right + 1) & 2) == 0
				y := vert
				if upward {
					y = n - 1 - vert
				}
				if !q.isFunc[y][x] && i < len(data)*8 {
					q.mods[y][x] = (data[i>>3]>>uint(7-(i&7)))&1 == 1
					i++
				}
			}
		}
	}
}

func (q *qrCode) applyMask(m int) {
	for y := 0; y < q.size; y++ {
		for x := 0; x < q.size; x++ {
			if q.isFunc[y][x] {
				continue
			}
			var inv bool
			switch m {
			case 0:
				inv = (x+y)%2 == 0
			case 1:
				inv = y%2 == 0
			case 2:
				inv = x%3 == 0
			case 3:
				inv = (x+y)%3 == 0
			case 4:
				inv = (x/3+y/2)%2 == 0
			case 5:
				inv = x*y%2+x*y%3 == 0
			case 6:
				inv = (x*y%2+x*y%3)%2 == 0
			case 7:
				inv = ((x+y)%2+x*y%3)%2 == 0
			}
			if inv {
				q.mods[y][x] = !q.mods[y][x]
			}
		}
	}
}

// Simplified penalty (rules 1, 2, 4 and a basic rule 3).
func (q *qrCode) penalty() int {
	n := q.size
	p := 0
	get := func(x, y int, horiz bool) bool {
		if horiz {
			return q.mods[y][x]
		}
		return q.mods[x][y]
	}
	for _, horiz := range []bool{true, false} {
		for a := 0; a < n; a++ {
			run := 1
			for b := 1; b < n; b++ {
				if get(b, a, horiz) == get(b-1, a, horiz) {
					run++
					if run == 5 {
						p += 3
					} else if run > 5 {
						p++
					}
				} else {
					run = 1
				}
			}
			for b := 0; b+6 < n; b++ {
				if get(b, a, horiz) && !get(b+1, a, horiz) && get(b+2, a, horiz) && get(b+3, a, horiz) &&
					get(b+4, a, horiz) && !get(b+5, a, horiz) && get(b+6, a, horiz) {
					p += 40
				}
			}
		}
	}
	dark := 0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if q.mods[y][x] {
				dark++
			}
			if x+1 < n && y+1 < n {
				c := q.mods[y][x]
				if q.mods[y][x+1] == c && q.mods[y+1][x] == c && q.mods[y+1][x+1] == c {
					p += 3
				}
			}
		}
	}
	total := n * n
	k := (abs(dark*20-total*10) + total - 1) / total
	return p + (k-1)*10
}

func (q *qrCode) SVG(border int) string {
	dim := q.size + border*2
	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, dim, dim)
	fmt.Fprintf(&sb, `<rect width="100%%" height="100%%" fill="#fff"/><path fill="#000" d="`)
	for y := 0; y < q.size; y++ {
		for x := 0; x < q.size; x++ {
			if q.mods[y][x] {
				fmt.Fprintf(&sb, "M%d,%dh1v1h-1z", x+border, y+border)
			}
		}
	}
	sb.WriteString(`"/></svg>`)
	return sb.String()
}
