// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

// Package settings stores what the relay needs to remember that isn't secret.
// Nobody edits this file by hand, and the token never goes in it.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// FileName is the settings file inside the data folder.
const FileName = "settings.json"

// Settings is the relay's saved configuration.
type Settings struct {
	// ServerURL is empty until the relay enrolls.
	ServerURL string `json:"server_url,omitempty"`
	// PinSHA256 is the hex SHA-256 of a self-signed server's public key.
	PinSHA256 string `json:"pin_sha256,omitempty"`
	RelayName string `json:"relay_name,omitempty"`
	// LoggerAddress is the N3FJP network server's host and port. Empty means
	// the default, this computer.
	LoggerAddress string `json:"logger_address,omitempty"`
}

// Load reads the settings in dir. Missing settings are empty ones, which is how
// a new install starts.
func Load(dir string) (Settings, error) {
	var loaded Settings
	data, err := os.ReadFile(filepath.Join(dir, FileName))
	if errors.Is(err, fs.ErrNotExist) {
		return loaded, nil
	}
	if err != nil {
		return loaded, fmt.Errorf("read settings: %w", err)
	}
	if err := json.Unmarshal(data, &loaded); err != nil {
		return loaded, fmt.Errorf("read settings: %w", err)
	}
	return loaded, nil
}

// Save writes the settings to dir so that a crash or power cut leaves either the
// old file or the new one, never half of one.
func Save(dir string, current Settings) error {
	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return fmt.Errorf("save settings: %w", err)
	}

	temp, err := os.CreateTemp(dir, FileName+".*")
	if err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	defer os.Remove(temp.Name())

	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return fmt.Errorf("save settings: %w", err)
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return fmt.Errorf("save settings: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	if err := os.Rename(temp.Name(), filepath.Join(dir, FileName)); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	return nil
}
