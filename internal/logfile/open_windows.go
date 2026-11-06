// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package logfile

import (
	"io/fs"
	"os"
	"syscall"
)

const (
	fileReadAttributes = 0x80
	shareEverything    = syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE | syscall.FILE_SHARE_DELETE
)

// openShared opens a log for reading without getting in the logger's way. Unlike
// os.Open, it also lets the logger replace or delete the file while it's open.
func openShared(path string) (*os.File, error) {
	return open(path, syscall.GENERIC_READ)
}

// statFile asks the open file for its size, because Windows can hold back the
// size it reports by name for as long as a logger keeps the file open. Asking
// for attributes alone can't conflict with any way a logger opened the file.
func statFile(path string) (fs.FileInfo, error) {
	file, err := open(path, fileReadAttributes)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return file.Stat()
}

func open(path string, access uint32) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	handle, err := syscall.CreateFile(name, access, shareEverything, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}
