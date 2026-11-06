// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package filepicker shows the system's own window for choosing a file. A web page
// can't do this for the relay, because browsers keep a chosen file's location from
// the page, and the relay needs it to keep watching the file.
package filepicker

import "errors"

// ErrUnavailable means this platform has no file window yet.
var ErrUnavailable = errors.New("no file window on this platform")

// Options describes the window.
type Options struct {
	Title string
	// FilterName and Patterns ("*.adi;*.adif") are the file type offered first.
	FilterName string
	Patterns   string
	// AllFilesName names the second file type, which shows everything.
	AllFilesName string
	// DefaultFolders are tried in order for where the window starts, the first time
	// only. After that, the system remembers where the person last looked.
	DefaultFolders []string
}
