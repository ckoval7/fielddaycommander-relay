// SPDX-FileCopyrightText: 2026 Corey Koval <ckoval7@gmail.com>
// SPDX-License-Identifier: GPL-3.0-or-later

package filepicker

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// Positions of the methods in each COM interface's table.
const (
	methodRelease = 2

	dialogShow             = 3
	dialogSetFileTypes     = 4
	dialogSetOptions       = 9
	dialogGetOptions       = 10
	dialogSetDefaultFolder = 11
	dialogSetTitle         = 17
	dialogGetResult        = 20

	itemGetDisplayName = 5
)

const (
	coinitApartmentThreaded = 0x2
	coinitDisableOLE1DDE    = 0x4
	clsctxInprocServer      = 0x1

	fosNoChangeDir      = 0x8
	fosForceFileSystem  = 0x40
	fosPathMustExist    = 0x800
	fosFileMustExist    = 0x1000
	sigdnFileSystemPath = 0x80058000
	errorCancelled      = 0x800704C7

	wsPopup        = 0x80000000
	wsExTopmost    = 0x8
	wsExToolWindow = 0x80
	swShowNA       = 8
	smCxScreen     = 0
	smCyScreen     = 1
)

var (
	ole32     = syscall.NewLazyDLL("ole32.dll")
	shell32   = syscall.NewLazyDLL("shell32.dll")
	user32    = syscall.NewLazyDLL("user32.dll")
	initCOM   = ole32.NewProc("CoInitializeEx")
	closeCOM  = ole32.NewProc("CoUninitialize")
	createCOM = ole32.NewProc("CoCreateInstance")
	freeCOM   = ole32.NewProc("CoTaskMemFree")

	shellItemFromPath = shell32.NewProc("SHCreateItemFromParsingName")

	createWindow     = user32.NewProc("CreateWindowExW")
	destroyWindow    = user32.NewProc("DestroyWindow")
	showWindow       = user32.NewProc("ShowWindow")
	setForeground    = user32.NewProc("SetForegroundWindow")
	getSystemMetrics = user32.NewProc("GetSystemMetrics")

	classFileOpenDialog = syscall.GUID{Data1: 0xDC1C5A9C, Data2: 0xE88A, Data3: 0x4DDE, Data4: [8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7}}
	interfaceFileOpen   = syscall.GUID{Data1: 0xD57C7288, Data2: 0xD4AD, Data3: 0x4768, Data4: [8]byte{0xBE, 0x02, 0x9D, 0x96, 0x95, 0x32, 0xD9, 0x60}}
	interfaceShellItem  = syscall.GUID{Data1: 0x43826D1E, Data2: 0xE718, Data3: 0x42EE, Data4: [8]byte{0xBC, 0x55, 0xA1, 0xE2, 0x61, 0xC3, 0x7B, 0xFE}}
)

// filterSpec matches COMDLG_FILTERSPEC.
type filterSpec struct {
	name    *uint16
	pattern *uint16
}

// Open shows the window until the person chooses a file or closes it, which
// returns "". It blocks, so call it from its own goroutine.
func Open(options Options) (string, error) {
	// COM and the window both belong to the thread that made them.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	result, _, _ := syscall.SyscallN(initCOM.Addr(), 0, coinitApartmentThreaded|coinitDisableOLE1DDE)
	if failed(result) {
		return "", hresultError("start COM", result)
	}
	defer func() { _, _, _ = syscall.SyscallN(closeCOM.Addr()) }()

	var dialog unsafe.Pointer
	result, _, _ = syscall.SyscallN(createCOM.Addr(),
		uintptr(unsafe.Pointer(&classFileOpenDialog)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&interfaceFileOpen)), uintptr(unsafe.Pointer(&dialog)))
	if failed(result) {
		return "", hresultError("create the file window", result)
	}
	defer release(dialog)

	if err := configure(dialog, options); err != nil {
		return "", err
	}

	owner, err := newOwner(options.Title)
	if err != nil {
		return "", err
	}
	defer func() { _, _, _ = syscall.SyscallN(destroyWindow.Addr(), owner) }()

	result, _, _ = syscall.SyscallN(method(dialog, dialogShow), uintptr(dialog), owner)
	if uint32(result) == errorCancelled {
		return "", nil
	}
	if failed(result) {
		return "", hresultError("show the file window", result)
	}

	var item unsafe.Pointer
	result, _, _ = syscall.SyscallN(method(dialog, dialogGetResult), uintptr(dialog), uintptr(unsafe.Pointer(&item)))
	if failed(result) {
		return "", hresultError("read the chosen file", result)
	}
	defer release(item)

	var name *uint16
	result, _, _ = syscall.SyscallN(method(item, itemGetDisplayName), uintptr(item), sigdnFileSystemPath, uintptr(unsafe.Pointer(&name)))
	if failed(result) {
		return "", hresultError("read the chosen file's location", result)
	}
	defer func() { _, _, _ = syscall.SyscallN(freeCOM.Addr(), uintptr(unsafe.Pointer(name))) }()
	return textFrom(name), nil
}

// textFrom reads the zero-terminated text Windows returned.
func textFrom(text *uint16) string {
	length := 0
	for *(*uint16)(unsafe.Add(unsafe.Pointer(text), uintptr(length)*unsafe.Sizeof(*text))) != 0 {
		length++
	}
	return syscall.UTF16ToString(unsafe.Slice(text, length))
}

func configure(dialog unsafe.Pointer, options Options) error {
	var flags uint32
	result, _, _ := syscall.SyscallN(method(dialog, dialogGetOptions), uintptr(dialog), uintptr(unsafe.Pointer(&flags)))
	if failed(result) {
		return hresultError("read the window's options", result)
	}
	// Not changing the working folder matters: the relay is still running afterward.
	flags |= fosNoChangeDir | fosForceFileSystem | fosPathMustExist | fosFileMustExist
	result, _, _ = syscall.SyscallN(method(dialog, dialogSetOptions), uintptr(dialog), uintptr(flags))
	if failed(result) {
		return hresultError("set the window's options", result)
	}

	title, err := syscall.UTF16PtrFromString(options.Title)
	if err != nil {
		return err
	}
	result, _, _ = syscall.SyscallN(method(dialog, dialogSetTitle), uintptr(dialog), uintptr(unsafe.Pointer(title)))
	if failed(result) {
		return hresultError("set the window's title", result)
	}

	filters, err := fileTypes(options)
	if err != nil {
		return err
	}
	result, _, _ = syscall.SyscallN(method(dialog, dialogSetFileTypes), uintptr(dialog), uintptr(len(filters)), uintptr(unsafe.Pointer(&filters[0])))
	if failed(result) {
		return hresultError("set the window's file types", result)
	}

	// Only a convenience, so a folder it can't use is no reason to fail.
	for _, folder := range options.DefaultFolders {
		if info, err := os.Stat(folder); err != nil || !info.IsDir() {
			continue
		}
		setDefaultFolder(dialog, folder)
		break
	}
	return nil
}

func fileTypes(options Options) ([]filterSpec, error) {
	strings := []string{options.FilterName, options.Patterns, options.AllFilesName, "*.*"}
	pointers := make([]*uint16, len(strings))
	for i, value := range strings {
		pointer, err := syscall.UTF16PtrFromString(value)
		if err != nil {
			return nil, err
		}
		pointers[i] = pointer
	}
	return []filterSpec{{pointers[0], pointers[1]}, {pointers[2], pointers[3]}}, nil
}

func setDefaultFolder(dialog unsafe.Pointer, folder string) {
	path, err := syscall.UTF16PtrFromString(folder)
	if err != nil {
		return
	}
	var item unsafe.Pointer
	result, _, _ := syscall.SyscallN(shellItemFromPath.Addr(), uintptr(unsafe.Pointer(path)), 0,
		uintptr(unsafe.Pointer(&interfaceShellItem)), uintptr(unsafe.Pointer(&item)))
	if failed(result) {
		return
	}
	defer release(item)
	_, _, _ = syscall.SyscallN(method(dialog, dialogSetDefaultFolder), uintptr(dialog), uintptr(item))
}

// newOwner makes a small window for the file window to belong to. Without an
// always-on-top owner, the file window can open behind the browser, because the
// person clicked in the browser and not in the relay. It sits mid-screen because
// the file window opens near its owner.
func newOwner(title string) (uintptr, error) {
	class, err := syscall.UTF16PtrFromString("STATIC")
	if err != nil {
		return 0, err
	}
	name, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return 0, err
	}
	width, _, _ := syscall.SyscallN(getSystemMetrics.Addr(), smCxScreen)
	height, _, _ := syscall.SyscallN(getSystemMetrics.Addr(), smCyScreen)

	window, _, callErr := syscall.SyscallN(createWindow.Addr(),
		wsExTopmost|wsExToolWindow, uintptr(unsafe.Pointer(class)), uintptr(unsafe.Pointer(name)), wsPopup,
		width/4, height/5, 0, 0, 0, 0, 0, 0)
	if window == 0 {
		return 0, fmt.Errorf("create the file window's owner: %w", callErr)
	}
	_, _, _ = syscall.SyscallN(showWindow.Addr(), window, swShowNA)
	_, _, _ = syscall.SyscallN(setForeground.Addr(), window)
	return window, nil
}

// method finds a COM method in the table at the start of the object.
func method(object unsafe.Pointer, index uintptr) uintptr {
	table := *(*unsafe.Pointer)(object)
	return *(*uintptr)(unsafe.Add(table, index*unsafe.Sizeof(uintptr(0))))
}

func release(object unsafe.Pointer) {
	_, _, _ = syscall.SyscallN(method(object, methodRelease), uintptr(object))
}

// failed reads an HRESULT, where the top bit marks failure. CoInitializeEx answers
// S_FALSE when COM is already started on this thread, which is fine.
func failed(result uintptr) bool {
	return uint32(result)&0x80000000 != 0
}

func hresultError(what string, result uintptr) error {
	return fmt.Errorf("%s: 0x%08X", what, uint32(result))
}
