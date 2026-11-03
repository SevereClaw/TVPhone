package main

// Interface sounds, synthesized in code (no audio files) and played natively, so they
// work everywhere: the phone window toolbar, hotkeys, background events and the UI.

import (
	"bytes"
	"encoding/binary"
	"math"
	"math/rand"
	"sync"
)

const sfxRate = 44100

type sfxBuf []float64

func newBuf(sec float64) sfxBuf { return make(sfxBuf, int(sec*sfxRate)) }

// expEnv: exponential attack/hold/release like WebAudio's exponentialRampToValueAtTime.
func expEnv(t, start, attack, hold, rel, peak float64) float64 {
	const floor = 0.0001
	x := t - start
	switch {
	case x < 0:
		return 0
	case x < attack:
		return floor * math.Pow(peak/floor, x/attack)
	case x < attack+hold:
		return peak
	case x < attack+hold+rel:
		return peak * math.Pow(floor/peak, (x-attack-hold)/rel)
	}
	return 0
}

type biquad struct{ b0, b1, b2, a1, a2, x1, x2, y1, y2 float64 }

func (f *biquad) set(kind string, freq, q float64) {
	w := 2 * math.Pi * freq / sfxRate
	cw, sw := math.Cos(w), math.Sin(w)
	alpha := sw / (2 * q)
	var b0, b1, b2, a0, a1, a2 float64
	switch kind {
	case "bandpass":
		b0, b1, b2 = alpha, 0, -alpha
	case "highpass":
		b0, b1, b2 = (1+cw)/2, -(1 + cw), (1+cw)/2
	default: // lowpass
		b0, b1, b2 = (1-cw)/2, 1-cw, (1-cw)/2
	}
	a0, a1, a2 = 1+alpha, -2*cw, 1-alpha
	f.b0, f.b1, f.b2, f.a1, f.a2 = b0/a0, b1/a0, b2/a0, a1/a0, a2/a0
}

func (f *biquad) run(x float64) float64 {
	y := f.b0*x + f.b1*f.x1 + f.b2*f.x2 - f.a1*f.y1 - f.a2*f.y2
	f.x2, f.x1, f.y2, f.y1 = f.x1, x, f.y1, y
	return y
}

func expSweep(f0, f1, t, dur float64) float64 {
	if t >= dur || f0 == f1 {
		if t >= dur {
			return f1
		}
		return f0
	}
	return f0 * math.Pow(f1/f0, t/dur)
}

// tone adds an oscillator with an exponential pitch sweep and a short-attack envelope.
func (b sfxBuf) tone(kind string, f0, f1, start, dur, peak float64) {
	phase := 0.0
	for i := int(start * sfxRate); i < len(b); i++ {
		t := float64(i)/sfxRate - start
		if t > dur+0.01 {
			break
		}
		phase += 2 * math.Pi * expSweep(f0, f1, t, dur) / sfxRate
		var v float64
		switch kind {
		case "square":
			v = 1
			if math.Sin(phase) < 0 {
				v = -1
			}
		case "triangle":
			v = 2 / math.Pi * math.Asin(math.Sin(phase))
		case "sawtooth":
			v = 2 * (phase/(2*math.Pi) - math.Floor(phase/(2*math.Pi)+0.5))
		default:
			v = math.Sin(phase)
		}
		b[i] += v * expEnv(t, 0, 0.005, 0, dur, peak)
	}
}

// noise adds filtered noise; freq(t) may sweep; gain(t) is the envelope.
func (b sfxBuf) noise(seed int64, kind string, q float64, freq func(t float64) float64, gain func(t float64) float64, start, dur float64) {
	r := rand.New(rand.NewSource(seed))
	var f biquad
	for i := int(start * sfxRate); i < len(b) && float64(i)/sfxRate < start+dur; i++ {
		t := float64(i)/sfxRate - start
		if i%32 == 0 {
			f.set(kind, freq(t), q)
		}
		b[i] += f.run(r.Float64()*2-1) * gain(t)
	}
}

func sfxHiss(b sfxBuf, start, d, vol float64) {
	b.noise(7, "bandpass", 0.6, func(float64) float64 { return 3200 },
		func(t float64) float64 { return expEnv(t, 0, 0.02, d*0.5, d*0.5, vol) }, start, d+0.05)
}

func synth(name string) sfxBuf {
	switch name {
	case "hiss":
		b := newBuf(0.6)
		sfxHiss(b, 0, 0.45, 0.14)
		return b
	case "whoosh", "whoosh_up", "whoosh_down":
		up := name != "whoosh_down"
		d := 0.8
		b := newBuf(d + 0.05)
		b.noise(11, "bandpass", 1.4, func(t float64) float64 {
			if up {
				return expSweep(250, 5000, t, d)
			}
			return expSweep(5000, 250, t, d)
		}, func(t float64) float64 {
			if t < d*0.7 {
				return 0.0001 * math.Pow(0.5/0.0001, t/(d*0.7))
			}
			return 0.5 * math.Pow(0.0001/0.5, (t-d*0.7)/(d*0.3))
		}, 0, d)
		return b
	case "chime":
		b := newBuf(1.1)
		for i, x := range [][2]float64{{1318.5, 0}, {1975.5, 0.045}, {2637, 0.09}, {3951, 0.13}} {
			b.tone("sine", x[0], x[0], x[1], 0.9-float64(i)*0.12, 0.16-float64(i)*0.025)
			b.tone("triangle", x[0]*2, x[0]*2, x[1], 0.25, 0.03)
		}
		return b
	case "crt":
		b := newBuf(0.8)
		b.noise(3, "highpass", 0.7, func(float64) float64 { return 2000 },
			func(t float64) float64 { return expEnv(t, 0, 0.001, 0.008, 0.03, 0.12) }, 0, 0.06)
		b.tone("sine", 110, 38, 0, 0.32, 0.26)
		b.tone("sine", 9500, 9500, 0.03, 0.7, 0.018)
		return b
	case "thump":
		b := newBuf(0.35)
		b.tone("sine", 140, 45, 0, 0.28, 0.4)
		b.tone("square", 70, 40, 0, 0.12, 0.05)
		return b
	case "tick":
		b := newBuf(0.06)
		b.tone("square", 1400, 1100, 0, 0.035, 0.05)
		return b
	case "pop":
		b := newBuf(0.12)
		b.tone("sine", 520, 980, 0, 0.09, 0.12)
		return b
	case "lost":
		b := newBuf(1.0)
		sfxHiss(b, 0, 0.9, 0.26)
		b.tone("sawtooth", 62, 55, 0, 0.7, 0.05)
		return b
	case "shutter": // camera shutter: two clicks and a soft body
		b := newBuf(0.25)
		for _, at := range []float64{0, 0.07} {
			at := at
			b.noise(int64(at*1000)+5, "highpass", 0.8, func(float64) float64 { return 2500 },
				func(t float64) float64 { return expEnv(t, 0, 0.001, 0.006, 0.04, 0.2) }, at, 0.06)
		}
		b.tone("sine", 220, 110, 0, 0.09, 0.12)
		return b
	case "rec_start":
		b := newBuf(0.35)
		b.tone("sine", 660, 660, 0, 0.09, 0.18)
		b.tone("sine", 990, 990, 0.12, 0.16, 0.18)
		return b
	case "rec_stop":
		b := newBuf(0.35)
		b.tone("sine", 990, 990, 0, 0.09, 0.18)
		b.tone("sine", 660, 660, 0.12, 0.16, 0.18)
		return b
	case "notify":
		b := newBuf(0.45)
		b.tone("triangle", 880, 880, 0, 0.12, 0.14)
		b.tone("triangle", 1320, 1320, 0.11, 0.25, 0.12)
		return b
	}
	return nil
}

var sfxNames = []string{"hiss", "whoosh_up", "whoosh_down", "chime", "crt", "thump", "tick", "pop", "lost", "shutter", "rec_start", "rec_stop", "notify"}

// wav encodes the sound as 16-bit mono PCM, with the master gain of the old UI version.
func (b sfxBuf) wav() []byte {
	var pcm bytes.Buffer
	for _, v := range b {
		v *= 0.55 * 1.6
		if v > 1 {
			v = 1
		} else if v < -1 {
			v = -1
		}
		binary.Write(&pcm, binary.LittleEndian, int16(v*32767))
	}
	var w bytes.Buffer
	w.WriteString("RIFF")
	binary.Write(&w, binary.LittleEndian, uint32(36+pcm.Len()))
	w.WriteString("WAVEfmt ")
	binary.Write(&w, binary.LittleEndian, []uint32{16})
	binary.Write(&w, binary.LittleEndian, []uint16{1, 1})
	binary.Write(&w, binary.LittleEndian, []uint32{sfxRate, sfxRate * 2})
	binary.Write(&w, binary.LittleEndian, []uint16{2, 16})
	w.WriteString("data")
	binary.Write(&w, binary.LittleEndian, uint32(pcm.Len()))
	w.Write(pcm.Bytes())
	return w.Bytes()
}

var (
	sfxOnce  sync.Once
	sfxWavs  map[string][]byte
	sfxAlias = map[string]string{"whoosh": "whoosh_up"}
)

func sfxWav(name string) []byte {
	sfxOnce.Do(func() {
		sfxWavs = map[string][]byte{}
		for _, n := range sfxNames {
			sfxWavs[n] = synth(n).wav()
		}
	})
	if a, ok := sfxAlias[name]; ok {
		name = a
	}
	return sfxWavs[name]
}

// sfx plays an interface sound if sounds are on.
func (a *App) sfx(name string) {
	a.mu.Lock()
	on := a.cfg.Settings.UISounds
	a.mu.Unlock()
	if !on {
		return
	}
	if w := sfxWav(name); w != nil {
		playWav(w)
	}
}
