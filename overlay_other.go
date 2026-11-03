//go:build !windows

package main

import "time"

func overlayAttach(a *App, id, serial string, pid int) {}
func overlayDetach(id string)                          {}
func overlayDetachAll()                                {}
func focusPid(pid int)                                 {}
func beep(ok bool)                                     {}
func tbDone(id string, btn int, text string, ok bool)  {}
func (a *App) applyHotkeys()                           {}

const (
	btnShot = iota
	btnRec
)

func revealWindow(pid int, pl *Place, revealAt time.Time, timeout time.Duration) bool {
	time.Sleep(900 * time.Millisecond) // tests: pretend the window appeared
	if w := time.Until(revealAt); w > 0 {
		time.Sleep(w)
	}
	return true
}

func placementArgs(pl *Place) []string { return nil }

func callPopupShow(info CallInfo, devName string, act func(string)) {}
func callPopupHide()                                                {}

func hideWindowOfPid(pid int, timeout time.Duration) {}
