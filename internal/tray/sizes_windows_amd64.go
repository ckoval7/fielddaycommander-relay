// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package tray

import "unsafe"

// Windows rejects a structure of the wrong size, so a mistake here would mean no
// icon. These stop the build instead.
var (
	_ [976]byte = [unsafe.Sizeof(notifyIconData{})]byte{}
	_ [80]byte  = [unsafe.Sizeof(windowClass{})]byte{}
	_ [48]byte  = [unsafe.Sizeof(message{})]byte{}
)
