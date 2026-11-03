//go:build !windows

package main

import "errors"

type TrayItem struct {
	Text      string
	Checked   bool
	Disabled  bool
	Separator bool
	Action    func()
}

func startTray(tip string, onOpen func(), menu func() []TrayItem) (bool, chan struct{}) {
	return false, nil
}
func trayNotify(title, text string)          {}
func traySetTip(text string)                 {}
func trayQuit()                              {}
func focusAppWindow(titlePrefix string) bool { return false }
func allowOtherForeground()                  {}
func autostartSupported() bool               { return false }
func autostartEnabled() bool                 { return false }
func setAutostart(on bool) error             { return errors.New("только для Windows") }
func shortcutsSupported() bool               { return false }
func createShortcut(where string) (string, error) {
	return "", errors.New("только для Windows")
}
func killOtherInstances()                             {}
func fixAutostartPath()                               {}
func trayNotifyAction(title, text string, act func()) {}
func foregroundPid() int                              { return 0 }
