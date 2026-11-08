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
	// backupSemantics lets CreateFile open a folder, so statFile can say it is one.
	backupSemantics = 0x02000000
	shareEverything = syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE | syscall.FILE_SHARE_DELETE
)

// openShared opens a log for reading without getting in the logger's way. Unlike
// os.Open, it also lets the logger rename or delete the file while it's open.
// Nothing can let a file open on Windows be replaced by name, so keep reads short.
func openShared(path string) (*os.File, error) {
	return open(path, syscall.GENERIC_READ, 0)
}

// statFile asks the open file for its size, because Windows can hold back the
// size it reports by name for as long as a logger keeps the file open. Asking
// for attributes alone can't conflict with any way a logger opened the file.
func statFile(path string) (fs.FileInfo, error) {
	file, err := open(path, fileReadAttributes, backupSemantics)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return file.Stat()
}

func open(path string, access, flags uint32) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	handle, err := syscall.CreateFile(name, access, shareEverything, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL|flags, 0)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}
