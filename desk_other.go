//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

func gracefulStop(cmd *exec.Cmd) { cmd.Process.Signal(syscall.SIGTERM) }

func knownFolder(kind string) string {
	home, _ := os.UserHomeDir()
	if d := os.Getenv("LAUNCHER_HOME"); d != "" {
		home = d
	}
	if kind == "videos" {
		return filepath.Join(home, "Videos")
	}
	return filepath.Join(home, "Pictures")
}

func openPath(p string, reveal bool) {
	if f := os.Getenv("LAUNCHER_OPEN_LOG"); f != "" {
		fh, _ := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		fh.WriteString(map[bool]string{true: "reveal ", false: "open "}[reveal] + p + "\n")
		fh.Close()
	}
}

func trashFile(p string) error           { return os.Remove(p) }
func copyFileToClipboard(p string) error { return nil }
