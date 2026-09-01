//go:build windows

package updatercore

import (
	"fmt"
	"syscall"
	"unsafe"
)

const (
	gmemMoveable   = 0x0002
	cfUnicodeText  = 13
	shellExecuteOK = 32
	showNormal     = 1
)

var (
	procShellExecuteW    = syscall.NewLazyDLL("shell32.dll").NewProc("ShellExecuteW")
	procOpenClipboard    = user32.NewProc("OpenClipboard")
	procCloseClipboard   = user32.NewProc("CloseClipboard")
	procEmptyClipboard   = user32.NewProc("EmptyClipboard")
	procSetClipboardData = user32.NewProc("SetClipboardData")
	procGlobalAlloc      = kernel32.NewProc("GlobalAlloc")
	procGlobalLock       = kernel32.NewProc("GlobalLock")
	procGlobalUnlock     = kernel32.NewProc("GlobalUnlock")
	procGlobalFree       = kernel32.NewProc("GlobalFree")
)

func win32CallError(operation string, callErr error) error {
	if callErr != nil && callErr != syscall.Errno(0) {
		return fmt.Errorf("%s: %w", operation, callErr)
	}
	return fmt.Errorf("%s failed", operation)
}

func openFallbackURL(url string) error {
	operation, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		return fmt.Errorf("encode browser operation: %w", err)
	}
	target, err := syscall.UTF16PtrFromString(url)
	if err != nil {
		return fmt.Errorf("encode browser URL: %w", err)
	}
	ret, _, callErr := procShellExecuteW.Call(
		0,
		uintptr(unsafe.Pointer(operation)),
		uintptr(unsafe.Pointer(target)),
		0,
		0,
		showNormal,
	)
	if ret <= shellExecuteOK {
		return win32CallError("open browser", callErr)
	}
	return nil
}

func copyFallbackText(text string) error {
	utf16, err := syscall.UTF16FromString(text)
	if err != nil {
		return fmt.Errorf("encode clipboard text: %w", err)
	}
	opened, _, callErr := procOpenClipboard.Call(0)
	if opened == 0 {
		return win32CallError("open clipboard", callErr)
	}
	defer procCloseClipboard.Call()

	if emptied, _, callErr := procEmptyClipboard.Call(); emptied == 0 {
		return win32CallError("empty clipboard", callErr)
	}

	memory, _, callErr := procGlobalAlloc.Call(gmemMoveable, uintptr(len(utf16))*2)
	if memory == 0 {
		return win32CallError("allocate clipboard memory", callErr)
	}
	locked, _, callErr := procGlobalLock.Call(memory)
	if locked == 0 {
		procGlobalFree.Call(memory)
		return win32CallError("lock clipboard memory", callErr)
	}
	copy(unsafe.Slice((*uint16)(unsafe.Pointer(locked)), len(utf16)), utf16)
	procGlobalUnlock.Call(memory)

	data, _, _ := procSetClipboardData.Call(uintptr(cfUnicodeText), memory)
	if data == 0 {
		procGlobalFree.Call(memory)
		return fmt.Errorf("set clipboard data failed")
	}
	// Ownership of memory transfers to the clipboard after SetClipboardData.
	return nil
}
