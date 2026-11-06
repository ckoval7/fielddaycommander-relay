// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package filepicker

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// The window appears on top of everything, with its title, and closing it counts
// as choosing nothing.
func TestTheWindowOpensOnTopAndClosingItChoosesNothing(t *testing.T) {
	title := fmt.Sprintf("Field Day Commander Relay test %d", os.Getpid())
	titlePointer, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		t.Fatal(err)
	}

	type answer struct {
		path string
		err  error
	}
	done := make(chan answer, 1)
	go func() {
		path, err := Open(Options{Title: title, FilterName: "ADIF logs", Patterns: "*.adi;*.adif", AllFilesName: "All files", DefaultFolders: []string{`C:\no\such\folder`, os.TempDir()}})
		done <- answer{path, err}
	}()

	findWindow := user32.NewProc("FindWindowW")
	getLong := user32.NewProc("GetWindowLongW")
	postMessage := user32.NewProc("PostMessageW")
	dialogClass, _ := syscall.UTF16PtrFromString("#32770")
	const (
		gwlExStyle = ^uintptr(19) // -20
		wmClose    = 0x10
	)

	var window uintptr
	deadline := time.Now().Add(15 * time.Second)
	for window == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the window never appeared")
		}
		select {
		case result := <-done:
			t.Fatalf("Open returned early: %+v", result)
		case <-time.After(50 * time.Millisecond):
		}
		window, _, _ = syscall.SyscallN(findWindow.Addr(), uintptr(unsafe.Pointer(dialogClass)), uintptr(unsafe.Pointer(titlePointer)))
	}

	style, _, _ := syscall.SyscallN(getLong.Addr(), window, gwlExStyle)
	if style&wsExTopmost == 0 {
		t.Errorf("the window can open behind others: style %#x", style)
	}

	_, _, _ = syscall.SyscallN(postMessage.Addr(), window, wmClose, 0, 0)
	select {
	case result := <-done:
		if result.err != nil || result.path != "" {
			t.Errorf("got %+v", result)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("closing the window didn't end Open")
	}
}
