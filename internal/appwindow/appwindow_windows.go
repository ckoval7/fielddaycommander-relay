// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package appwindow

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

const (
	// edgeWindowClass is the class of Edge's windows. Its ordinary windows add the
	// browser's name to their titles, so only an app window matches the page's title.
	edgeWindowClass = "Chrome_WidgetWin_1"
	swRestore       = 9
)

var (
	user32              = syscall.NewLazyDLL("user32.dll")
	findWindow          = user32.NewProc("FindWindowW")
	isIconic            = user32.NewProc("IsIconic")
	showWindow          = user32.NewProc("ShowWindow")
	setForegroundWindow = user32.NewProc("SetForegroundWindow")
)

// Show brings forward the window titled title, or opens url in a new one. It returns
// ErrUnavailable if there's no window and no Edge to make one with.
func Show(url, title string) error {
	raised, err := raise(title)
	if err != nil {
		return err
	}
	if raised {
		return nil
	}
	edge, err := findEdge()
	if err != nil {
		return err
	}
	command := exec.Command(edge, "--app="+url)
	if err := command.Start(); err != nil {
		return fmt.Errorf("start Edge: %w", err)
	}
	// Edge may hand over to a copy already running and exit at once.
	go func() { _ = command.Wait() }()
	return nil
}

// raise brings the window forward if it's there. Windows may refuse if the relay
// isn't what the person just used, and flashes the taskbar button instead.
func raise(title string) (bool, error) {
	class, err := syscall.UTF16PtrFromString(edgeWindowClass)
	if err != nil {
		return false, err
	}
	name, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return false, err
	}
	window, _, _ := findWindow.Call(uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(name)))
	if window == 0 {
		return false, nil
	}
	if minimized, _, _ := isIconic.Call(window); minimized != 0 {
		_, _, _ = showWindow.Call(window, swRestore)
	}
	_, _, _ = setForegroundWindow.Call(window)
	return true, nil
}

func findEdge() (string, error) {
	for _, path := range edgeCandidates(os.Getenv) {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	return "", ErrUnavailable
}
