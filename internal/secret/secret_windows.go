// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package secret

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

const (
	credTypeGeneric        = 1
	credPersistLocal       = 2
	errorNotFound          = syscall.Errno(1168)
	maxCredentialBlobBytes = 5 * 512
)

var (
	advapi32   = syscall.NewLazyDLL("advapi32.dll")
	credWrite  = advapi32.NewProc("CredWriteW")
	credRead   = advapi32.NewProc("CredReadW")
	credDelete = advapi32.NewProc("CredDeleteW")
	credFree   = advapi32.NewProc("CredFree")
)

// credential matches the Windows CREDENTIALW structure.
type credential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        syscall.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

// New returns a store backed by Windows Credential Manager. Windows protects the
// secret with the signed-in user's account.
func New(target string) Store { return &credentialManager{target: target} }

type credentialManager struct {
	target string
}

func (c *credentialManager) Get() (string, error) {
	target, err := syscall.UTF16PtrFromString(c.target)
	if err != nil {
		return "", err
	}

	var found *credential
	result, _, callErr := credRead.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&found)))
	if result == 0 {
		if errors.Is(callErr, errorNotFound) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("read from Credential Manager: %w", callErr)
	}
	defer func() { _, _, _ = credFree.Call(uintptr(unsafe.Pointer(found))) }()

	if found.CredentialBlobSize == 0 {
		return "", ErrNotFound
	}
	return string(unsafe.Slice(found.CredentialBlob, found.CredentialBlobSize)), nil
}

func (c *credentialManager) Set(value string) error {
	if value == "" {
		return errors.New("refusing to store an empty secret")
	}
	if len(value) > maxCredentialBlobBytes {
		return errors.New("secret is too long for Credential Manager")
	}
	target, err := syscall.UTF16PtrFromString(c.target)
	if err != nil {
		return err
	}

	blob := []byte(value)
	entry := credential{
		Type:               credTypeGeneric,
		TargetName:         target,
		CredentialBlobSize: uint32(len(blob)),
		CredentialBlob:     &blob[0],
		Persist:            credPersistLocal,
	}
	result, _, callErr := credWrite.Call(uintptr(unsafe.Pointer(&entry)), 0)
	if result == 0 {
		return fmt.Errorf("write to Credential Manager: %w", callErr)
	}
	return nil
}

func (c *credentialManager) Delete() error {
	target, err := syscall.UTF16PtrFromString(c.target)
	if err != nil {
		return err
	}
	result, _, callErr := credDelete.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0)
	if result == 0 && !errors.Is(callErr, errorNotFound) {
		return fmt.Errorf("delete from Credential Manager: %w", callErr)
	}
	return nil
}
