// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !windows

package secret

// New returns a store that refuses to hold anything. Windows is the only
// platform with a keychain so far, and the token never falls back to a file.
func New(string) Store { return unavailable{} }

type unavailable struct{}

func (unavailable) Get() (string, error) { return "", ErrNotFound }
func (unavailable) Set(string) error     { return ErrUnavailable }
func (unavailable) Delete() error        { return nil }
