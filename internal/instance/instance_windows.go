// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package instance

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

var createMutex = syscall.NewLazyDLL("kernel32.dll").NewProc("CreateMutexW")

func acquire(name string) (func(), error) {
	text, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, err
	}
	handle, _, callErr := createMutex.Call(0, 0, uintptr(unsafe.Pointer(text)))
	// Another person's relay on this computer shows up as access denied.
	if errors.Is(callErr, syscall.ERROR_ALREADY_EXISTS) || errors.Is(callErr, syscall.ERROR_ACCESS_DENIED) {
		if handle != 0 {
			_ = syscall.CloseHandle(syscall.Handle(handle))
		}
		return nil, ErrHeld
	}
	if handle == 0 {
		return nil, fmt.Errorf("check for another relay: %w", callErr)
	}
	return func() { _ = syscall.CloseHandle(syscall.Handle(handle)) }, nil
}
