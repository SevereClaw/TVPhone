package main

import (
	"syscall"
	"unsafe"
)

var procPlaySoundW = syscall.NewLazyDLL("winmm.dll").NewProc("PlaySoundW")

// playWav plays an in-memory WAV asynchronously (the buffer lives for the whole run).
func playWav(w []byte) {
	procPlaySoundW.Call(uintptr(unsafe.Pointer(&w[0])), 0, 0x0004|0x0001|0x0002) // SND_MEMORY|SND_ASYNC|SND_NODEFAULT
}

const nativeSfx = true
