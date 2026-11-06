// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package secret keeps the relay's token in the operating system's keychain.
package secret

import "errors"

// ErrNotFound means the keychain holds no token.
var ErrNotFound = errors.New("secret not found")

// ErrUnavailable means this system has no keychain the relay can use.
var ErrUnavailable = errors.New("no keychain available on this system")

// Store holds one secret.
type Store interface {
	// Get returns the secret, or ErrNotFound.
	Get() (string, error)
	Set(value string) error
	// Delete removes the secret. Deleting nothing isn't an error.
	Delete() error
}

// DefaultTarget names the relay's token in the keychain.
const DefaultTarget = "Field Day Commander Relay/token"
