// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package logfile

import (
	"io/fs"
	"os"
)

func openShared(path string) (*os.File, error) { return os.Open(path) }

func statFile(path string) (fs.FileInfo, error) { return os.Stat(path) }
