//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
)

const exeSuffix = ""

func hideWindow(cmd *exec.Cmd) {}

func openAppWindow(url, profileDir string) {
	fmt.Fprintln(os.Stderr, "UI:", url)
	if os.Getenv("LAUNCHER_NO_BROWSER") == "" {
		exec.Command("xdg-open", url).Start()
	}
}
