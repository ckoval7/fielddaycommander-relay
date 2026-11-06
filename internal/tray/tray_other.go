// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package tray

import "context"

// Run has no icon to show outside Windows, and waits until ctx is canceled.
func Run(ctx context.Context, _ Options) error {
	<-ctx.Done()
	return nil
}
