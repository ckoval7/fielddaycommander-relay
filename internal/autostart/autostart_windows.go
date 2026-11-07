// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package autostart

import (
	"errors"
	"fmt"
	"syscall"
	"unicode/utf16"
	"unsafe"
)

var (
	advapi32       = syscall.NewLazyDLL("advapi32.dll")
	regCreateKeyEx = advapi32.NewProc("RegCreateKeyExW")
	regSetValueEx  = advapi32.NewProc("RegSetValueExW")
	regDeleteValue = advapi32.NewProc("RegDeleteValueW")
)

// New returns the entry for program, started with dataDir if it isn't empty.
func New(program, dataDir string) Starter {
	return starter{store: registry{name: EntryName}, command: CommandLine(program, dataDir)}
}

// registry keeps the entry in the current person's part of the Windows registry.
type registry struct{ name string }

func (r registry) entry() (string, bool, error) {
	data, found, err := readValue(RunKey, r.name)
	if err != nil || !found {
		return "", false, err
	}
	return decodeString(data), true, nil
}

func (r registry) setEntry(command string) error {
	text, err := syscall.UTF16FromString(command)
	if err != nil {
		return fmt.Errorf("save the startup entry: %w", err)
	}
	return writeString(RunKey, r.name, text)
}

func (r registry) removeEntry() error { return deleteValue(RunKey, r.name) }

func (r registry) turnedOff() (bool, error) {
	data, found, err := readValue(SwitchKey, r.name)
	if err != nil || !found || len(data) == 0 {
		return false, err
	}
	return data[0]&1 == 1, nil
}

func (r registry) clearSwitch() error { return deleteValue(SwitchKey, r.name) }

// decodeString reads a saved string, which may or may not end in a NUL.
func decodeString(data []byte) string {
	units := make([]uint16, 0, len(data)/2)
	for i := 0; i+1 < len(data); i += 2 {
		unit := uint16(data[i]) | uint16(data[i+1])<<8
		if unit == 0 {
			break
		}
		units = append(units, unit)
	}
	return string(utf16.Decode(units))
}

func openKey(path string, access uint32) (syscall.Handle, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var key syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, name, 0, access, &key); err != nil {
		return 0, err
	}
	return key, nil
}

func readValue(path, value string) ([]byte, bool, error) {
	key, err := openKey(path, syscall.KEY_QUERY_VALUE)
	if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read the startup entry: %w", err)
	}
	defer func() { _ = syscall.RegCloseKey(key) }()

	name, err := syscall.UTF16PtrFromString(value)
	if err != nil {
		return nil, false, fmt.Errorf("read the startup entry: %w", err)
	}
	var size uint32
	err = syscall.RegQueryValueEx(key, name, nil, nil, nil, &size)
	if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read the startup entry: %w", err)
	}
	if size == 0 {
		return nil, true, nil
	}
	data := make([]byte, size)
	if err := syscall.RegQueryValueEx(key, name, nil, nil, &data[0], &size); err != nil {
		return nil, false, fmt.Errorf("read the startup entry: %w", err)
	}
	return data[:size], true, nil
}

func writeString(path, value string, text []uint16) error {
	pathName, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("save the startup entry: %w", err)
	}
	name, err := syscall.UTF16PtrFromString(value)
	if err != nil {
		return fmt.Errorf("save the startup entry: %w", err)
	}
	var key syscall.Handle
	if result, _, _ := regCreateKeyEx.Call(uintptr(syscall.HKEY_CURRENT_USER), uintptr(unsafe.Pointer(pathName)), 0, 0, 0,
		syscall.KEY_SET_VALUE, 0, uintptr(unsafe.Pointer(&key)), 0); result != 0 {
		return fmt.Errorf("save the startup entry: %w", syscall.Errno(result))
	}
	defer func() { _ = syscall.RegCloseKey(key) }()

	result, _, _ := regSetValueEx.Call(uintptr(key), uintptr(unsafe.Pointer(name)), 0, syscall.REG_SZ,
		uintptr(unsafe.Pointer(&text[0])), uintptr(len(text)*2))
	if result != 0 {
		return fmt.Errorf("save the startup entry: %w", syscall.Errno(result))
	}
	return nil
}

// deleteValue removes a value. Removing one that isn't there isn't an error.
func deleteValue(path, value string) error {
	key, err := openKey(path, syscall.KEY_SET_VALUE)
	if errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove the startup entry: %w", err)
	}
	defer func() { _ = syscall.RegCloseKey(key) }()

	name, err := syscall.UTF16PtrFromString(value)
	if err != nil {
		return fmt.Errorf("remove the startup entry: %w", err)
	}
	result, _, _ := regDeleteValue.Call(uintptr(key), uintptr(unsafe.Pointer(name)))
	if errno := syscall.Errno(result); result != 0 && !errors.Is(errno, syscall.ERROR_FILE_NOT_FOUND) {
		return fmt.Errorf("remove the startup entry: %w", errno)
	}
	return nil
}
