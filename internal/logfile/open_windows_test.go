// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package logfile

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
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

// While the relay reads a log, its logger must still be able to write to it, rename
// it and delete it. Windows won't let anyone replace a file by name while it's open,
// so a logger that saves that way waits for the read to end, which takes as long as
// reading one window.
func TestALoggerCanChangeItsLogWhileItIsBeingRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "log.adi")
	writeFile(t, path, "<CALL:5>K1ABC<EOR>")

	file, err := openShared(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	logger, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("the logger can't open its log: %v", err)
	}
	_, err = logger.WriteString("<CALL:5>W2XYZ<EOR>")
	logger.Close()
	if err != nil {
		t.Errorf("the logger can't write: %v", err)
	}

	moved := filepath.Join(dir, "log.bak")
	if err := os.Rename(path, moved); err != nil {
		t.Errorf("the logger can't rename its log: %v", err)
	}
	if err := os.Remove(moved); err != nil {
		t.Errorf("the logger can't delete its log: %v", err)
	}
}
