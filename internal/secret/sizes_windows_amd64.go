// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package secret

import "unsafe"

// Windows would misread a structure of the wrong size. This stops the build
// instead.
var _ [80]byte = [unsafe.Sizeof(credential{})]byte{}
