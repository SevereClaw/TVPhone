//go:build !windows

package main

import (
	"os"
	"sync"
)

var sfxLogMu sync.Mutex

// playWav: on non-Windows test builds, just record what would play.
func playWav(w []byte) {
	if f := os.Getenv("LAUNCHER_SFX_LOG"); f != "" {
		sfxLogMu.Lock()
		defer sfxLogMu.Unlock()
		for n, b := range sfxWavs {
			if &b[0] == &w[0] {
				fh, _ := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
				fh.WriteString(n + "\n")
				fh.Close()
			}
		}
	}
}

const nativeSfx = true // tests exercise the same path
