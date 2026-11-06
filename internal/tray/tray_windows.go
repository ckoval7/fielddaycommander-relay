// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package tray

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

const (
	wmDestroy     = 0x0002
	wmClose       = 0x0010
	wmNull        = 0x0000
	wmApp         = 0x8000
	wmLButtonUp   = 0x0202
	wmRButtonUp   = 0x0205
	wmTrayMessage = wmApp + 1

	nimAdd     = 0
	nimDelete  = 2
	nifMessage = 0x1
	nifIcon    = 0x2
	nifTip     = 0x4

	mfString       = 0x0
	tpmReturnCmd   = 0x0100
	tpmRightButton = 0x0002
	idiApplication = 32512
	smCXSmallIcon  = 49
	smCYSmallIcon  = 50
	imageIcon      = 1

	menuOpen = 1
	menuQuit = 2
)

var (
	user32  = syscall.NewLazyDLL("user32.dll")
	shell32 = syscall.NewLazyDLL("shell32.dll")
	kernel  = syscall.NewLazyDLL("kernel32.dll")

	registerClassEx       = user32.NewProc("RegisterClassExW")
	unregisterClass       = user32.NewProc("UnregisterClassW")
	createWindowEx        = user32.NewProc("CreateWindowExW")
	defWindowProc         = user32.NewProc("DefWindowProcW")
	destroyWindow         = user32.NewProc("DestroyWindow")
	getMessage            = user32.NewProc("GetMessageW")
	translateMessage      = user32.NewProc("TranslateMessage")
	dispatchMessage       = user32.NewProc("DispatchMessageW")
	postMessage           = user32.NewProc("PostMessageW")
	postQuitMessage       = user32.NewProc("PostQuitMessage")
	loadIcon              = user32.NewProc("LoadIconW")
	loadImage             = user32.NewProc("LoadImageW")
	getSystemMetrics      = user32.NewProc("GetSystemMetrics")
	createPopupMenu       = user32.NewProc("CreatePopupMenu")
	appendMenu            = user32.NewProc("AppendMenuW")
	destroyMenu           = user32.NewProc("DestroyMenu")
	trackPopupMenu        = user32.NewProc("TrackPopupMenu")
	getCursorPos          = user32.NewProc("GetCursorPos")
	setForegroundWindow   = user32.NewProc("SetForegroundWindow")
	registerWindowMessage = user32.NewProc("RegisterWindowMessageW")
	shellNotifyIcon       = shell32.NewProc("Shell_NotifyIconW")
	getModuleHandle       = kernel.NewProc("GetModuleHandleW")
)

type point struct{ X, Y int32 }

type message struct {
	Window   uintptr
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Point    point
	lPrivate uint32
}

type windowClass struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClassExtra int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSmall  uintptr
}

type notifyIconData struct {
	Size            uint32
	Window          uintptr
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            uintptr
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GUID            [16]byte
	BalloonIcon     uintptr
}

// icon holds the state the window procedure needs. Windows calls that procedure
// with no way to pass Go values, and the relay has one tray icon.
type icon struct {
	options   Options
	window    uintptr
	data      notifyIconData
	restarted uintptr
}

var (
	currentMu sync.Mutex
	current   *icon
)

// Run shows the icon and blocks until ctx is canceled or the person quits.
func Run(ctx context.Context, options Options) error {
	// A window belongs to the thread that made it, and its messages arrive there.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	currentMu.Lock()
	if current != nil {
		currentMu.Unlock()
		return errors.New("tray icon already running")
	}
	state := &icon{options: options}
	current = state
	currentMu.Unlock()
	defer func() {
		currentMu.Lock()
		current = nil
		currentMu.Unlock()
	}()

	instance, _, _ := getModuleHandle.Call(0)
	className, err := syscall.UTF16PtrFromString("FieldDayCommanderRelayTray")
	if err != nil {
		return err
	}
	class := windowClass{
		WndProc:   syscall.NewCallback(windowProc),
		Instance:  instance,
		ClassName: className,
	}
	class.Size = uint32(unsafe.Sizeof(class))
	if atom, _, callErr := registerClassEx.Call(uintptr(unsafe.Pointer(&class))); atom == 0 {
		return fmt.Errorf("register tray window: %w", callErr)
	}
	defer func() { _, _, _ = unregisterClass.Call(uintptr(unsafe.Pointer(className)), instance) }()

	state.window, _, err = createWindowEx.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(className)), 0, 0, 0, 0, 0, 0, 0, instance, 0)
	if state.window == 0 {
		return fmt.Errorf("create tray window: %w", err)
	}

	// Explorer sends this after it restarts, when every tray icon has to be re-added.
	if name, err := syscall.UTF16PtrFromString("TaskbarCreated"); err == nil {
		state.restarted, _, _ = registerWindowMessage.Call(uintptr(unsafe.Pointer(name)))
	}

	state.data = notifyIconData{
		Window:          state.window,
		ID:              1,
		Flags:           nifMessage | nifIcon | nifTip,
		CallbackMessage: wmTrayMessage,
		Icon:            loadTrayIcon(instance, options.IconResource),
	}
	state.data.Size = uint32(unsafe.Sizeof(state.data))
	if tip, err := syscall.UTF16FromString(options.Tooltip); err == nil {
		copy(state.data.Tip[:len(state.data.Tip)-1], tip)
	}
	if err := state.add(); err != nil {
		_, _, _ = destroyWindow.Call(state.window)
		return err
	}

	stop := context.AfterFunc(ctx, func() {
		_, _, _ = postMessage.Call(state.window, wmClose, 0, 0)
	})
	defer stop()

	var incoming message
	for {
		result, _, callErr := getMessage.Call(uintptr(unsafe.Pointer(&incoming)), 0, 0, 0)
		switch int32(result) {
		case -1:
			return fmt.Errorf("tray message loop: %w", callErr)
		case 0:
			return nil
		}
		_, _, _ = translateMessage.Call(uintptr(unsafe.Pointer(&incoming)))
		_, _, _ = dispatchMessage.Call(uintptr(unsafe.Pointer(&incoming)))
	}
}

// loadTrayIcon returns the program's own icon at the size the tray uses on this
// display. Windows picks the closest of the sizes built into the program. A
// program without that resource, like a test binary, gets the generic icon.
func loadTrayIcon(instance, resource uintptr) uintptr {
	if resource != 0 {
		width, _, _ := getSystemMetrics.Call(smCXSmallIcon)
		height, _, _ := getSystemMetrics.Call(smCYSmallIcon)
		if loaded, _, _ := loadImage.Call(instance, resource, imageIcon, width, height, 0); loaded != 0 {
			return loaded
		}
	}
	generic, _, _ := loadIcon.Call(0, idiApplication)
	return generic
}

func (i *icon) add() error {
	if result, _, callErr := shellNotifyIcon.Call(nimAdd, uintptr(unsafe.Pointer(&i.data))); result == 0 {
		return fmt.Errorf("add tray icon: %w", callErr)
	}
	return nil
}

func windowProc(window uintptr, kind uint32, wParam, lParam uintptr) uintptr {
	currentMu.Lock()
	state := current
	currentMu.Unlock()

	if state != nil {
		switch {
		case kind == wmTrayMessage:
			switch uint32(lParam) & 0xFFFF {
			case wmLButtonUp:
				state.open()
			case wmRButtonUp:
				state.showMenu(window)
			}
			return 0
		case kind == wmClose:
			_, _, _ = destroyWindow.Call(window)
			return 0
		case kind == wmDestroy:
			_, _, _ = shellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&state.data)))
			_, _, _ = postQuitMessage.Call(0)
			return 0
		case state.restarted != 0 && uintptr(kind) == state.restarted:
			_ = state.add()
			return 0
		}
	}
	result, _, _ := defWindowProc.Call(window, uintptr(kind), wParam, lParam)
	return result
}

func (i *icon) open() {
	if i.options.OnOpen != nil {
		go i.options.OnOpen()
	}
}

func (i *icon) showMenu(window uintptr) {
	menu, _, _ := createPopupMenu.Call()
	if menu == 0 {
		return
	}
	defer func() { _, _, _ = destroyMenu.Call(menu) }()

	for _, item := range []struct {
		id    uintptr
		label string
	}{{menuOpen, i.options.OpenLabel}, {menuQuit, i.options.QuitLabel}} {
		label, err := syscall.UTF16PtrFromString(item.label)
		if err != nil {
			return
		}
		_, _, _ = appendMenu.Call(menu, mfString, item.id, uintptr(unsafe.Pointer(label)))
	}

	var at point
	_, _, _ = getCursorPos.Call(uintptr(unsafe.Pointer(&at)))
	// Without this the menu stays open after a click elsewhere.
	_, _, _ = setForegroundWindow.Call(window)
	chosen, _, _ := trackPopupMenu.Call(menu, tpmReturnCmd|tpmRightButton, uintptr(at.X), uintptr(at.Y), 0, window, 0)
	_, _, _ = postMessage.Call(window, wmNull, 0, 0)

	switch chosen {
	case menuOpen:
		i.open()
	case menuQuit:
		if i.options.OnQuit != nil {
			go i.options.OnQuit()
		}
	}
}
