// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package logfile

import (
	"io"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"
)

// A logger that holds its file open and refuses other writers, the strictest way
// one can reasonably work, must still be seen growing.
func TestALogHeldOpenByALoggerCanBeWatched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "held.adi")
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	logger, err := syscall.CreateFile(name, syscall.GENERIC_WRITE, syscall.FILE_SHARE_READ, nil, syscall.CREATE_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(logger)

	text := []byte("<CALL:5>K1ABC<EOR>")
	if _, err := syscall.Write(logger, text); err != nil {
		t.Fatal(err)
	}
	info, err := statFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(text)) {
		t.Errorf("size %d, want %d", info.Size(), len(text))
	}

	file, err := openShared(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	got, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(text) {
		t.Errorf("read %q", got)
	}
}

// A logger that saves by replacing the file must not fail because the relay is
// reading it.
func TestALogCanBeReplacedWhileItIsBeingRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.adi")
	writeFile(t, path, "<CALL:5>K1ABC<EOR>")
	next := filepath.Join(dir, "next.adi")
	writeFile(t, next, "<CALL:5>W2XYZ<EOR>")

	file, err := openShared(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	from, _ := syscall.UTF16PtrFromString(next)
	to, _ := syscall.UTF16PtrFromString(path)
	const moveReplaceExisting = 1
	kernel := syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")
	if result, _, callErr := kernel.Call(uintptr(unsafe.Pointer(from)), uintptr(unsafe.Pointer(to)), moveReplaceExisting); result == 0 {
		t.Errorf("replace failed: %v", callErr)
	}
}
